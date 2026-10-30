package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/choktest"
	"github.com/zynthara/chok/internal/tracectx"
)

type postRequest struct {
	ID string `path:"id"`
}

type post struct {
	ID string `json:"id"`
}

func getPost(_ context.Context, req *postRequest) (post, error) { return post{ID: req.ID}, nil }

// spanAttrs flattens a span's attributes.
func spanAttrs(s tracetest.SpanStub) map[attribute.Key]string {
	out := map[attribute.Key]string{}
	for _, kv := range s.Attributes {
		out[kv.Key] = kv.Value.String()
	}
	return out
}

func onlySpan(t *testing.T, spans *tracetest.InMemoryExporter) tracetest.SpanStub {
	t.Helper()
	got := spans.GetSpans()
	if len(got) != 1 {
		t.Fatalf("%d spans, want 1: %v", len(got), got)
	}
	return got[0]
}

// TestApp_AccessLogAndMetricsTogether: with telemetry in the App's stack
// a request is still in the access log, and in the histogram, under the
// same route.
func TestApp_AccessLogAndMetricsTogether(t *testing.T) {
	var logs bytes.Buffer
	app := chok.New(testBase(t), chok.Logger(slog.New(slog.NewJSONHandler(&logs, nil))))
	tel := newTel(t, metricsOn())
	app.Use(tel)
	chok.GET(app.Router(), "/api/v1/posts/{id}", getPost)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/posts/pst_1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	if !strings.Contains(logs.String(), `"msg":"access","method":"GET","path":"/api/v1/posts/pst_1","route":"/api/v1/posts/{id}","status":200`) {
		t.Fatalf("the access log line is there:\n%s", logs.String())
	}
	n, ok := metric(t, tel.Registry(), "http_server_request_duration_seconds",
		map[string]string{"http_request_method": "GET", "http_route": "/api/v1/posts/{id}", "http_response_status_code": "200"})
	if !ok || n != 1 {
		t.Fatalf("the histogram has the request: %v %v", n, ok)
	}
}

// TestMiddleware_SpanNamedAfterTheRoute: the server span of a request is
// "{method} {route}", with the route's template, not the path.
func TestMiddleware_SpanNamedAfterTheRoute(t *testing.T) {
	tel, spans := tracing(t, metricsOn())
	c := choktest.App(t, chok.Base{}, func(app *chok.App) {
		app.Use(tel)
		chok.GET(app.Router(), "/api/v1/posts/{id}", getPost)
	})
	var got post
	if code := c.GetJSON("/api/v1/posts/pst_42", &got, "User-Agent", "chok-test"); code != http.StatusOK || got.ID != "pst_42" {
		t.Fatalf("GET = %d %+v", code, got)
	}
	s := onlySpan(t, spans)
	if s.Name != "GET /api/v1/posts/{id}" {
		t.Fatalf("span name %q", s.Name)
	}
	attrs := spanAttrs(s)
	for k, want := range map[attribute.Key]string{
		"http.request.method": "GET", "http.route": "/api/v1/posts/{id}", "http.response.status_code": "200",
		"url.path": "/api/v1/posts/pst_42", "url.scheme": "http", "user_agent.original": "chok-test",
		"client.address": "192.0.2.1",
	} {
		if attrs[k] != want {
			t.Errorf("%s = %q, want %q", k, attrs[k], want)
		}
	}
	if s.SpanKind.String() != "server" || s.Status.Code != codes.Unset {
		t.Fatalf("a server span, status unset on success: %v %v", s.SpanKind, s.Status)
	}
}

// TestMiddleware_ContinuesTheIncomingTrace: a W3C traceparent makes the
// request's span a child of the caller's, in the caller's trace, and the
// handler sees that span in its context (through the hook identity's
// audit reads).
func TestMiddleware_ContinuesTheIncomingTrace(t *testing.T) {
	tel, spans := tracing(t, Config{})
	var inHandler string
	h := tel.Middleware()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		inHandler = tracectx.TraceID(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)
	s := onlySpan(t, spans)
	if s.SpanContext.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || s.Parent.SpanID().String() != "00f067aa0ba902b7" || !s.Parent.IsRemote() {
		t.Fatalf("trace %s, parent %s remote=%v", s.SpanContext.TraceID(), s.Parent.SpanID(), s.Parent.IsRemote())
	}
	if inHandler != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("the handler's context carries the span: %q", inHandler)
	}
}

func TestMiddleware_RecordsTheRequestDuration(t *testing.T) {
	tel := newTel(t, metricsOn())
	app := chok.New(testBase(t), chok.Logger(quiet()))
	app.Use(tel)
	chok.GET(app.Router(), "/api/v1/posts/{id}", getPost)
	chok.POST(app.Router(), "/api/v1/posts", func(context.Context, *chok.None) (post, error) {
		return post{}, chok.ErrConflict
	})
	for range 2 {
		app.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/posts/a", nil))
	}
	app.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/posts", nil))
	for _, tc := range []struct {
		labels map[string]string
		want   float64
	}{
		{map[string]string{"http_request_method": "GET", "http_route": "/api/v1/posts/{id}", "http_response_status_code": "200"}, 2},
		{map[string]string{"http_request_method": "POST", "http_route": "/api/v1/posts", "http_response_status_code": "409"}, 1},
	} {
		if n, ok := metric(t, tel.Registry(), "http_server_request_duration_seconds", tc.labels); !ok || n != tc.want {
			t.Fatalf("%v: count %v (found %v), want %v", tc.labels, n, ok, tc.want)
		}
	}
}

