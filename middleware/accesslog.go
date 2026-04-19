package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"github.com/zynthara/chok/internal/ctxval"
	"github.com/zynthara/chok/log"
)

// AccessLog logs request method, path, status, and latency.
// When a tracing span is active, trace_id / span_id are attached so log
// entries correlate with the distributed trace.
func AccessLog(logger log.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)

		ctx := c.Request.Context()
		rid := ctxval.RequestIDFrom(ctx)
		// Prefer the matched route template (low cardinality, no user data)
		// over the raw URL path (which could contain control characters for
		// log injection). Fall back to "unmatched" for 404 routes.
		path := c.FullPath()
		if path == "" {
			path = "unmatched"
		}
		fields := []any{
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency", latency.String(),
			"client_ip", c.ClientIP(),
			"request_id", rid,
		}
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			fields = append(fields,
				"trace_id", sc.TraceID().String(),
				"span_id", sc.SpanID().String(),
			)
		}
		logger.Info("access", fields...)
	}
}
