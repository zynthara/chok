package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/handler"
)

// Timeout returns a middleware that injects a context deadline into each
// request. Handlers (and libraries they call — DB drivers, HTTP clients)
// that respect context cancellation will stop work when the deadline
// fires, and the middleware writes a 504 if no response was produced.
//
// A zero or negative duration disables the middleware (pass-through).
//
// This is a cooperative timeout: the handler runs synchronously on the
// same goroutine, so the ResponseWriter is never accessed concurrently.
// If a handler ignores context cancellation (e.g. a pure CPU loop), it
// will block until it returns on its own. For hard process-level limits,
// configure http.Server.WriteTimeout instead.
//
// v1 carried extra "did gin's double context lose the deadline" gymnastics;
// with the single context.Context those are gone (SPEC §4.1) — derive,
// serve, and backstop with the written-tracking check.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	if d <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			r = r.WithContext(ctx)

			next.ServeHTTP(w, r)

			// If the handler bailed out due to the deadline (or any other
			// reason) without writing a response, send 504. The envelope
			// matches every other error response (code/reason/message/
			// request_id) — apierr.ErrGatewayTimeout is the v1 body.
			if ctx.Err() != nil && !responseWritten(w) {
				handler.WriteResponse(w, r, 0, nil, apierr.ErrGatewayTimeout)
			}
		})
	}
}
