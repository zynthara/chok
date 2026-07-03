package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zynthara/chok/v2/auth"
	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/handler"
)

// withPrincipal simulates the Authn stage: stamps a fixed principal
// onto every request context.
func withPrincipal(p auth.Principal) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
		})
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.WriteResponse(w, r, 200, map[string]any{"ok": true}, nil)
	})
}

// authzRouter wires principal → AttachAuthz → RequireAuthz around a
// trivial handler, mirroring the production chain web.Module installs.
func authzRouter(az authz.Authorizer, principal *auth.Principal, obj, act string) http.Handler {
	mws := []func(http.Handler) http.Handler{}
	if principal != nil {
		mws = append(mws, withPrincipal(*principal))
	}
	mws = append(mws, AttachAuthz(az), RequireAuthz(obj, act))
	return chain(okHandler(), mws...)
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
// route mounts RequireAuthz but no AttachAuthz ran (no authz module
// assembled and nothing attached manually).
func TestRequireAuthz_NoAuthzAttached_500(t *testing.T) {
	r := chain(okHandler(),
		withPrincipal(auth.Principal{Subject: "usr_x"}),
		// No AttachAuthz!
		RequireAuthz("task", "read"),
	)

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

// domainMux mounts the middleware chain behind a real ServeMux route
// so r.PathValue("wsid") resolves — the stdlib replacement for gin's
// :wsid param in these tests.
func domainMux(az authz.Authorizer, principal *auth.Principal, domainParam string) http.Handler {
	mws := []func(http.Handler) http.Handler{}
	if principal != nil {
		mws = append(mws, withPrincipal(*principal))
	}
	mws = append(mws, AttachAuthz(az), RequireAuthzInDomain("task", "read", domainParam))
	m := http.NewServeMux()
	m.Handle("GET /workspaces/{wsid}/tasks", chain(okHandler(), mws...))
	return m
}

// TestRequireAuthzInDomain_Allowed uses DomainAuthorizerFunc, which
// implements both Authorizer and DomainAuthorizer. The middleware
// reads {wsid} from the path and forwards it to AuthorizeInDomain.
func TestRequireAuthzInDomain_Allowed(t *testing.T) {
	az := authz.DomainAuthorizerFunc(func(_ context.Context, sub, dom, obj, act string) (bool, error) {
		return sub == "usr_1" && dom == "ws-abc" && obj == "task" && act == "read", nil
	})
	r := domainMux(az, &auth.Principal{Subject: "usr_1"}, "wsid")

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
	r := domainMux(az, &auth.Principal{Subject: "usr_x"}, "wsid")

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
// case: the route declares {wsid} but the middleware looks up a
// different name. Better to surface 400 than to call
// AuthorizeInDomain with dom="".
func TestRequireAuthzInDomain_MissingDomainParam(t *testing.T) {
	az := authz.DomainAuthorizerFunc(func(_ context.Context, _, _, _, _ string) (bool, error) {
		return true, nil
	})
	// Route pattern uses {wsid} but middleware looks for wsid_typo.
	r := domainMux(az, &auth.Principal{Subject: "usr_x"}, "wsid_typo")

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
	// No principal injected.
	r := domainMux(az, nil, "wsid")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/workspaces/ws-abc/tasks", nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthorizerFrom_ExportedCarrier(t *testing.T) {
	az := authz.AuthorizerFunc(func(context.Context, string, string, string) (bool, error) {
		return true, nil
	})
	ctx := WithAuthorizer(context.Background(), az)
	if got, ok := AuthorizerFrom(ctx); !ok || got == nil {
		t.Fatal("WithAuthorizer/AuthorizerFrom round-trip failed")
	}
	if _, ok := AuthorizerFrom(context.Background()); ok {
		t.Fatal("bare context must have no authorizer")
	}
}
