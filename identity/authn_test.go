package identity

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
	"github.com/zynthara/chok/v3/store"
)

// whoami answers the principal Authn stored.
func whoami(app *chok.App, idn *Identity) {
	chok.GET(app.Group("/api", idn.Authn()), "/whoami", func(ctx context.Context, _ *chok.None) (chok.Principal, error) {
		p, _ := chok.PrincipalFrom(ctx)
		return p, nil
	})
}

func TestAuthn_StoresThePrincipal(t *testing.T) {
	idn := newIdentity(t, Config{})
	c := serve(t, idn, func(app *chok.App) { whoami(app, idn) })
	tok := register(t, c, "alice@test.com", "password123")
	u := userByEmail(t, idn, "alice@test.com")
	var p chok.Principal
	if code := c.WithBearer(tok).GetJSON("/api/whoami", &p); code != http.StatusOK {
		t.Fatalf("whoami = %d", code)
	}
	if p.UserID != u.RID || !strings.HasPrefix(p.UserID, "usr_") || p.Email != "alice@test.com" || p.Extra["name"] != u.Name || len(p.Roles) != 0 {
		t.Fatalf("principal = %+v", p)
	}
	if err := idn.Users().SetRoles(t.Context(), u.RID, []string{"editor"}); err != nil {
		t.Fatal(err)
	}
	_, tok = login(t, c, "alice@test.com", "password123")
	c.WithBearer(tok).GetJSON("/api/whoami", &p)
	if !slices.Equal(p.Roles, []string{"editor"}) || !p.HasRole("editor") {
		t.Fatalf("the roles after SetRoles: %+v", p)
	}
}

func TestAuthn_MissingTokenIs401Envelope(t *testing.T) {
	idn := newIdentity(t, Config{})
	c := serve(t, idn, func(app *chok.App) { whoami(app, idn) })
	resp := c.Do(httptestRequest(http.MethodGet, "/api/whoami", nil))
	var e envelope
	decodeBody(t, resp, &e)
	if resp.StatusCode != http.StatusUnauthorized || e.Code != 401 || e.Reason != "Unauthorized" || e.Message != "a bearer token is required" {
		t.Fatalf("no token = %d %+v", resp.StatusCode, e)
	}
	if e.RequestID == "" || e.RequestID != resp.Header.Get("X-Request-Id") || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("the envelope carries the request id: %q header %q", e.RequestID, resp.Header.Get("X-Request-Id"))
	}
}

func TestAuthn_RefusesBadHeaders(t *testing.T) {
	idn := newIdentity(t, Config{})
	c := serve(t, idn, func(app *chok.App) { whoami(app, idn) })
	tok := register(t, c, "alice@test.com", "password123")
	u := userByEmail(t, idn, "alice@test.com")
	resetToken, _, _ := idn.reset.sign(u.RID, map[string]any{"purpose": "reset", "pv": u.PasswordVersion})
	expired := newTestTokens(t, issuerAccess, func() time.Time { return time.Now().Add(-3 * time.Hour) })
	expired.key = []byte(testKey)
	old, _, _ := expired.sign(u.RID, map[string]any{"pv": 0})
	for name, header := range map[string]string{
		"garbage":            "Bearer garbage.token.here",
		"another scheme":     "NotBearer " + tok,
		"no token":           "Bearer",
		"basic":              "Basic YWxpY2U6cGFzc3dvcmQxMjM=",
		"a reset token":      "Bearer " + resetToken,
		"an expired token":   "Bearer " + old,
		"token in the realm": "Bearer" + tok,
	} {
		var e envelope
		if code := c.GetJSON("/api/whoami", &e, "Authorization", header); code != http.StatusUnauthorized || e.Reason != "Unauthorized" {
			t.Errorf("%s = %d %+v", name, code, e)
		}
	}
}

func TestAuthn_BearerSchemeIsCaseInsensitive(t *testing.T) {
	idn := newIdentity(t, Config{})
	c := serve(t, idn, func(app *chok.App) { whoami(app, idn) })
	tok := register(t, c, "alice@test.com", "password123")
	for _, scheme := range []string{"bearer ", "BEARER ", "Bearer    "} {
		if code := c.GetJSON("/api/whoami", nil, "Authorization", scheme+tok); code != http.StatusOK {
			t.Errorf("%q = %d", scheme, code)
		}
	}
}

