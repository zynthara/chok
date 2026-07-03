// Package middleware is chok's http.Handler decorator set — the v2
// (stdlib) rewrite of the gin middleware stack. Every constructor
// returns a func(http.Handler) http.Handler (kernel.Middleware is an
// alias of that shape); gin's c.Next/c.Abort onion becomes plain
// wrapping: post-processing goes after next.ServeHTTP, aborting means
// writing the response and not calling next (SPEC §4.2 item 2).
package middleware

import (
	"fmt"
	"net/http"
	"os"
	"runtime/debug"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/internal/ctxval"
	"github.com/zynthara/chok/v2/log"
)

// Recovery returns a middleware that recovers from panics and returns 500.
// The response uses handler.WriteResponse for format consistency with normal
// errors (code/reason/message/request_id). The panic and stack trace are
// logged via the fallback logger (or the context logger when the panic
// happened deep enough for one to exist on this layer's request); when
// neither is available the trace goes to stderr so the incident is never
// silently swallowed.
//
// Recovery sits outermost, ahead of RequestID — in the stdlib onion the
// inner layers derive new request copies, so at recover time this layer's
// context has no request ID. The correlation ID is taken from the
// X-Request-ID response header (set by RequestID before user code runs)
// and stamped back into the context handed to WriteResponse, preserving
// the v1 behaviour that panic envelopes and panic logs carry request_id.
func Recovery(fallback log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// Cap stack length. Go stacks can balloon into hundreds of
					// KB under panic storms (many goroutines, deep recursion)
					// and uncapped logging floods slog pipelines and disk.
					const maxStack = 8 << 10
					stackBytes := debug.Stack()
					if len(stackBytes) > maxStack {
						stackBytes = stackBytes[:maxStack]
					}
					stack := string(stackBytes)

					ctx := r.Context()
					rid := ctxval.RequestIDFrom(ctx)
					if rid == "" {
						rid = w.Header().Get("X-Request-ID")
					}

					logger := fallback
					if l, ok := ctxval.LoggerFrom(ctx).(log.Logger); ok && l != nil {
						logger = l
					}
					if logger != nil {
						logger.ErrorContext(ctx, "panic recovered",
							"panic", rec,
							"request_id", rid,
							"stack", stack,
						)
					} else {
						// No logger anywhere — fall back to stderr; silent
						// swallow would leave operators chasing phantoms.
						fmt.Fprintf(os.Stderr, "[chok recovery] panic: %v\n%s\n", rec, stack)
					}

					if rid != "" {
						r = r.WithContext(ctxval.WithRequestID(ctx, rid))
					}
					handler.WriteResponse(w, r, 0, nil, apierr.ErrInternal)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
