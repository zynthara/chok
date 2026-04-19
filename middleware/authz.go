package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/authz"
	"github.com/zynthara/chok/handler"
	"github.com/zynthara/chok/internal/ctxval"
	"github.com/zynthara/chok/log"
)

// Authz creates an authorization middleware.
//
// It extracts the Principal from the request context (set by Authn), then
// calls az.Authorize with subject=Principal.Subject, object=route pattern,
// action=HTTP method.
//
// Error semantics:
//   - Authorize returns error → infrastructure failure → log + 500.
//   - Authorize returns !allowed → policy denial → 403.
//
// Panics if az is nil (configuration error caught at startup).
func Authz(az authz.Authorizer) gin.HandlerFunc {
	if az == nil {
		panic("middleware: Authz authorizer must not be nil")
	}
	return func(c *gin.Context) {
		p, ok := auth.PrincipalFrom(c.Request.Context())
		if !ok {
			handler.WriteResponse(c, 0, nil, apierr.ErrUnauthenticated)
			c.Abort()
			return
		}

		ctx := c.Request.Context()
		object := c.FullPath()
		if object == "" {
			object = c.Request.URL.Path // fallback for unmatched routes
		}
		allowed, err := az.Authorize(ctx, p.Subject, object, c.Request.Method)
		if err != nil {
			// Infrastructure failure (policy engine down, DB error, etc.)
			// Log the cause so it's visible in observability; return 500.
			if l, ok := ctxval.LoggerFrom(ctx).(log.Logger); ok && l != nil {
				l.ErrorContext(ctx, "authorization error",
					"error", err,
					"subject", p.Subject,
					"object", c.FullPath(),
					"action", c.Request.Method,
				)
			}
			handler.WriteResponse(c, 0, nil, apierr.ErrInternal)
			c.Abort()
			return
		}
		if !allowed {
			handler.WriteResponse(c, 0, nil, apierr.ErrPermissionDenied)
			c.Abort()
			return
		}
		c.Next()
	}
}
