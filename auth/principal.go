// Package auth provides authentication primitives: identity context,
// password hashing, and JWT token management (in the jwt sub-package).
package auth

import "context"

// Principal represents an authenticated request subject.
type Principal struct {
	Subject string         // Primary identifier (e.g. userID, RID).
	Name    string         // Display name (optional).
	Roles   []string       // Role list (optional; simple RBAC without external authorizer).
	Claims  map[string]any // Extension fields from the token (optional).
}

type principalKey struct{}

// WithPrincipal stores a Principal in the context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom retrieves the Principal from the context.
// Returns false if no Principal is present (unauthenticated request).
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// HasRole reports whether the principal has the given role.
func (p Principal) HasRole(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}
