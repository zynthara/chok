package chok

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zynthara/chok/internal/openapi"
)

func tag(name string, log *[]string, mu *sync.Mutex) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			*log = append(*log, name+">")
			mu.Unlock()
			next.ServeHTTP(w, r)
			mu.Lock()
			*log = append(*log, "<"+name)
			mu.Unlock()
		})
	}
}

func TestRouter_GroupPrefixAndMiddlewareCompose(t *testing.T) {
	var log []string
	var mu sync.Mutex
	app := New(testBase(t), Logger(quiet()), Wrap(tag("app", &log, &mu)))
	api := app.Group("/api", tag("group", &log, &mu))
	v1 := api.Group("/v1/", tag("v1", &log, &mu))
	GET(v1, "/ping", func(context.Context, *None) (string, error) { return "pong", nil }, Use(tag("route", &log, &mu)))
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/api/v1/ping"})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	want := []string{"app>", "group>", "v1>", "route>", "<route", "<v1", "<group", "<app"}
	if !slices.Equal(log, want) {
		t.Fatalf("order %v, want %v", log, want)
	}
}

func TestRouter_EmptyPrefixGroupIsMiddlewareOnly(t *testing.T) {
	var log []string
	var mu sync.Mutex
	app := newTestApp(t)
	g := app.Group("", tag("g", &log, &mu))
	GET(g, "/x", func(context.Context, *None) (None, error) { return None{}, nil })
	if rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/x"}); rec.Code != http.StatusNoContent || len(log) != 2 {
		t.Fatalf("%d %v", rec.Code, log)
	}
}

func TestRouter_NoRouteIs404Envelope(t *testing.T) {
	app := newTestApp(t)
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/nope"})
	e := decode[envelope](t, rec)
	if rec.Code != http.StatusNotFound || e.Reason != "NotFound" || e.RequestID == "" {
		t.Fatalf("%d %+v", rec.Code, e)
	}
}

func TestRouter_WrongMethodIs405EnvelopeWithAllow(t *testing.T) {
	app := newTestApp(t)
	GET(app.Router(), "/only-get", func(context.Context, *None) (None, error) { return None{}, nil })
	rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/only-get"})
	e := decode[envelope](t, rec)
	if rec.Code != http.StatusMethodNotAllowed || e.Reason != "MethodNotAllowed" || !strings.Contains(rec.Header().Get("Allow"), "GET") {
		t.Fatalf("%d %+v %v", rec.Code, e, rec.Header())
	}
}

func TestRouter_TrailingSlashIsServeMuxSemantics(t *testing.T) {
	app := newTestApp(t)
	app.Router().Handle(http.MethodGet, "/dir/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	if rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/dir"}); rec.Code/100 != 3 || rec.Header().Get("Location") != "/dir/" {
		t.Fatalf("ServeMux redirects to the canonical path: %d %v", rec.Code, rec.Header())
	}
	if rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/dir/anything"}); rec.Code != http.StatusTeapot {
		t.Fatalf("a trailing-slash pattern is a subtree: %d", rec.Code)
	}
}

func TestRouter_UnmatchedRunsFullMiddlewareStack(t *testing.T) {
	var log []string
	var mu sync.Mutex
	app := New(testBase(t), Logger(quiet()), Wrap(tag("app", &log, &mu)))
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/missing"})
	if rec.Header().Get(requestIDHeader) == "" || len(log) != 2 {
		t.Fatalf("a 404 still runs the stack: %v %v", rec.Header(), log)
	}
}

func TestRouter_AccessLogUsesRoutePattern(t *testing.T) {
	var buf bytes.Buffer
	app := New(testBase(t), Logger(slog.New(slog.NewJSONHandler(&buf, nil))))
	GET(app.Group("/api"), "/posts/{id}", func(context.Context, *None) (None, error) { return None{}, nil })
	do(t, app.Handler(), call{method: http.MethodGet, path: "/api/posts/42"})
	do(t, app.Handler(), call{method: http.MethodGet, path: "/nowhere"})
	do(t, app.Handler(), call{method: http.MethodGet, path: "/healthz"})
	out := buf.String()
	for _, want := range []string{`"route":"/api/posts/{id}"`, `"path":"/api/posts/42"`, `"status":204`, `"route":"unmatched"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("access log lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"path":"/healthz"`) {
		t.Fatal("probes are not access-logged")
	}
}

func TestRouter_RouteTableCollectsTypedRoutes(t *testing.T) {
	app := newTestApp(t)
	GET(app.Group("/api"), "/x/{id}", echoItem, Summary("Read x"), Use(fakeAuthn(openapi.BearerAuth)))
	app.Router().Handle(http.MethodGet, "/raw", http.NotFoundHandler())
	docs := app.routes.all()
	if len(docs) != 1 || docs[0].path != "/api/x/{id}" || docs[0].method != http.MethodGet || docs[0].cfg.summary != "Read x" ||
		len(docs[0].security) != 1 || docs[0].security[0].Name != "bearerAuth" {
		t.Fatalf("docs: %+v", docs)
	}
}

func TestRouter_SpoofedForwardedForDoesNotBypassClientIP(t *testing.T) {
	app := newTestApp(t)
	var got string
	GET(app.Router(), "/ip", func(ctx context.Context, _ *None) (None, error) { got = ClientIP(ctx); return None{}, nil })
	do(t, app.Handler(), call{method: http.MethodGet, path: "/ip", header: map[string]string{"X-Forwarded-For": "1.2.3.4"}})
	if got != "192.0.2.1" {
		t.Fatalf("without trusted proxies the peer wins, got %q", got)
	}
}

func TestRouter_TrustedProxyHonoursForwardedFor(t *testing.T) {
	base := testBase(t)
	base.HTTP.TrustedProxies = []string{"192.0.2.0/24"}
	app := New(base, Logger(quiet()))
	var got string
	GET(app.Router(), "/ip", func(ctx context.Context, _ *None) (None, error) { got = ClientIP(ctx); return None{}, nil })
	do(t, app.Handler(), call{method: http.MethodGet, path: "/ip", header: map[string]string{"X-Forwarded-For": "1.2.3.4"}})
	if got != "1.2.3.4" {
		t.Fatalf("a trusted peer's header is believed, got %q", got)
	}
}
