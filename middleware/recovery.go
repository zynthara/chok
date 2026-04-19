package middleware

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/handler"
	"github.com/zynthara/chok/internal/ctxval"
	"github.com/zynthara/chok/log"
)

// Recovery returns a middleware that recovers from panics and returns 500.
// The response uses handler.WriteResponse for format consistency with normal
// errors (code/reason/message/request_id). The panic and stack trace are
// logged via the context logger if available; if not (e.g. the panic fired
// before the Logger middleware ran), the trace is written to stderr so the
// incident is never silently swallowed.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				ctx := c.Request.Context()
				// Cap stack length. Go stacks can balloon into hundreds of
				// KB under panic storms (many goroutines, deep recursion)
				// and uncapped logging floods slog pipelines and disk.
				const maxStack = 8 << 10
				stackBytes := debug.Stack()
				if len(stackBytes) > maxStack {
					stackBytes = stackBytes[:maxStack]
				}
				stack := string(stackBytes)
				if l, ok := ctxval.LoggerFrom(ctx).(log.Logger); ok && l != nil {
					l.ErrorContext(ctx, "panic recovered",
						"panic", r,
						"stack", stack,
					)
				} else {
					// No ctx logger — Logger middleware hasn't run yet
					// (panic in an earlier middleware). Fall back to
					// stderr so the panic is still visible; silent
					// swallow would leave operators chasing phantoms.
					fmt.Fprintf(os.Stderr, "[chok recovery] panic: %v\n%s\n", r, stack)
				}
				if !c.Writer.Written() {
					handler.WriteResponse(c, 0, nil, apierr.ErrInternal)
				}
				c.Abort()
			}
		}()
		c.Next()
	}
}
