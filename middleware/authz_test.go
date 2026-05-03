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

// authzRouter wires AttachAuthz + RequireAuthz against a stub
// principal, mirroring the production Authn → AttachAuthz → RequireAuthz
// chain that parts/http.go installs at startup.
func authzRouter(az authz.Authorizer, principal *auth.Principal, obj, act string) *gin.Engine {
	r := gin.New()
	if principal != nil {
		p := *principal
		r.Use(func(c *gin.Context) {
			ctx := auth.WithPrincipal(c.Request.Context(), p)
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		})
	}
	r.Use(AttachAuthz(az))
	r.GET("/resource", RequireAuthz(obj, act), func(c *gin.Context) {
		handler.WriteResponse(c, 200, gin.H{"ok": true}, nil)
	})
	return r
}

func TestRequireAuthz_Allowed(t *testing.T) {
	az := authz.AuthorizerFunc(func(_ context.Context, sub, obj, act string) (bool, error) {
		return sub == "usr_1" && obj == "task" && act == "read", nil
	})
	p := &auth.Principal{Subject: "usr_1"}
	r := authzRouter(az, p, "task", "read")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/resource", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireAuthz_Denied(t *testing.T) {
	az := authz.AuthorizerFunc(func(_ context.Context, _, _, _ string) (bool, error) {
		return false, nil
	})
	p := &auth.Principal{Subject: "usr_2"}
	r := authzRouter(az, p, "task", "read")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/resource", nil)
	r.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["reason"] != "PermissionDenied" {
		t.Fatalf("reason = %v, want PermissionDenied", resp["reason"])
	}
}

func TestRequireAuthz_AuthorizerError_500(t *testing.T) {
	az := authz.AuthorizerFunc(func(_ context.Context, _, _, _ string) (bool, error) {
		return false, errors.New("policy engine down")
	})
	p := &auth.Principal{Subject: "usr_3"}
	r := authzRouter(az, p, "task", "read")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/resource", nil)
	r.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Fatalf("expected 500 for authorizer error, got %d", w.Code)
	}
}

func TestRequireAuthz_NoPrincipal_401(t *testing.T) {
	az := authz.AuthorizerFunc(func(_ context.Context, _, _, _ string) (bool, error) {
		return true, nil
	})
	r := authzRouter(az, nil, "task", "read")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/resource", nil)
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// TestRequireAuthz_NoAuthzAttached_500 covers the wiring-error case:
// route mounts RequireAuthz but no AttachAuthz ran (operator forgot
// to register AuthzComponent or didn't include it in HTTPComponent
// OptionalDependencies).
func TestRequireAuthz_NoAuthzAttached_500(t *testing.T) {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		ctx := auth.WithPrincipal(c.Request.Context(), auth.Principal{Subject: "usr_x"})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	// No AttachAuthz!
	r.GET("/resource", RequireAuthz("task", "read"), func(c *gin.Context) {
		handler.WriteResponse(c, 200, nil, nil)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/resource", nil)
	r.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Fatalf("expected 500 for missing AttachAuthz, got %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if msg, _ := resp["message"].(string); msg != "authz not wired" {
		t.Fatalf("message = %q, want %q", msg, "authz not wired")
	}
}

// --- RequireAuthzInDomain ------------------------------------------

// TestRequireAuthzInDomain_Allowed uses DomainAuthorizerFunc, which
// implements both Authorizer and DomainAuthorizer. The middleware
// reads :wsid from the path and forwards it to AuthorizeInDomain.
func TestRequireAuthzInDomain_Allowed(t *testing.T) {
	az := authz.DomainAuthorizerFunc(func(_ context.Context, sub, dom, obj, act string) (bool, error) {
		return sub == "usr_1" && dom == "ws-abc" && obj == "task" && act == "read", nil
	})
	p := &auth.Principal{Subject: "usr_1"}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		ctx := auth.WithPrincipal(c.Request.Context(), *p)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	r.Use(AttachAuthz(az))
	r.GET("/workspaces/:wsid/tasks", RequireAuthzInDomain("task", "read", "wsid"),
		func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/workspaces/ws-abc/tasks", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRequireAuthzInDomain_FailClosedOnNonDomainAuthorizer is the
// SPEC v0.3.2 invariant: when the registered Authorizer doesn't
// implement DomainAuthorizer, the middleware must REFUSE the request
// (500) rather than silently degrade to Authorize and drop the
// domain constraint.
func TestRequireAuthzInDomain_FailClosedOnNonDomainAuthorizer(t *testing.T) {
	// AuthorizerFunc implements only Authorizer.
	az := authz.AuthorizerFunc(func(_ context.Context, _, _, _ string) (bool, error) {
		return true, nil // would have allowed if degraded
	})
	r := gin.New()
	r.Use(func(c *gin.Context) {
		ctx := auth.WithPrincipal(c.Request.Context(), auth.Principal{Subject: "usr_x"})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	r.Use(AttachAuthz(az))
	r.GET("/workspaces/:wsid/tasks", RequireAuthzInDomain("task", "read", "wsid"),
		func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/workspaces/ws-abc/tasks", nil)
	r.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Fatalf("fail-closed expected 500, got %d (request silently bypassed domain check)", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if msg, _ := resp["message"].(string); msg != "authz domain not supported" {
		t.Fatalf("message = %q, want %q", msg, "authz domain not supported")
	}
}

// TestRequireAuthzInDomain_MissingDomainParam covers the routing-bug
// case: the route declares :wsid but the request doesn't supply it.
// Better to surface 400 than to call AuthorizeInDomain with dom="".
func TestRequireAuthzInDomain_MissingDomainParam(t *testing.T) {
	az := authz.DomainAuthorizerFunc(func(_ context.Context, _, _, _, _ string) (bool, error) {
		return true, nil
	})
	r := gin.New()
	r.Use(func(c *gin.Context) {
		ctx := auth.WithPrincipal(c.Request.Context(), auth.Principal{Subject: "usr_x"})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	r.Use(AttachAuthz(az))
	// Route pattern uses :wsid but middleware looks for :wsid_typo.
	r.GET("/workspaces/:wsid/tasks", RequireAuthzInDomain("task", "read", "wsid_typo"),
		func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/workspaces/ws-abc/tasks", nil)
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400 for missing domain param, got %d", w.Code)
	}
}

func TestRequireAuthzInDomain_NoPrincipal_401(t *testing.T) {
	az := authz.DomainAuthorizerFunc(func(_ context.Context, _, _, _, _ string) (bool, error) {
		return true, nil
	})
	r := gin.New()
	// No principal injected.
	r.Use(AttachAuthz(az))
	r.GET("/workspaces/:wsid/tasks", RequireAuthzInDomain("task", "read", "wsid"),
		func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/workspaces/ws-abc/tasks", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}
