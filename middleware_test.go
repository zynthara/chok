package chok

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zynthara/chok/v3/internal/csrf"
	"github.com/zynthara/chok/v3/internal/openapi"
	"github.com/zynthara/chok/v3/internal/reqinfo"
)

func TestRecover_PanicBecomes500Envelope(t *testing.T) {
	var buf bytes.Buffer
	app := New(testBase(t), Logger(slog.New(slog.NewJSONHandler(&buf, nil))))
	GET(app.Router(), "/boom", func(context.Context, *None) (None, error) { panic("kaboom") })
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/boom"})
	e := decode[envelope](t, rec)
	if rec.Code != http.StatusInternalServerError || e.Reason != "InternalError" || e.RequestID == "" {
		t.Fatalf("%d %+v", rec.Code, e)
	}
	if !strings.Contains(buf.String(), "kaboom") || !strings.Contains(buf.String(), `"request_id"`) {
		t.Fatalf("the panic is logged with the request id: %s", buf.String())
	}
}

func TestRecover_DoesNotDoubleWrite(t *testing.T) {
	app := newTestApp(t)
	app.Router().Handle(http.MethodGet, "/half", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("partial"))
		panic("after writing")
	}))
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/half"})
	if rec.Code != http.StatusAccepted || rec.Body.String() != "partial" {
		t.Fatalf("a started response is left alone: %d %q", rec.Code, rec.Body)
	}
}

func TestRequestID_GeneratedPropagatedSanitized(t *testing.T) {
	app := newTestApp(t)
	var seen string
	GET(app.Router(), "/id", func(ctx context.Context, _ *None) (None, error) { seen = RequestID(ctx); return None{}, nil })
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/id"})
	if len(seen) != 32 || rec.Header().Get(requestIDHeader) != seen {
		t.Fatalf("generated: %q header %q", seen, rec.Header().Get(requestIDHeader))
	}
	rec = do(t, app.Handler(), call{method: http.MethodGet, path: "/id", header: map[string]string{requestIDHeader: "abc-1.2_3"}})
	if seen != "abc-1.2_3" || rec.Header().Get(requestIDHeader) != "abc-1.2_3" {
		t.Fatalf("propagated: %q", seen)
	}
	do(t, app.Handler(), call{method: http.MethodGet, path: "/id", header: map[string]string{requestIDHeader: "a\nb\"c d"}})
	if seen != "abcd" {
		t.Fatalf("log-injection characters are stripped: %q", seen)
	}
	do(t, app.Handler(), call{method: http.MethodGet, path: "/id", header: map[string]string{requestIDHeader: "\n\n"}})
	if len(seen) != 32 {
		t.Fatalf("an all-invalid id is replaced: %q", seen)
	}
	long := strings.Repeat("x", 300)
	do(t, app.Handler(), call{method: http.MethodGet, path: "/id", header: map[string]string{requestIDHeader: long}})
	if len(seen) != 128 {
		t.Fatalf("ids are capped at 128 bytes: %d", len(seen))
	}
}

func TestCORS(t *testing.T) {
	base := testBase(t)
	base.HTTP.CORS.AllowOrigins = []string{"https://app.example"}
	base.HTTP.CORS.MaxAge = 10 * time.Minute
	app := New(base, Logger(quiet()))
	GET(app.Router(), "/x", func(context.Context, *None) (string, error) { return "ok", nil })
	rec := do(t, app.Handler(), call{method: http.MethodOptions, path: "/x", header: map[string]string{
		"Origin": "https://app.example", "Access-Control-Request-Method": "GET",
	}})
	h := rec.Header()
	if rec.Code != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != "https://app.example" ||
		h.Get("Access-Control-Max-Age") != "600" || !strings.Contains(h.Get("Access-Control-Allow-Methods"), "PATCH") {
		t.Fatalf("preflight: %d %v", rec.Code, h)
	}
	rec = do(t, app.Handler(), call{method: http.MethodGet, path: "/x", header: map[string]string{"Origin": "https://evil.example"}})
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("a disallowed origin passes through without CORS headers: %v", rec.Header())
	}
	rec = do(t, app.Handler(), call{method: http.MethodGet, path: "/x", header: map[string]string{"Origin": "https://app.example"}})
	if rec.Header().Get("Access-Control-Expose-Headers") != "X-Request-Id" {
		t.Fatalf("expose: %v", rec.Header())
	}
}

