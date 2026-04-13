package middleware

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/handler"
)

// TokenParser parses a token string and returns the subject and claims.
// The built-in jwt.Manager implements this interface (HS256).
// Users needing RS256/EdDSA/external IdP can provide their own implementation.
type TokenParser interface {
	Parse(token string) (subject string, claims map[string]any, err error)
}

// PrincipalResolver builds a full Principal from the JWT subject and claims.
// Optional: when nil, the middleware constructs a minimal Principal from subject
// and claims only. Receives claims so that roles/tenant can be read directly
// from the token without an extra database lookup.
type PrincipalResolver func(ctx context.Context, subject string, claims map[string]any) (auth.Principal, error)

// Authn creates a Bearer-token authentication middleware.
//
// parser: validates the token (built-in jwt.Manager or custom TokenParser).
// Panics if nil — configuration errors are caught at startup, not at request time.
// resolver: optional, enriches Principal from subject+claims.
func Authn(parser TokenParser, resolver PrincipalResolver) gin.HandlerFunc {
	if parser == nil {
		panic("middleware: Authn parser must not be nil")
	}
	return func(c *gin.Context) {
		tokenStr := extractBearer(c.GetHeader("Authorization"))
		if tokenStr == "" {
			handler.WriteResponse(c, 0, nil, apierr.ErrUnauthenticated)
			c.Abort()
			return
		}

		subject, claims, err := parser.Parse(tokenStr)
		if err != nil {
			handler.WriteResponse(c, 0, nil, apierr.ErrUnauthenticated)
			c.Abort()
			return
		}

		var p auth.Principal
		if resolver != nil {
			p, err = resolver(c.Request.Context(), subject, claims)
			if err != nil {
				handler.WriteResponse(c, 0, nil, apierr.ErrUnauthenticated)
				c.Abort()
				return
			}
		} else {
			p = auth.Principal{Subject: subject, Claims: claims}
		}

		ctx := auth.WithPrincipal(c.Request.Context(), p)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func extractBearer(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return parts[1]
}