func TestMiddleware_UnmatchedRoute(t *testing.T) {
	tel, spans := tracing(t, metricsOn())
	app := chok.New(testBase(t), chok.Logger(quiet()))
	app.Use(tel)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/no/such/thing/123", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET = %d", rec.Code)
	}
	if n, ok := metric(t, tel.Registry(), "http_server_request_duration_seconds",
		map[string]string{"http_route": "unmatched", "http_response_status_code": "404"}); !ok || n != 1 {
		t.Fatalf("a 404 is labelled unmatched, never with its path: %v %v", n, ok)
	}
	s := onlySpan(t, spans)
	if _, hasRoute := spanAttrs(s)["http.route"]; s.Name != "GET unmatched" || hasRoute {
		t.Fatalf("span %q, attributes %v", s.Name, spanAttrs(s))
	}
}

// TestMiddleware_OtherMethodsAreOTHER: a method outside the standard ones
// cannot mint a label value.
func TestMiddleware_OtherMethodsAreOTHER(t *testing.T) {
	tel, spans := tracing(t, metricsOn())
	h := tel.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusMethodNotAllowed) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("BREW", "/pot", nil))
	if n, ok := metric(t, tel.Registry(), "http_server_request_duration_seconds",
		map[string]string{"http_request_method": "_OTHER", "http_response_status_code": "405"}); !ok || n != 1 {
		t.Fatalf("BREW is counted as _OTHER: %v %v", n, ok)
	}
	s := onlySpan(t, spans)
	if attrs := spanAttrs(s); s.Name != "HTTP unmatched" || attrs["http.request.method"] != "_OTHER" || attrs["http.request.method_original"] != "BREW" {
		t.Fatalf("span %q, attributes %v", s.Name, attrs)
	}
}

// TestMiddleware_PanicCountsAs500: a panicking handler is a 500 in the
// metrics and an error span, and the App's Recover still answers it.
func TestMiddleware_PanicCountsAs500(t *testing.T) {
	tel, spans := tracing(t, metricsOn())
	app := chok.New(testBase(t), chok.Logger(quiet()))
	app.Use(tel)
	app.Router().Handle(http.MethodGet, "/boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("kaboom") }))
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"reason":"InternalError"`) {
		t.Fatalf("Recover answers: %d %s", rec.Code, rec.Body)
	}
	if n, ok := metric(t, tel.Registry(), "http_server_request_duration_seconds",
		map[string]string{"http_route": "/boom", "http_response_status_code": "500"}); !ok || n != 1 {
		t.Fatalf("the panic is a 500: %v %v", n, ok)
	}
	s := onlySpan(t, spans)
	if s.Status.Code != codes.Error || spanAttrs(s)["error.type"] != "500" {
		t.Fatalf("the span is an error: %v %v", s.Status, spanAttrs(s))
	}
}

func TestMiddleware_ActiveRequestsReturnToZero(t *testing.T) {
	tel := newTel(t, metricsOn())
	var during float64
	h := tel.Middleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		during, _ = metric(t, tel.Registry(), "http_server_active_requests", nil)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	after, ok := metric(t, tel.Registry(), "http_server_active_requests", nil)
	if during != 1 || !ok || after != 0 {
		t.Fatalf("active requests: %v during, %v after", during, after)
	}
}

// TestMiddleware_SharesOneSetOfMetrics: every handler Middleware returns
// writes to the Telemetry's one histogram, registered once.
func TestMiddleware_SharesOneSetOfMetrics(t *testing.T) {
	tel := newTel(t, metricsOn())
	ok200 := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	tel.Middleware()(ok200).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	tel.Middleware()(ok200).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if n, ok := metric(t, tel.Registry(), "http_server_request_duration_seconds", map[string]string{"http_route": "unmatched"}); !ok || n != 2 {
		t.Fatalf("both handlers count into one series: %v %v", n, ok)
	}
}

// TestMetricsEndpoint_ServesTheHistogram: /metrics shows the request
// histogram in the Prometheus text format, the probes under their own
// route.
func TestMetricsEndpoint_ServesTheHistogram(t *testing.T) {
	tel := newTel(t, metricsOn())
	app := chok.New(testBase(t), chok.Logger(quiet()))
	app.Use(tel)
	h := app.Handler()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	body := scrape(t, h, "/metrics")
	for _, want := range []string{
		"# TYPE http_server_request_duration_seconds histogram",
		`http_server_request_duration_seconds_bucket{http_request_method="GET",http_response_status_code="200",http_route="/healthz",le="0.005"}`,
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/healthz"} 1`,
		"# TYPE http_server_active_requests gauge",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics lacks %q:\n%s", want, body)
		}
	}
}
