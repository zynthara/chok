package middleware

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/internal/ctxval"
	"github.com/zynthara/chok/log"
)

// Logger injects the given logger into the request context.
// handler.HandleRequest retrieves it via ctxval for error logging.
func Logger(l log.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := ctxval.WithLogger(c.Request.Context(), l)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// RequestIDFrom extracts the request ID from context (user-facing helper).
func RequestIDFrom(ctx context.Context) string {
	return ctxval.RequestIDFrom(ctx)
}

// LoggerFrom extracts the logger from context (user-facing helper).
func LoggerFrom(ctx context.Context) log.Logger {
	if l, ok := ctxval.LoggerFrom(ctx).(log.Logger); ok {
		return l
	}
	return nil
}
