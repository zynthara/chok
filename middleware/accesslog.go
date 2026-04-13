package middleware

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/internal/ctxval"
	"github.com/zynthara/chok/log"
)

// AccessLog logs request method, path, status, and latency.
func AccessLog(logger log.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)

		rid := ctxval.RequestIDFrom(c.Request.Context())
		logger.Info("access",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latency", latency.String(),
			"client_ip", c.ClientIP(),
			"request_id", rid,
		)
	}
}
