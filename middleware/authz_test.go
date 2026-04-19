package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/authz"
	"github.com/zynthara/chok/handler"
)

func authzRouter(az authz.Authorizer, principal *auth.Principal) *gin.Engine {
	r := gin.New()
	// Simulate authn by injecting principal into context.
	if principal != nil {
		p := *principal
		r.Use(func(c *gin.Context) {
			ctx := auth.WithPrincipal(c.Request.Context(), p)
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		})
	}
	r.Use(Authz(az))
	r.GET("/resource", func(c *gin.Context) {
		handler.WriteResponse(c, 200, gin.H{"ok": true}, nil)
	})
	return r
}

func TestAuthz_Allowed(t *testing.T) {
	az := authz.AuthorizerFunc(func(_ context.Context, sub, obj, act string) (bool, error) {
		return sub == "usr_1" && act == "GET", nil
	})
	p := &auth.Principal{Subject: "usr_1"}
	r := authzRouter(az, p)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/resource", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthz_Denied(t *testing.T) {
	az := authz.AuthorizerFunc(func(_ context.Context, _, _, _ string) (bool, error) {
		return false, nil
	})
	p := &auth.Principal{Subject: "usr_2"}
	r := authzRouter(az, p)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/resource", nil)
	r.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["reason"] != "PermissionDenied" {
		t.Fatalf("reason = %v, want PermissionDenied", resp["reason"])
	}
}

func TestAuthz_AuthorizerError_500(t *testing.T) {
	az := authz.AuthorizerFunc(func(_ context.Context, _, _, _ string) (bool, error) {
		return false, errors.New("policy engine down")
	})
	p := &auth.Principal{Subject: "usr_3"}
	r := authzRouter(az, p)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/resource", nil)
	r.ServeHTTP(w, req)

	// Infrastructure error → 500, not 403.
	if w.Code != 500 {
		t.Fatalf("expected 500 for authorizer error, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["reason"] != "InternalError" {
		t.Fatalf("reason = %v, want InternalError", resp["reason"])
	}
}

func TestAuthz_NoPrincipal_401(t *testing.T) {
	az := authz.AuthorizerFunc(func(_ context.Context, _, _, _ string) (bool, error) {
		return true, nil
	})
	// No principal injected — simulates missing authn middleware.
	r := authzRouter(az, nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/resource", nil)
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthz_NilAuthorizer_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil authorizer")
		}
	}()
	Authz(nil)
}

func TestAuthz_UsesRoutePattern(t *testing.T) {
	var capturedObj string
	az := authz.AuthorizerFunc(func(_ context.Context, _, obj, _ string) (bool, error) {
		capturedObj = obj
		return true, nil
	})
	p := &auth.Principal{Subject: "usr_4"}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		ctx := auth.WithPrincipal(c.Request.Context(), *p)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	r.Use(Authz(az))
	r.GET("/users/:id", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/users/usr_abc", nil)
	r.ServeHTTP(w, req)

	// FullPath() should return the route pattern, not the actual URL.
	if capturedObj != "/users/:id" {
		t.Fatalf("object = %q, want /users/:id (route pattern)", capturedObj)
	}
}
