package chok

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zynthara/chok/internal/openapi"
	"github.com/zynthara/chok/internal/reqinfo"
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

// panicNote is a panic value that is not an error but prints itself.
type panicNote string

func (n panicNote) String() string { return "note: " + string(n) }

// TestRecover_PanicLogsAnErrorByItsKind: a handler that panics with an
// error is logged the way a 5xx is, by the kinds of its chain, since the
// text can quote a value; a runtime error keeps its text, the Go
// runtime's own words; any other panic value, a string or a
// fmt.Stringer, is the application's own words and is logged as it is.
func TestRecover_PanicLogsAnErrorByItsKind(t *testing.T) {
	for _, c := range []struct {
		name string
		boom func()
		want string
	}{
		{"error", func() { panic(fmt.Errorf("decode: %w", errors.New("bad token s3cret-value"))) }, `"panic":"*fmt.wrapError <- *errors.errorString"`},
		{"string", func() { panic("plain message") }, `"panic":"plain message"`},
		{"stringer", func() { panic(panicNote("a stringer")) }, `"panic":"note: a stringer"`},
		{"index", func() { func(s []int, i int) { _ = s[i] }(nil, 5) }, `"panic":"runtime error: index out of range [5] with length 0"`},
		{"nil map", func() { func(m map[string]int) { m["k"] = 1 }(nil) }, `"panic":"assignment to entry in nil map"`},
		{"assertion", func() { func(v any) { _ = v.(int) }("x") }, `"panic":"interface conversion: interface {} is string, not int"`},
	} {
		var buf bytes.Buffer
		app := New(testBase(t), Logger(slog.New(slog.NewJSONHandler(&buf, nil))))
		GET(app.Router(), "/boom", func(context.Context, *None) (None, error) { c.boom(); return None{}, nil })
		rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/boom"})
		out := buf.String()
		if rec.Code != http.StatusInternalServerError || !strings.Contains(out, `"msg":"chok: panic in handler"`) ||
			!strings.Contains(out, c.want) || strings.Contains(out, "s3cret-value") {
			t.Errorf("%s: %d, want %s in the log:\n%s", c.name, rec.Code, c.want, out)
		}
	}
}

// TestRecover_LogsWithTheRequestLogger: RequestID sits outside Recover,
// so the request Recover holds already carries the request's logger and
// ID: the panic line and the line of the 500 it becomes both go to the
// App's logger with the request_id, and nothing goes to slog.Default.
func TestRecover_LogsWithTheRequestLogger(t *testing.T) {
	before := slog.Default()
	t.Cleanup(func() { slog.SetDefault(before) })
	var global syncBuffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&global, nil)))
	var logs syncBuffer
	app := New(testBase(t), Logger(slog.New(slog.NewTextHandler(&logs, nil))))
	GET(app.Router(), "/boom", func(context.Context, *None) (None, error) { panic("kaboom") })
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/boom"})
	id := rec.Header().Get(requestIDHeader)
	if rec.Code != http.StatusInternalServerError || id == "" {
		t.Fatalf("%d, request id %q", rec.Code, id)
	}
	out := logs.String()
	for _, want := range []string{
		`msg="chok: panic in handler" request_id=` + id + ` panic=kaboom`,
		`msg="chok: request failed" request_id=` + id + ` status=500 reason=InternalError`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the App's log lacks %s:\n%s", want, out)
		}
	}
	if g := global.String(); g != "" {
		t.Errorf("slog.Default got a line:\n%s", g)
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

// TestCSRF_AllowCrossSitePost: a route let past the check takes another
// site's POST below its router's prefix, a repeat is harmless; the
// route's other methods, other routes and a foreign Router's request stay
// checked.
func TestCSRF_AllowCrossSitePost(t *testing.T) {
	app := New(testBase(t), Logger(quiet()))
	pay := app.Group("/pay")
	pay.Handle("", "/return", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusSeeOther) }))
	POST(pay, "/refund", func(context.Context, *None) (None, error) { return None{}, nil })
	AllowCrossSitePost(pay, "/return")
	AllowCrossSitePost(pay, "/return")
	AllowCrossSitePost(struct{ Router }{pay}, "/refund")
	crossSite := map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://gateway.example"}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/pay/return", http.StatusSeeOther},
		{http.MethodPut, "/pay/return", http.StatusForbidden},
		{http.MethodPost, "/pay/refund", http.StatusForbidden},
	} {
		if rec := do(t, app.Handler(), call{method: c.method, path: c.path, header: crossSite}); rec.Code != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, rec.Code, c.want)
		}
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

// TestClientIP_UntrustedForwardingWarnsOnce: a forwarding header from a
// peer that http.trusted_proxies does not list is ignored, and the App
// says so once, naming the peer, without its port, and the header; a
// listed peer's header, or a request without one, says nothing.
func TestClientIP_UntrustedForwardingWarnsOnce(t *testing.T) {
	forwarded := map[string]string{"X-Forwarded-For": "203.0.113.7"}
	for name, c := range map[string]struct {
		trusted []string
		header  map[string]string
		wantIP  string
		warns   int
	}{
		"untrusted peer":       {nil, forwarded, "192.0.2.1", 1},
		"trusted peer":         {[]string{"192.0.2.0/24"}, forwarded, "203.0.113.7", 0},
		"no forwarding header": {nil, nil, "192.0.2.1", 0},
	} {
		t.Run(name, func(t *testing.T) {
			base := testBase(t)
			base.HTTP.TrustedProxies = c.trusted
			var logs bytes.Buffer
			app := New(base, Logger(slog.New(slog.NewJSONHandler(&logs, nil))))
			var got string
			GET(app.Router(), "/ip", func(ctx context.Context, _ *None) (None, error) { got = ClientIP(ctx); return None{}, nil })
			for range 2 { // httptest's peer is 192.0.2.1:1234
				do(t, app.Handler(), call{method: http.MethodGet, path: "/ip", header: c.header})
				if got != c.wantIP {
					t.Fatalf("ClientIP %q, want %q", got, c.wantIP)
				}
			}
			type line struct {
				Level     string `json:"level"`
				Msg       string `json:"msg"`
				RequestID string `json:"request_id"`
				Peer      string `json:"peer"`
				Header    string `json:"header"`
			}
			var warns []line
			for raw := range strings.Lines(logs.String()) {
				var l line
				if err := json.Unmarshal([]byte(raw), &l); err != nil {
					t.Fatalf("log line %q: %v", raw, err)
				}
				if l.Level == "WARN" {
					warns = append(warns, l)
				}
			}
			if len(warns) != c.warns {
				t.Fatalf("%d warnings, want %d:\n%s", len(warns), c.warns, logs.String())
			}
			if c.warns == 0 {
				return
			}
			if w := warns[0]; !strings.Contains(w.Msg, "untrusted peer is ignored; set http.trusted_proxies") ||
				w.Peer != "192.0.2.1" || w.Header != "X-Forwarded-For" || w.RequestID == "" {
				t.Fatalf("the warning names the peer and the header, with the request id: %+v", w)
			}
		})
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
