package store

import (
	"context"
	"sync"

	"gorm.io/gorm"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/db"
)

// defaultAdminRoles is the global default admin role list for OwnerScope
// auto-detection. Protected by defaultAdminMu.
var (
	defaultAdminRoles = []string{"admin"}
	defaultAdminMu    sync.RWMutex
)

// SetDefaultAdminRoles sets the global admin role names used by auto-detected
// OwnerScope. Call once at startup before creating any Store.
//
// Deprecated: Global admin roles are shared across all Store instances.
// Prefer per-Store admin roles via OwnerScope("admin", "superadmin") passed
// to WithScope at Store construction. This function will be removed in a
// future release.
func SetDefaultAdminRoles(roles ...string) {
	defaultAdminMu.Lock()
	defaultAdminRoles = roles
	defaultAdminMu.Unlock()
}

// getDefaultAdminRoles returns a copy of the global default admin roles.
func getDefaultAdminRoles() []string {
	defaultAdminMu.RLock()
	roles := make([]string, len(defaultAdminRoles))
	copy(roles, defaultAdminRoles)
	defaultAdminMu.RUnlock()
	return roles
}

// OwnerScope returns a ScopeFunc that restricts queries to the current
// principal's own records (WHERE owner_id = subject). Principals holding
// any of adminRoles bypass the filter and see all records.
//
// Unauthenticated requests fail closed with ErrUnauthenticated.
//
// Usage:
//
//	store.New[Product](gdb, logger,
//	    store.WithScope(store.OwnerScope("admin")),
//	)
func OwnerScope(adminRoles ...string) ScopeFunc {
	adminSet := make(map[string]struct{}, len(adminRoles))
	for _, r := range adminRoles {
		adminSet[r] = struct{}{}
	}

	return func(ctx context.Context, q *gorm.DB) (*gorm.DB, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, apierr.ErrUnauthenticated
		}

		// Fail-closed: empty subject must not produce a no-op WHERE.
		if p.Subject == "" {
			return nil, apierr.ErrUnauthenticated
		}

		// Admin bypass.
		for _, r := range p.Roles {
			if _, isAdmin := adminSet[r]; isAdmin {
				return q, nil
			}
		}

		return q.Where("owner_id = ?", p.Subject), nil
	}
}

// fillOwner enforces OwnerID on Owned models using the authenticated
// principal's Subject. A caller-provided OwnerID is IGNORED unless the
// principal holds one of the admin roles configured via SetDefaultAdminRoles
// (the escape hatch for administrative imports / cross-user writes).
//
// Without this enforcement, an authenticated user could spoof OwnerID in
// the request body and create rows attributed to another user.
//
// Semantics when the context carries no principal:
//   - strict=true  → return apierr.ErrUnauthenticated (fail-closed). This is
//     the safer default for HTTP code paths that might miss Authn middleware.
//   - strict=false → keep legacy "no-op" behaviour so background jobs and
//     tests can Create rows with a preset OwnerID without a principal.
//
// Non-Owned models bypass the check entirely and always return nil.
func fillOwner[T db.Modeler](ctx context.Context, obj *T, strict bool) error {
	owned, ok := any(obj).(db.OwnerAccessor)
	if !ok {
		return nil
	}
	p, hasPrincipal := auth.PrincipalFrom(ctx)
	if !hasPrincipal {
		if strict {
			return apierr.ErrUnauthenticated
		}
		return nil
	}
	if isAdminPrincipal(p) {
		// Admin may set OwnerID explicitly; only auto-fill when empty.
		if owned.GetOwnerID() == "" {
			owned.SetOwnerID(p.Subject)
		}
		return nil
	}
	// Non-admin: unconditionally overwrite to principal's Subject.
	owned.SetOwnerID(p.Subject)
	return nil
}

// isAdminPrincipal reports whether the principal holds any of the global
// default admin roles. Returns false when no admin roles are configured.
func isAdminPrincipal(p auth.Principal) bool {
	admins := getDefaultAdminRoles()
	if len(admins) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(admins))
	for _, r := range admins {
		set[r] = struct{}{}
	}
	for _, r := range p.Roles {
		if _, ok := set[r]; ok {
			return true
		}
	}
	return false
}