func TestBearer_RejectsOversizedHeader(t *testing.T) {
	huge := "Bearer " + strings.Repeat("a", maxAuthorizationLen)
	if got := bearer(huge); got != "" {
		t.Fatalf("an oversized header must be refused, got %d bytes", len(got))
	}
	atLimit := "Bearer " + strings.Repeat("a", maxAuthorizationLen-len("Bearer "))
	if got := bearer(atLimit); got == "" {
		t.Fatal("a header at exactly the limit is read")
	}
}

func TestAuthn_ChecksTheAccount(t *testing.T) {
	idn := newIdentity(t, Config{})
	c := serve(t, idn, func(app *chok.App) { whoami(app, idn) })
	tok := register(t, c, "alice@test.com", "password123")
	u := userByEmail(t, idn, "alice@test.com")
	if code := c.WithBearer(tok).GetJSON("/api/whoami", nil); code != http.StatusOK {
		t.Fatalf("an active user = %d", code)
	}
	// Disabled: out at once, though the token has not expired.
	if err := idn.Users().SetActive(t.Context(), u.RID, false); err != nil {
		t.Fatal(err)
	}
	var e envelope
	if code := c.WithBearer(tok).GetJSON("/api/whoami", &e); code != http.StatusUnauthorized || e.Message != "account is disabled" {
		t.Fatalf("a disabled user = %d %+v", code, e)
	}
	// Re-enabled: the old token was revoked by both changes.
	if err := idn.Users().SetActive(t.Context(), u.RID, true); err != nil {
		t.Fatal(err)
	}
	if code := c.WithBearer(tok).GetJSON("/api/whoami", &e); code != http.StatusUnauthorized || e.Message != "token invalidated, please log in again" {
		t.Fatalf("a token from before the changes = %d %+v", code, e)
	}
	// A deleted account.
	_, tok = login(t, c, "alice@test.com", "password123")
	if err := idn.users.Delete(t.Context(), store.RID(u.RID)); err != nil {
		t.Fatal(err)
	}
	if code := c.WithBearer(tok).GetJSON("/api/whoami", &e); code != http.StatusUnauthorized || e.Message != "account not found" {
		t.Fatalf("a deleted user = %d %+v", code, e)
	}
}

// note is an application's owned model, fenced to its writer.
type note struct {
	db.OwnedModel
	Text string `json:"text" store:"query,update" gorm:"size:100;not null"`
}

func TestAuthn_ProtectsApplicationRoutes(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&note{})))
	idn := newIdentityOn(t, d, Config{})
	notes := store.New[note](d)
	c := serve(t, idn, func(app *chok.App) {
		api := app.Group("/api", idn.Authn())
		chok.POST(api, "/notes", func(ctx context.Context, req *struct {
			Text string `json:"text" validate:"required"`
		}) (*note, error) {
			n := &note{Text: req.Text}
			return n, notes.Create(ctx, n)
		}, chok.Created())
		chok.List(api, "/notes", notes)
	})
	alice := c.WithBearer(register(t, c, "alice@test.com", "password123"))
	bob := c.WithBearer(register(t, c, "bob@test.com", "password123"))
	if code := c.PostJSON("/api/notes", map[string]string{"text": "x"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous write = %d", code)
	}
	if code := alice.PostJSON("/api/notes", map[string]string{"text": "alice's"}, nil); code != http.StatusCreated {
		t.Fatalf("alice's write = %d", code)
	}
	// The owner column holds the stable user id the principal carries.
	var owner string
	if err := d.Unsafe(t.Context()).Model(&note{}).Select("owner_id").Take(&owner).Error; err != nil {
		t.Fatal(err)
	}
	if owner != userByEmail(t, idn, "alice@test.com").RID {
		t.Fatalf("owner_id = %q", owner)
	}
	var page chok.Page[note]
	if code := bob.GetJSON("/api/notes", &page); code != http.StatusOK || page.Total != 0 {
		t.Fatalf("bob sees alice's notes: %d %+v", code, page)
	}
	if code := alice.GetJSON("/api/notes", &page); code != http.StatusOK || page.Total != 1 {
		t.Fatalf("alice's list = %d %+v", code, page)
	}
}
