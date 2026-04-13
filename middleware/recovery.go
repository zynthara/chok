package middleware

import (
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
// logged via the context logger if available.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				ctx := c.Request.Context()
				if l, ok := ctxval.LoggerFrom(ctx).(log.Logger); ok && l != nil {
					l.ErrorContext(ctx, "panic recovered",
						"panic", r,
						"stack", string(debug.Stack()),
					)
				}
				handler.WriteResponse(c, 0, nil, apierr.ErrInternal)
				c.Abort()
			}
		}()
		c.Next()
	}
}
