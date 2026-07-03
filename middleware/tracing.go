package middleware

import (
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/zynthara/chok/v2/internal/ctxval"
)

// Tracing returns a middleware that creates a server span for each HTTP
// request using the global OpenTelemetry TracerProvider. Span name follows
// the OpenTelemetry HTTP semantic conventions: "METHOD route" (e.g.
// "GET /api/v1/posts/{id}").
//
// When the TracerProvider is a no-op (tracing disabled), the middleware
// adds negligible overhead — the span is never sampled and attributes
// are not recorded.
//
// This middleware should be placed early in the chain (after Recovery,
// RequestID) so that downstream middleware and handlers inherit the
// span context. The Logger middleware will automatically pick up
// trace_id / span_id from the span context.
//
// Route-template naming detail (M2): the matched pattern is only known
// after routing, so the span starts as "METHOD" and is renamed to
// "METHOD pattern" once the handler returns ("METHOD unmatched" for
// unrouted requests). Name-based head sampling that keyed on the full
// v1 span name should switch to attribute-based rules.
func Tracing(serviceName string) func(http.Handler) http.Handler {
	tracer := otel.Tracer(serviceName)
	propagator := otel.GetTextMapPropagator()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract incoming span context from request headers (W3C
			// Trace Context, B3, etc. depending on configured propagator).
			ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

			ctx, span := tracer.Start(ctx, r.Method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					semconv.HTTPRequestMethodKey.String(r.Method),
					semconv.URLPath(r.URL.Path),
					semconv.ServerAddress(r.Host),
				),
			)
			defer span.End()

			r = r.WithContext(ctx)
			next.ServeHTTP(w, r)

			// Rename with the matched route template (low cardinality)
			// and record response attributes.
			route := ctxval.RoutePatternFrom(ctx)
			if route == "" {
				route = "unmatched" // fixed value to prevent cardinality explosion
			}
			span.SetName(r.Method + " " + route)
			if route != "unmatched" {
				span.SetAttributes(semconv.HTTPRoute(route))
			}
			status := statusOf(w)
			span.SetAttributes(semconv.HTTPResponseStatusCode(status))
			if status >= 500 {
				span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%d", status)))
			}
		})
	}
}
