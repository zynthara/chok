package casbin

import (
	"context"
	"fmt"
)

// Service is Casbin's runtime policy management surface, returned by
// Authorizer-side downcasts in setup code:
//
//	az := authzComp.Authorizer()
//	svc := az.(casbin.Service)
//	svc.AddUserToRoleInDomain(ctx, "usr_alice", "admin", "ws_abc")
//
// The interface provides global / domain-scoped pairs for role
// assignment and role permissioning, plus a direct-user grant path
// (GrantUser) for non-role scenarios. Domain="" inputs are
// normalised to "*" by every method; passing "*" as a tenant id
// (vs. the global sentinel) is rejected with a structured error.
//
// All methods take ctx for cancellation/tracing propagation but
// none currently use it — the underlying Casbin SDK is synchronous.
// We keep the parameter for forward compatibility.
type Service interface {
	// ── Role binding (global / single-tenant) ──────────────
	// AddUserToRole binds a user to a role globally (domain="*").
	// Equivalent to AddUserToRoleInDomain with empty domain.
	AddUserToRole(ctx context.Context, userID, role string) error
	// RemoveUserFromRole removes a global role binding.
	RemoveUserFromRole(ctx context.Context, userID, role string) error
	// UserRoles lists all roles bound to user globally (domain "*").
	UserRoles(ctx context.Context, userID string) ([]string, error)

	// ── Role binding (tenant-scoped) ─────────────────────────
	AddUserToRoleInDomain(ctx context.Context, userID, role, domain string) error
	RemoveUserFromRoleInDomain(ctx context.Context, userID, role, domain string) error
	UserRolesInDomain(ctx context.Context, userID, domain string) ([]string, error)
	// DomainsForUser lists every domain in which the user has at
	// least one role binding. "*" appears here when the user has a
	// global role.
	DomainsForUser(ctx context.Context, userID string) ([]string, error)

	// ── Role → permission (global) ─────────────────────────
	GrantRole(ctx context.Context, role, obj, act string) error
	RevokeRole(ctx context.Context, role, obj, act string) error
	RolePermissions(ctx context.Context, role string) ([]Permission, error)

	// ── Role → permission (tenant-scoped) ────────────────────
	GrantRoleInDomain(ctx context.Context, role, obj, act, domain string) error
	RevokeRoleInDomain(ctx context.Context, role, obj, act, domain string) error
	RolePermissionsInDomain(ctx context.Context, role, domain string) ([]Permission, error)

	// ── Direct user grants (no role mediation) ─────────────
	// GrantUser writes p(userID, "*", obj, act) so the userID
	// matches Casbin's matcher via the r.sub == p.sub clause —
	// useful for one-off privileges that don't fit a role.
	GrantUser(ctx context.Context, userID, obj, act string) error
	RevokeUser(ctx context.Context, userID, obj, act string) error

	// ── Queries ────────────────────────────────────────────
	HasPermission(ctx context.Context, userID, obj, act string) (bool, error)
	HasPermissionInDomain(ctx context.Context, userID, obj, act, domain string) (bool, error)

	// ReloadPolicy re-reads policies from the adapter (e.g. after a
	// peer instance committed changes and the local Watcher missed
	// the broadcast). Production deployments rarely need to call
	// this directly when RedisWatcher is enabled.
	ReloadPolicy(ctx context.Context) error
}

// Permission is the (object, action) pair attached to a role.
type Permission struct {
	Object string
	Action string
}

// normalizeDomain implements the SPEC §7.7 v0.3.4 vocabulary:
//
//	"" → "*" (API friendly alias for global)
//	"*" → "*" (literal global sentinel)
//	other → other (tenant id)
//
// ALL Service writers + AuthorizeInDomain must funnel through this
// before touching Casbin so the storage row uses a single sentinel
// for global. Otherwise "" and "*" coexist and policies become
// invisible to each other (matcher checks `r.dom == p.dom` literally).
func normalizeDomain(d string) string {
	if d == "" {
		return globalDomain
	}
	return d
}

// rejectGlobalAsTenant prevents callers from accidentally using "*"
// (the global sentinel) as a tenant id. SPEC §7.7 v0.3.4: a
// deployment that creates `ws_*` is fine; a deployment that calls
// `AddUserToRoleInDomain(ctx, u, r, "*")` because they think "*"
// means "this tenant" gets a structured error rather than a silent
// global grant.
func rejectGlobalAsTenant(domain, methodName string) error {
	if domain == globalDomain {
		return fmt.Errorf("authz/casbin: tenant domain cannot be %q (reserved for global); use %s for global",
			globalDomain, globalAlternative(methodName))
	}
	return nil
}

// globalAlternative names the global-equivalent method so the error
// from rejectGlobalAsTenant points to the fix.
func globalAlternative(methodName string) string {
	switch methodName {
	case "AddUserToRoleInDomain":
		return "AddUserToRole"
	case "RemoveUserFromRoleInDomain":
		return "RemoveUserFromRole"
	case "UserRolesInDomain":
		return "UserRoles"
	case "GrantRoleInDomain":
		return "GrantRole"
	case "RevokeRoleInDomain":
		return "RevokeRole"
	case "RolePermissionsInDomain":
		return "RolePermissions"
	case "HasPermissionInDomain":
		return "HasPermission"
	default:
		return "the no-domain variant of this method"
	}
}

// globalDomain is the literal Casbin policy value used for
// global-scope rows. Both g(...) and p(...) tuples store it in their
// domain column so the matcher's `p.dom == "*"` clause picks it up.
const globalDomain = "*"
