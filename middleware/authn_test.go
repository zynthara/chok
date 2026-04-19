package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/auth/jwt"
	"github.com/zynthara/chok/handler"
)

const testKey = "test-key-that-is-exactly-32-byte"

func fixedNow() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

func newTestJWT(t *testing.T) *jwt.Manager {
	t.Helper()
	m, err := jwt.NewManager(jwt.Options{
		SigningKey: testKey,
		Expiration: time.Hour,
		Now:        fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func authnRouter(parser TokenParser, resolver PrincipalResolver) *gin.Engine {
	r := gin.New()
	r.Use(Authn(parser, resolver))
	r.GET("/me", func(c *gin.Context) {
		p, ok := auth.PrincipalFrom(c.Request.Context())
		if !ok {
			handler.WriteResponse(c, 0, nil, errors.New("no principal"))
			return
		}
		c.JSON(200, gin.H{
			"subject": p.Subject,
			"name":    p.Name,
			"roles":   p.Roles,
		})
	})
	return r
}

func TestAuthn_ValidToken_NilResolver(t *testing.T) {
	m := newTestJWT(t)
	token, _, _ := m.Sign("usr_1", map[string]any{"role": "admin"})

	r := authnRouter(m, nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["subject"] != "usr_1" {
		t.Fatalf("subject = %v, want usr_1", resp["subject"])
	}
}

func TestAuthn_ValidToken_WithResolver(t *testing.T) {
	m := newTestJWT(t)
	token, _, _ := m.Sign("usr_2", map[string]any{"role": "editor"})

	resolver := func(_ context.Context, sub string, claims map[string]any) (auth.Principal, error) {
		return auth.Principal{
			Subject: sub,
			Name:    "Resolved User",
			Roles:   []string{claims["role"].(string)},
		}, nil
	}

	r := authnRouter(m, resolver)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["name"] != "Resolved User" {
		t.Fatalf("name = %v, want Resolved User", resp["name"])
	}
}

func TestAuthn_NoToken_401(t *testing.T) {
	m := newTestJWT(t)
	r := authnRouter(m, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/me", nil)
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthn_InvalidToken_401(t *testing.T) {
	m := newTestJWT(t)
	r := authnRouter(m, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/me", nil)
	req.Header.Set("Authorization", "Bearer garbage.token.here")
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthn_MalformedHeader_401(t *testing.T) {
	m := newTestJWT(t)
	r := authnRouter(m, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/me", nil)
	req.Header.Set("Authorization", "NotBearer xyz")
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthn_ResolverError_401(t *testing.T) {
	m := newTestJWT(t)
	token, _, _ := m.Sign("usr_3", nil)

	resolver := func(_ context.Context, _ string, _ map[string]any) (auth.Principal, error) {
		return auth.Principal{}, errors.New("user suspended")
	}

	r := authnRouter(m, resolver)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthn_BearerCaseInsensitive(t *testing.T) {
	m := newTestJWT(t)
	token, _, _ := m.Sign("usr_4", nil)

	r := authnRouter(m, nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/me", nil)
	req.Header.Set("Authorization", "bearer "+token) // lowercase
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200 for lowercase bearer, got %d", w.Code)
	}
}

// --- Custom TokenParser ---

type stubParser struct {
	sub    string
	claims map[string]any
	err    error
}

func (s *stubParser) Parse(_ string) (string, map[string]any, error) {
	return s.sub, s.claims, s.err
}

func TestAuthn_CustomTokenParser(t *testing.T) {
	parser := &stubParser{sub: "custom_sub", claims: map[string]any{"x": 1}}
	r := authnRouter(parser, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/me", nil)
	req.Header.Set("Authorization", "Bearer anything")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["subject"] != "custom_sub" {
		t.Fatalf("subject = %v, want custom_sub", resp["subject"])
	}
}

func TestAuthn_NilParser_Panics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for nil parser")
		}
	}()
	Authn(nil, nil) // should panic at construction, not at request time
}

// TestExtractBearer_RejectsOversizedHeader verifies the H2 fix: an
// Authorization header longer than maxAuthorizationLen is treated as
// invalid before reaching the JWT parser, blocking a single-core DoS
// against jwt.Parse / HMAC computation.
func TestExtractBearer_RejectsOversizedHeader(t *testing.T) {
	// 8 KB + 1 byte payload after the "Bearer " prefix.
	huge := "Bearer " + string(make([]byte, maxAuthorizationLen))
	if got := extractBearer(huge); got != "" {
		t.Fatalf("extractBearer should reject oversized header, got %d bytes", len(got))
	}

	// Exactly at the boundary still parses (note: the resulting token
	// is meaningless, but extraction itself must not refuse it).
	atLimit := "Bearer " + string(make([]byte, maxAuthorizationLen-len("Bearer ")))
	if got := extractBearer(atLimit); got == "" {
		t.Fatal("extractBearer should accept header at exactly the limit")
	}
}
