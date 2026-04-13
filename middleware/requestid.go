package middleware

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/internal/ctxval"
)

const headerXRequestID = "X-Request-ID"

// RequestID generates or propagates a request ID via the X-Request-ID header.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader(headerXRequestID)
		if rid == "" {
			rid = generateID()
		}
		ctx := ctxval.WithRequestID(c.Request.Context(), rid)
		c.Request = c.Request.WithContext(ctx)
		c.Header(headerXRequestID, rid)
		c.Next()
	}
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
