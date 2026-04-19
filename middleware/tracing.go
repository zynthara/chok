package middleware

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracing returns a middleware that creates a server span for each HTTP
// request using the global OpenTelemetry TracerProvider. Span name follows
// the OpenTelemetry HTTP semantic conventions: "METHOD route" (e.g.
// "GET /api/v1/posts/:id").
//
// When the TracerProvider is a no-op (tracing disabled), the middleware
// adds negligible overhead — the span is never sampled and attributes
// are not recorded.
//
// This middleware should be placed early in the chain (after Recovery,
// RequestID) so that downstream middleware and handlers inherit the
// span context. The Logger middleware will automatically pick up
// trace_id / span_id from the span context.
func Tracing(serviceName string) gin.HandlerFunc {
	tracer := otel.Tracer(serviceName)
	propagator := otel.GetTextMapPropagator()

	return func(c *gin.Context) {
		// Extract incoming span context from request headers (W3C
		// Trace Context, B3, etc. depending on configured propagator).
		ctx := propagator.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))

		// Start a server span. The span name uses the matched route
		// template (e.g. "/api/v1/posts/:id") rather than the actual
		// URL to keep cardinality low.
		route := c.FullPath()
		if route == "" {
			route = "unmatched" // fixed value to prevent cardinality explosion
		}
		spanName := c.Request.Method + " " + route

		ctx, span := tracer.Start(ctx, spanName,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(c.Request.Method),
				semconv.URLPath(c.Request.URL.Path),
				semconv.ServerAddress(c.Request.Host),
			),
		)
		defer span.End()

		c.Request = c.Request.WithContext(ctx)
		c.Next()

		// Record response attributes.
		status := c.Writer.Status()
		span.SetAttributes(semconv.HTTPResponseStatusCode(status))
		if status >= 500 {
			span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%d", status)))
		}
	}
}
