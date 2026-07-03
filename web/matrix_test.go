package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/internal/ctxval"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/middleware"
	"github.com/zynthara/chok/v2/metrics"
)

// This file is the SPEC §4.2 behaviour-contract matrix, one test per
// row, each named TestMatrix_*. Decisions under test:
//
//	404/405 apierr envelope           → PRESERVED (v1 NoRoute/NoMethod)
//	unmatched runs full middleware    → PRESERVED
//	trailing-slash / fixed-path fixup → DECLARED CHANGE (ServeMux rules)
//	route labels FullPath ":rid"      → DECLARED CHANGE (Pattern "{rid}")
//	spoofed X-Forwarded-For           → fail-closed (regression)

type envelope struct {
	Code      int    `json:"code"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func do(t *testing.T, c *Component, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)
	return w
}

func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("response is not the apierr envelope: %v (%s)", err, w.Body.String())
	}
	return e
}

func TestMatrix_NoRoute404Envelope(t *testing.T) {
	c := newWebComponent(t, "", nil)
	c.router.Handle("GET", "/exists", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))

	w := do(t, c, "GET", "/missing", nil)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	e := decodeEnvelope(t, w)
	if e.Code != 404 || e.Reason != "NotFound" || e.Message != "route not found" {
		t.Fatalf("v1 NoRoute envelope not preserved: %+v", e)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("envelope content type = %q", ct)
	}
}

func TestMatrix_NoMethod405Envelope(t *testing.T) {
	c := newWebComponent(t, "", nil)
	c.router.Handle("GET", "/things", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))

	w := do(t, c, "DELETE", "/things", nil)
	if w.Code != 405 {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	e := decodeEnvelope(t, w)
	if e.Reason != "MethodNotAllowed" || e.Message != "method not allowed" {
		t.Fatalf("v1 NoMethod envelope not preserved: %+v", e)
	}
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("Allow header lost in translation: %q", allow)
	}
}

func TestMatrix_UnmatchedRunsFullMiddlewareStack(t *testing.T) {
	mc := metricsPeer()
	c := newWebComponent(t, "", []kernel.Component{mc})

	w := do(t, c, "GET", "/definitely-not-mounted", nil)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	// RequestID middleware ran: header stamped, envelope correlated.
	rid := w.Header().Get("X-Request-ID")
	if rid == "" {
		t.Fatal("unmatched request skipped the middleware stack (no X-Request-ID)")
	}
	if e := decodeEnvelope(t, w); e.RequestID != rid {
		t.Fatalf("envelope request_id %q != header %q", e.RequestID, rid)
	}
	// Metrics middleware ran and recorded the fixed "unmatched" label
	// (v1 parity for c.FullPath()=="").
	labels := gatherRequestTotalLabels(t, mc.(*metrics.Component).Registry())
	if labels["path"] != "unmatched" {
		t.Fatalf("RED path label = %q, want unmatched", labels["path"])
	}
	if labels["status"] != "404" {
		t.Fatalf("RED status label = %q, want 404", labels["status"])
	}
}

func TestMatrix_TrailingSlashIsServeMuxSemantics(t *testing.T) {
	c := newWebComponent(t, "", nil)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	c.router.Handle("GET", "/tree/", ok)  // subtree pattern
	c.router.Handle("GET", "/exact", ok)  // exact pattern

	// ServeMux behaviour kept: /tree redirects up to the subtree root.
	w := do(t, c, "GET", "/tree", nil)
	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("subtree redirect expected 307, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/tree/" {
		t.Fatalf("Location = %q, want /tree/", loc)
	}

	// DECLARED CHANGE: v1 gin RedirectTrailingSlash would 301
	// /exact/ → /exact; ServeMux does no such fixup — 404 envelope.
	w = do(t, c, "GET", "/exact/", nil)
	if w.Code != 404 {
		t.Fatalf("declared change broken: /exact/ should 404 (no trailing-slash fixup), got %d", w.Code)
	}

	// Path cleaning stays a ServeMux redirect.
	w = do(t, c, "GET", "/tree//x", nil)
	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("clean-path redirect expected 307, got %d", w.Code)
	}
}

func TestMatrix_RouteLabelsUsePatternStyle(t *testing.T) {
	mc := metricsPeer()
	c := newWebComponent(t, "", []kernel.Component{mc})
	c.router.Handle("GET", "/users/{rid}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))

	if w := do(t, c, "GET", "/users/usr_1", nil); w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// DECLARED CHANGE: metric label is the ServeMux pattern ({rid}),
	// not gin's :rid — dashboards keying on the old style must update.
	labels := gatherRequestTotalLabels(t, mc.(*metrics.Component).Registry())
	if labels["path"] != "/users/{rid}" {
		t.Fatalf("path label = %q, want /users/{rid}", labels["path"])
	}
}

func TestMatrix_SpoofedXFFDoesNotBypassClientIP(t *testing.T) {
	// Fail-closed default: no trusted proxies configured.
	var seen string
	c := newWebComponent(t, "", nil)
	c.router.Handle("GET", "/ip", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = ctxval.ClientIPFrom(r.Context())
		w.WriteHeader(200)
	}))

	// Real socket end-to-end so RemoteAddr is genuine.
	srv := httptest.NewServer(c.handler)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/ip", nil)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Real-IP", "6.6.6.7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if seen == "" || seen == "6.6.6.6" || seen == "6.6.6.7" {
		t.Fatalf("spoofed forwarding headers must not win, resolved %q", seen)
	}
	if !strings.HasPrefix(seen, "127.0.0.1") && !strings.HasPrefix(seen, "::1") {
		t.Fatalf("expected loopback socket peer, got %q", seen)
	}
}

func TestMatrix_TrustedProxyHonoursXFFEndToEnd(t *testing.T) {
	var seen string
	c := newWebComponent(t, `
http:
  trusted_proxies: ["127.0.0.1", "::1"]
`, nil)
	c.router.Handle("GET", "/ip", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = ctxval.ClientIPFrom(r.Context())
		w.WriteHeader(200)
	}))

	srv := httptest.NewServer(c.handler)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/ip", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if seen != "203.0.113.7" {
		t.Fatalf("trusted local proxy: XFF client expected, got %q", seen)
	}
}

// --- router mechanics beyond the matrix --------------------------------

func TestRouter_GroupPrefixAndMiddlewareCompose(t *testing.T) {
	c := newWebComponent(t, "", nil)
	r := c.ProvideRouter()

	tag := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Add("X-Chain", name)
				next.ServeHTTP(w, req)
			})
		}
	}

	api := r.Group("/api/v1", tag("group"))
	api.Handle("GET", "/posts/{rid}", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-RID", req.PathValue("rid"))
		w.WriteHeader(200)
	}), tag("route"))

	w := do(t, c, "GET", "/api/v1/posts/p1", nil)
	if w.Code != 200 {
		t.Fatalf("grouped route unreachable: %d", w.Code)
	}
	if got := w.Header().Get("X-RID"); got != "p1" {
		t.Fatalf("PathValue through group = %q", got)
	}
	// Group middleware outermost, then route middleware (kernel.Router
	// contract: "wrapped by the given middleware (outermost first)").
	if got := w.Header().Values("X-Chain"); len(got) != 2 || got[0] != "group" || got[1] != "route" {
		t.Fatalf("chain order = %v, want [group route]", got)
	}
}

func TestRouter_EmptyPrefixGroupIsMiddlewareOnly(t *testing.T) {
	c := newWebComponent(t, "", nil)
	r := c.ProvideRouter()

	authed := r.Group("", func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Guarded", "yes")
			next.ServeHTTP(w, req)
		})
	})
	authed.Handle("GET", "/direct", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))

	w := do(t, c, "GET", "/direct", nil)
	if w.Code != 200 || w.Header().Get("X-Guarded") != "yes" {
		t.Fatalf("empty-prefix group broken: code=%d guarded=%q", w.Code, w.Header().Get("X-Guarded"))
	}
}

func TestRouter_NestedGroups(t *testing.T) {
	c := newWebComponent(t, "", nil)
	r := c.ProvideRouter()

	v1 := r.Group("/api").Group("/v1")
	v1.Handle("GET", "/ping", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	}))

	if w := do(t, c, "GET", "/api/v1/ping", nil); w.Code != 204 {
		t.Fatalf("nested group route unreachable: %d", w.Code)
	}
}

func TestRouter_RouteTableCollectsHandlerMeta(t *testing.T) {
	type createReq struct {
		Name string `json:"name" binding:"required"`
	}
	c := newWebComponent(t, "", nil)
	r := c.ProvideRouter()

	api := r.Group("/api/v1")
	api.Handle("POST", "/users", handler.HandleRequest(
		func(_ context.Context, req *createReq) (*createReq, error) { return req, nil },
		handler.WithSuccessCode(201), handler.WithSummary("Create user"), handler.WithTags("users"),
	))
	api.Handle("GET", "/plain", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))

	routes := c.Routes()
	if len(routes) != 2 {
		t.Fatalf("route table size = %d", len(routes))
	}
	typed := routes[0]
	if typed.Method != "POST" || typed.Pattern != "/api/v1/users" {
		t.Fatalf("typed route mis-recorded: %+v", typed)
	}
	if typed.Meta == nil || typed.Meta.Code != 201 || typed.Meta.Summary != "Create user" {
		t.Fatalf("handler metadata not collected: %+v", typed.Meta)
	}
	if routes[1].Meta != nil {
		t.Fatal("plain handlers must carry no metadata")
	}
}

func TestModule_ErrorMapperInjection(t *testing.T) {
	sentinel := errors.New("domain: not found")
	reg := apierr.NewMapperRegistry()
	reg.Register(func(err error) *apierr.Error {
		if errors.Is(err, sentinel) {
			return apierr.ErrNotFound.WithMessage("mapped by app registry")
		}
		return nil
	})

	tk := newTestKernel(t, "")
	c := Module().(*Component)
	c.AttachErrorMappers(reg) // App assembly handshake (mini-SPEC §5)
	if err := c.Init(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	c.router.Handle("GET", "/fail", handler.HandleRequest(
		func(context.Context, *struct{}) (any, error) { return nil, sentinel },
	))

	w := do(t, c, "GET", "/fail", nil)
	if w.Code != 404 {
		t.Fatalf("mapper not applied, got %d: %s", w.Code, w.Body.String())
	}
	if e := decodeEnvelope(t, w); e.Message != "mapped by app registry" {
		t.Fatalf("envelope = %+v", e)
	}
}

// fakeAuthzComponent fills the authz role for the soft-dep test.
type fakeAuthzComponent struct {
	az authz.Authorizer
}

func (f *fakeAuthzComponent) Describe() kernel.Descriptor {
	return kernel.Descriptor{Kind: "authz"}
}
func (f *fakeAuthzComponent) Init(context.Context, kernel.Kernel) error { return nil }
func (f *fakeAuthzComponent) Close(context.Context) error               { return nil }
func (f *fakeAuthzComponent) Authorizer() authz.Authorizer              { return f.az }

func TestModule_AuthzSoftDependency(t *testing.T) {
	t.Run("absent: nothing attached, no error", func(t *testing.T) {
		c := newWebComponent(t, "", nil) // Init succeeding IS the assertion
		var attached bool
		c.router.Handle("GET", "/probe", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, attached = middleware.AuthorizerFrom(r.Context())
			w.WriteHeader(200)
		}))
		do(t, c, "GET", "/probe", nil)
		if attached {
			t.Fatal("no authz module assembled ⇒ nothing must be attached")
		}
	})

	t.Run("present: attached to request contexts", func(t *testing.T) {
		az := authz.AuthorizerFunc(func(context.Context, string, string, string) (bool, error) {
			return true, nil
		})
		peer := &fakeAuthzComponent{az: az}
		c := newWebComponent(t, "", []kernel.Component{peer})
		var attached bool
		c.router.Handle("GET", "/probe", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, attached = middleware.AuthorizerFrom(r.Context())
			w.WriteHeader(200)
		}))
		do(t, c, "GET", "/probe", nil)
		if !attached {
			t.Fatal("assembled authz module must be attached to request contexts")
		}
	})
}

func gatherRequestTotalLabels(t *testing.T, reg *prometheus.Registry) map[string]string {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "http_requests_total" {
			continue
		}
		if len(mf.GetMetric()) == 0 {
			t.Fatal("http_requests_total empty")
		}
		labels := map[string]string{}
		for _, lp := range mf.GetMetric()[0].GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		return labels
	}
	t.Fatal("http_requests_total not found")
	return nil
}
