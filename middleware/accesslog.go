package middleware

import (
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/zynthara/chok/v2/internal/ctxval"
	"github.com/zynthara/chok/v2/log"
)

// AccessLog logs request method, path, status, and latency.
// When a tracing span is active, trace_id / span_id are attached so log
// entries correlate with the distributed trace.
//
// The path field is the matched route template (low cardinality, no
// user data — raw URLs could carry control characters for log
// injection), read from the route-pattern carrier after the handler
// returns; unmatched requests log "unmatched". Requests that panic
// produce no access line (v1 parity: the entry after c.Next was
// skipped on unwind; Recovery owns the incident log).
func AccessLog(logger log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			latency := time.Since(start)

			ctx := r.Context()
			path := ctxval.RoutePatternFrom(ctx)
			if path == "" {
				path = "unmatched"
			}
			fields := []any{
				"method", r.Method,
				"path", path,
				"status", statusOf(w),
				"latency", latency.String(),
				"client_ip", ctxval.ClientIPFrom(ctx),
				"request_id", ctxval.RequestIDFrom(ctx),
			}
			if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
				fields = append(fields,
					"trace_id", sc.TraceID().String(),
					"span_id", sc.SpanID().String(),
				)
			}
			logger.Info("access", fields...)
		})
	}
}
