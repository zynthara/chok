package middleware

import (
	"context"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"github.com/zynthara/chok/internal/ctxval"
	"github.com/zynthara/chok/log"
)

// Logger injects the given logger into the request context. The logger is
// enriched with the request's request_id (if present) so downstream code
// calling log.FromContext(ctx) gets structured per-request logging for free.
//
// Both ctxval (internal, used by handler error logging) and log.WithContext
// (public, used by business code) are populated.
func Logger(l log.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// Build a per-request logger enriched with request_id and
		// trace context (when OpenTelemetry tracing is active).
		reqLogger := l
		if rid := ctxval.RequestIDFrom(ctx); rid != "" {
			reqLogger = l.With("request_id", rid)
		}
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			reqLogger = reqLogger.With(
				"trace_id", sc.TraceID().String(),
				"span_id", sc.SpanID().String(),
			)
		}

		ctx = ctxval.WithLogger(ctx, reqLogger)
		ctx = log.WithContext(ctx, reqLogger)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// RequestIDFrom extracts the request ID from context (user-facing helper).
func RequestIDFrom(ctx context.Context) string {
	return ctxval.RequestIDFrom(ctx)
}

// LoggerFrom extracts the logger from context (user-facing helper).
// Prefers log.FromContext (canonical path); falls back to ctxval for
// backward compatibility.
func LoggerFrom(ctx context.Context) log.Logger {
	if l := log.FromContext(ctx); l != nil {
		return l
	}
	if l, ok := ctxval.LoggerFrom(ctx).(log.Logger); ok {
		return l
	}
	return nil
}