// TestCSRF_RejectsCrossSitePost: with the default http.csrf, a browser's
// cross-site POST is a 403 envelope; a CORS origin is trusted, and a
// same-origin request or one without the browser's headers passes.
func TestCSRF_RejectsCrossSitePost(t *testing.T) {
	base := testBase(t)
	base.HTTP.CORS.AllowOrigins = []string{"https://app.example"}
	app := New(base, Logger(quiet()))
	POST(app.Router(), "/transfer", func(context.Context, *None) (None, error) { return None{}, nil })
	rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/transfer", header: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}})
	if e := decode[envelope](t, rec); rec.Code != http.StatusForbidden || e.Reason != "Forbidden" {
		t.Fatalf("cross-site POST: %d %+v", rec.Code, e)
	}
	rec = do(t, app.Handler(), call{method: http.MethodPost, path: "/transfer", header: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://app.example"}})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("a CORS origin is trusted: %d", rec.Code)
	}
	rec = do(t, app.Handler(), call{method: http.MethodPost, path: "/transfer", header: map[string]string{"Sec-Fetch-Site": "same-origin"}})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("same-origin: %d", rec.Code)
	}
	rec = do(t, app.Handler(), call{method: http.MethodPost, path: "/transfer"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("no Sec-Fetch-Site and no Origin, as from curl: %d", rec.Code)
	}
}

// TestCSRF_OnByDefault: http.csrf defaults to true, through Load and in
// the defaults a zero Base gets.
func TestCSRF_OnByDefault(t *testing.T) {
	if !testBase(t).HTTP.CSRF {
		t.Fatal("the defaults leave csrf off")
	}
	loaded, err := Load[Base]("csrftest", Args(nil))
	if err != nil || !loaded.HTTP.CSRF {
		t.Fatalf("Load: csrf %v, %v", loaded.HTTP.CSRF, err)
	}
	app := New(loaded, Logger(quiet()))
	POST(app.Router(), "/x", func(context.Context, *None) (None, error) { return None{}, nil })
	if rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/x", header: map[string]string{"Sec-Fetch-Site": "cross-site"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", rec.Code)
	}
}

// TestCSRF_BatteryExemption: a battery exempts its own route through the
// internal hook, below its router's prefix; other routes stay checked, a
// repeat is harmless and a foreign router is ignored.
func TestCSRF_BatteryExemption(t *testing.T) {
	app := New(testBase(t), Logger(quiet()))
	auth := app.Group("/auth")
	auth.Handle(http.MethodPost, "/apple/callback", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusFound) }))
	POST(auth, "/login", func(context.Context, *None) (None, error) { return None{}, nil })
	csrf.Exempt(auth, http.MethodPost, "/apple/callback")
	csrf.Exempt(auth, http.MethodPost, "/apple/callback")
	csrf.Exempt(http.NewServeMux(), http.MethodPost, "/login")
	crossSite := map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://appleid.apple.com"}
	if rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/auth/apple/callback", header: crossSite}); rec.Code != http.StatusFound {
		t.Fatalf("exempted callback: %d", rec.Code)
	}
	if rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/auth/login", header: crossSite}); rec.Code != http.StatusForbidden {
		t.Fatalf("login stays checked: %d", rec.Code)
	}
}

// TestCSRF_CookieWithoutCSRFWarnsOnce: with http.csrf off and routes
// that accept the session cookie, Run warns once before it serves; with
// csrf on, or only a bearer token, it says nothing.
func TestCSRF_CookieWithoutCSRFWarnsOnce(t *testing.T) {
	ok := func(context.Context, *None) (None, error) { return None{}, nil }
	for name, c := range map[string]struct {
		csrf  bool
		authn Middleware
		want  int
	}{
		"cookie, csrf off": {false, fakeAuthn(openapi.BearerAuth, openapi.CookieAuth("sid")), 1},
		"cookie, csrf on":  {true, fakeAuthn(openapi.CookieAuth("sid")), 0},
		"bearer, csrf off": {false, fakeAuthn(openapi.BearerAuth), 0},
	} {
		t.Run(name, func(t *testing.T) {
			base := testBase(t)
			base.HTTP.CSRF = c.csrf
			var logs syncBuffer
			app := New(base, Logger(slog.New(slog.NewJSONHandler(&logs, nil))))
			api := app.Group("/api", c.authn)
			GET(api, "/a", ok)
			POST(api, "/b", ok)
			app.Router().Handle(http.MethodGet, "/raw", http.NotFoundHandler(), c.authn)
			ctx, cancel := context.WithCancel(t.Context())
			done := runAsync(app, ctx)
			waitFor(t, app.ready.Load, "ready")
			cancel()
			if err := await(t, done); err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(logs.String(), "http.csrf is off while routes accept a session cookie"); n != c.want {
				t.Fatalf("%d warnings, want %d:\n%s", n, c.want, logs.String())
			}
		})
	}
	// Wrap's authentication counts as well.
	base := testBase(t)
	base.HTTP.CSRF = false
	var logs syncBuffer
	app := New(base, Logger(slog.New(slog.NewJSONHandler(&logs, nil))), Wrap(fakeAuthn(openapi.CookieAuth("sid"))))
	ctx, cancel := context.WithCancel(t.Context())
	done := runAsync(app, ctx)
	waitFor(t, app.ready.Load, "ready")
	cancel()
	if err := await(t, done); err != nil || strings.Count(logs.String(), "http.csrf is off") != 1 {
		t.Fatalf("Wrap: %v\n%s", err, logs.String())
	}
}

