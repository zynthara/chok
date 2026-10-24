package chok

import (
	"context"
	"slices"
)

// Principal is the authenticated caller. identity's Authn middleware
// stores it in the request context; store's owner scope and application
// code read it with PrincipalFrom. It lives in the root package so the
// batteries that produce and consume it need not import each other.
type Principal struct {
	UserID string
	Email  string
	Roles  []string
	Extra  map[string]string
}

// HasRole reports whether the principal holds role.
func (p Principal) HasRole(role string) bool {
	return slices.Contains(p.Roles, role)
}

type principalKey struct{}

// WithPrincipal returns a context that carries p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored in ctx, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
