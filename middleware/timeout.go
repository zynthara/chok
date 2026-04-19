package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/internal/ctxval"
)

// Timeout returns a middleware that injects a context deadline into each
// request. Handlers (and libraries they call — DB drivers, HTTP clients)
// that respect context cancellation will stop work when the deadline
// fires, and the middleware writes a 504 if no response was produced.
//
// A zero or negative duration disables the middleware (pass-through).
//
// This is a cooperative timeout: the handler runs synchronously on the
// same goroutine, so gin.Context is never accessed concurrently. If a
// handler ignores context cancellation (e.g. a pure CPU loop), it will
// block until it returns on its own. For hard process-level limits,
// configure http.Server.WriteTimeout instead.
func Timeout(d time.Duration) gin.HandlerFunc {
	if d <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)

		c.Next()

		// If the handler bailed out due to the deadline (or any other
		// reason) without writing a response, send 504.
		if ctx.Err() != nil && !c.Writer.Written() {
			c.Header("Content-Type", "application/json; charset=utf-8")
			c.Writer.WriteHeader(http.StatusGatewayTimeout)
			body := timeoutBodyFor(ctxval.RequestIDFrom(c.Request.Context()))
			c.Writer.Write(body) //nolint:errcheck
			c.Abort()
		}
	}
}

// timeoutBodyFor serialises the 504 envelope for a given request_id.
// Encoded on demand (rather than via a static []byte) so the body
// carries the same request_id field that other error responses use,
// letting log correlation work at the edge.
func timeoutBodyFor(requestID string) []byte {
	body := struct {
		Code      int    `json:"code"`
		Reason    string `json:"reason"`
		Message   string `json:"message"`
		RequestID string `json:"request_id,omitempty"`
	}{
		Code:      http.StatusGatewayTimeout,
		Reason:    "GatewayTimeout",
		Message:   "request timed out",
		RequestID: requestID,
	}
	b, err := json.Marshal(body)
	if err != nil {
		// Should be unreachable — fall back to the static form.
		return []byte(`{"code":504,"reason":"GatewayTimeout","message":"request timed out"}`)
	}
	return b
}
