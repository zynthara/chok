package telemetry

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/internal/reqinfo"
)

// unmatched is the route of a request no route pattern matched: a 404 or
// a 405. It cannot collide with a pattern, which starts with a slash.
const unmatched = "unmatched"

// httpMetrics are the RED metrics of the server, named after the OpenTelemetry
// HTTP semantic conventions: http.server.request.duration and
// http.server.active_requests, written as Prometheus names.
type httpMetrics struct {
	duration *prometheus.HistogramVec
	active   prometheus.Gauge
}

func newHTTPMetrics() *httpMetrics {
	return &httpMetrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_server_request_duration_seconds",
			Help: "Duration of HTTP server requests (OpenTelemetry http.server.request.duration).",
			// The bucket boundaries the semantic conventions advise.
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10},
		}, []string{"http_request_method", "http_route", "http_response_status_code"}),
		active: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_server_active_requests",
			Help: "HTTP server requests in flight (OpenTelemetry http.server.active_requests).",
		}),
	}
}

func (m *httpMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.duration, m.active}
}

// Middleware returns the request instrumentation the App puts between
// ClientIP and AccessLog (t implements chok.Instrumenter): it continues
// the trace of an incoming W3C traceparent, records a server span named
// "{method} {route}", such as "GET /api/v1/posts/{id}", and observes the
// request's duration in http_server_request_duration_seconds by method,
// route and status code. A request no route matched is named "GET
// unmatched"; a method outside the standard ones is recorded as _OTHER
// (the span's name says HTTP), so neither can grow the label sets. A
// panic counts as a 500 and goes on to the App's Recover.
func (t *Telemetry) Middleware() chok.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			begin := time.Now()
			if t.http != nil {
				t.http.active.Inc()
				defer t.http.active.Dec()
			}
			method, known := normalizeMethod(r.Method)
			ctx := t.prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx, span := t.tracer.Start(ctx, spanMethod(method, known), trace.WithSpanKind(trace.SpanKindServer))
			if span.IsRecording() { // tracing on and sampled
				span.SetAttributes(requestAttributes(r, method, known)...)
			}
			sw := &statusWriter{ResponseWriter: w}
			r = r.WithContext(ctx)
			completed := false
			defer func() {
				if completed {
					return
				}
				// next panicked: record a 500, unless a status went out
				// already. The panic goes on, untouched, to the App's
				// Recover, which answers and logs it.
				status := sw.status
				if status == 0 {
					status = http.StatusInternalServerError
				}
				t.finish(r, span, method, known, status, begin)
				span.SetStatus(codes.Error, "panic")
				span.End()
			}()
			next.ServeHTTP(sw, r)
			completed = true
			t.finish(r, span, method, known, sw.code(), begin)
			span.End()
		})
	}
}

// finish names the span after the matched route and records the request.
func (t *Telemetry) finish(r *http.Request, span trace.Span, method string, known bool, status int, begin time.Time) {
	route := unmatched
	if info := reqinfo.From(r.Context()); info != nil && info.Route != "" {
		route = info.Route
	}
	if span.IsRecording() {
		span.SetName(spanMethod(method, known) + " " + route)
		span.SetAttributes(semconv.HTTPResponseStatusCode(status))
		if route != unmatched {
			span.SetAttributes(semconv.HTTPRoute(route))
		}
		if status >= http.StatusInternalServerError {
			span.SetAttributes(semconv.ErrorTypeKey.String(strconv.Itoa(status)))
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	}
	if t.http != nil {
		t.http.duration.WithLabelValues(method, route, strconv.Itoa(status)).Observe(time.Since(begin).Seconds())
	}
}

// normalizeMethod maps the methods outside RFC 9110 and RFC 5789 to
// _OTHER, as the semantic conventions ask: the method is a label, and a
// client must not be able to mint new ones.
func normalizeMethod(m string) (string, bool) {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return m, true
	}
	return "_OTHER", false
}

// spanMethod is the method part of a span's name: HTTP for _OTHER.
func spanMethod(method string, known bool) string {
	if !known {
		return "HTTP"
	}
	return method
}

func requestAttributes(r *http.Request, method string, known bool) []attribute.KeyValue {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(method),
		semconv.URLPath(r.URL.Path),
		semconv.URLScheme(scheme),
		semconv.ServerAddress(hostOnly(r.Host)),
		semconv.NetworkProtocolVersion(fmt.Sprintf("%d.%d", r.ProtoMajor, r.ProtoMinor)),
	}
	if !known {
		attrs = append(attrs, semconv.HTTPRequestMethodOriginal(r.Method))
	}
	if ua := r.UserAgent(); ua != "" {
		attrs = append(attrs, semconv.UserAgentOriginal(ua))
	}
	if ip := chok.ClientIP(r.Context()); ip != "" {
		attrs = append(attrs, semconv.ClientAddress(ip))
	}
	return attrs
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// statusWriter records the status of a response, passing Flush, Hijack
// and the http.ResponseController methods through.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 && (code < 100 || code > 199 || code == http.StatusSwitchingProtocols) {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("telemetry: the response writer cannot hijack")
}

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
