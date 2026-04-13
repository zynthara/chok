package store

import (
	"context"

	"gorm.io/gorm"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/db"
)

// defaultAdminRoles is the global default admin role list for OwnerScope
// auto-detection. Change via SetDefaultAdminRoles.
var defaultAdminRoles = []string{"admin"}

// SetDefaultAdminRoles sets the global admin role names used by auto-detected
// OwnerScope. Call once at startup before creating any Store.
func SetDefaultAdminRoles(roles ...string) {
	defaultAdminRoles = roles
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

		// Admin bypass.
		for _, r := range p.Roles {
			if _, isAdmin := adminSet[r]; isAdmin {
				return q, nil
			}
		}

		return q.Where("owner_id = ?", p.Subject), nil
	}
}

// fillOwner sets OwnerID from the context principal if the model
// implements db.OwnerAccessor and OwnerID is not already set.
func fillOwner[T db.Modeler](ctx context.Context, obj *T) {
	owned, ok := any(obj).(db.OwnerAccessor)
	if !ok {
		return
	}
	if owned.GetOwnerID() != "" {
		return
	}
	if p, ok := auth.PrincipalFrom(ctx); ok {
		owned.SetOwnerID(p.Subject)
	}
}