// syncBuffer is a bytes.Buffer the App's goroutines may log into.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestTimeout_Writes504WhenHandlerRespectsCtx(t *testing.T) {
	base := testBase(t)
	base.HTTP.RequestTimeout = 20 * time.Millisecond
	app := New(base, Logger(quiet()))
	GET(app.Router(), "/slow", func(ctx context.Context, _ *None) (None, error) {
		<-ctx.Done()
		return None{}, ctx.Err()
	})
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/slow"})
	if e := decode[envelope](t, rec); rec.Code != http.StatusGatewayTimeout || e.Reason != "Timeout" || e.RequestID == "" {
		t.Fatalf("%d %+v", rec.Code, e)
	}
}

func TestTimeout_NoDoubleWriteWhenHandlerWrote(t *testing.T) {
	base := testBase(t)
	base.HTTP.RequestTimeout = 10 * time.Millisecond
	app := New(base, Logger(quiet()))
	app.Router().Handle(http.MethodGet, "/wrote", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	if rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/wrote"}); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
}

func TestTimeout_ZeroDisables(t *testing.T) {
	app := newTestApp(t)
	var has bool
	GET(app.Router(), "/d", func(ctx context.Context, _ *None) (None, error) { _, has = ctx.Deadline(); return None{}, nil })
	do(t, app.Handler(), call{method: http.MethodGet, path: "/d"})
	if has {
		t.Fatal("request_timeout 0 sets no deadline")
	}
}

func TestStatusWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	w := trackWriter(rec)
	if trackWriter(w) != w {
		t.Fatal("wrapping is idempotent")
	}
	w.WriteHeader(http.StatusCreated)
	w.WriteHeader(http.StatusTeapot)
	_, _ = w.Write([]byte("abc"))
	if w.code() != http.StatusCreated || rec.Code != http.StatusCreated || w.bytes != 3 {
		t.Fatalf("status %d/%d bytes %d", w.code(), rec.Code, w.bytes)
	}
	w2 := trackWriter(httptest.NewRecorder())
	_, _ = w2.Write([]byte("x"))
	if w2.code() != http.StatusOK {
		t.Fatal("a write without WriteHeader is 200")
	}
	if http.NewResponseController(w2).Flush() != nil || w2.Unwrap() == nil {
		t.Fatal("Flush reaches the underlying writer")
	}
}

type instrument struct{ seen *[]string }

func (i instrument) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			*i.seen = append(*i.seen, fmt.Sprint(RequestID(r.Context()) != "" && ClientIP(r.Context()) != ""))
		})
	}
}

func TestInstrumenter_SitsInsideRequestIDAndClientIP(t *testing.T) {
	var seen []string
	app := newTestApp(t)
	app.Use(instrument{seen: &seen})
	GET(app.Router(), "/x", func(context.Context, *None) (None, error) { return None{}, nil })
	do(t, app.Handler(), call{method: http.MethodGet, path: "/x"})
	if len(seen) != 1 || seen[0] != "true" {
		t.Fatalf("instrumenters see the request id and client ip: %v", seen)
	}
}

// routeSeen records the route pattern a request was served under, as
// telemetry reads it.
type routeSeen struct{ routes *[]string }

func (i routeSeen) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			route := "?"
			if info := reqinfo.From(r.Context()); info != nil {
				route = info.Route
			}
			*i.routes = append(*i.routes, route)
		})
	}
}

// TestBuiltins_CarryTheirRoute: the probes, the OpenAPI document and UI and
// the pprof endpoints are named by their pattern in the access log and to
// instrumenters, like any route; only a request no pattern matched has
// none (the access log's "unmatched").
func TestBuiltins_CarryTheirRoute(t *testing.T) {
	var buf bytes.Buffer
	base := testBase(t)
	base.App.Debug = true
	app := New(base, Logger(slog.New(slog.NewJSONHandler(&buf, nil))))
	var routes []string
	app.Use(routeSeen{routes: &routes})
	paths := []string{"/healthz", "/livez", "/readyz", "/openapi.json", "/docs", "/debug/pprof/cmdline", "/debug/pprof/heap", "/nowhere"}
	for _, p := range paths {
		do(t, app.Handler(), call{method: http.MethodGet, path: p})
	}
	want := []string{"/healthz", "/livez", "/readyz", "/openapi.json", "/docs", "/debug/pprof/cmdline", "/debug/pprof/", ""}
	if fmt.Sprint(routes) != fmt.Sprint(want) {
		t.Fatalf("routes %q, want %q", routes, want)
	}
	if !strings.Contains(buf.String(), `"path":"/openapi.json","route":"/openapi.json"`) ||
		!strings.Contains(buf.String(), `"path":"/nowhere","route":"unmatched"`) {
		t.Fatalf("the access log names the built-in route: %s", buf.String())
	}
}
