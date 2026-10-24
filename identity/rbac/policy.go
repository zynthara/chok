package rbac

import (
	"context"
	"errors"
	"fmt"
)

// PolicyService manages the policies: which roles a user holds, globally
// or in a domain (a tenant: a workspace, an organization), which
// permissions — an object and an action — a role or a user has, and the
// first administrator. Domain "" is the global scope; "*" is reserved for
// it and refused as a tenant. Changes reach the other replicas through
// the Watcher option and, with identity's audit log on, are recorded in
// it.
type PolicyService struct{ e *engine }

// Permission is an object and an action a role may perform on it. Object
// patterns follow casbin's keyMatch: "post.*" covers "post.read"; "*"
// covers everything.
type Permission struct {
	Object string
	Action string
}

// BootstrapAdmin makes userID an administrator: the role admin, holding
// every action on every object. It is idempotent — the first grant on a
// fresh database — and additive: it never revokes what an earlier run or
// an operator granted.
func (p *PolicyService) BootstrapAdmin(ctx context.Context, userID string) error {
	if userID == "" {
		return errors.New("rbac: BootstrapAdmin needs a user id")
	}
	if err := p.AddUserToRole(ctx, userID, adminRole); err != nil {
		return fmt.Errorf("rbac: bootstrap %s as %s: %w", userID, adminRole, err)
	}
	return p.e.grantRoleBatch(ctx, adminRole, []Permission{{Object: "*", Action: "*"}})
}

// adminRole is the role BootstrapAdmin binds.
const adminRole = "admin"

// AddUserToRole binds userID to role in the global scope.
func (p *PolicyService) AddUserToRole(ctx context.Context, userID, role string) error {
	added, err := p.e.enforcer.AddRoleForUserInDomain(userID, role, globalDomain)
	if err != nil {
		return err
	}
	if added {
		p.e.fireAudit(ctx, "AddUserToRole", role, userID, globalDomain)
	}
	return nil
}

// RemoveUserFromRole removes a global binding.
func (p *PolicyService) RemoveUserFromRole(ctx context.Context, userID, role string) error {
	removed, err := p.e.enforcer.DeleteRoleForUserInDomain(userID, role, globalDomain)
	if err != nil {
		return err
	}
	if removed {
		p.e.fireAudit(ctx, "RemoveUserFromRole", role, userID, globalDomain)
	}
	return nil
}

// UserRoles lists the roles bound to userID globally.
func (p *PolicyService) UserRoles(ctx context.Context, userID string) ([]string, error) {
	return p.e.enforcer.GetRolesForUserInDomain(userID, globalDomain), nil
}

// AddUserToRoleInDomain binds userID to role in domain.
func (p *PolicyService) AddUserToRoleInDomain(ctx context.Context, userID, role, domain string) error {
	if err := rejectGlobalAsTenant(domain, "AddUserToRoleInDomain"); err != nil {
		return err
	}
	dom := normalizeDomain(domain)
	added, err := p.e.enforcer.AddRoleForUserInDomain(userID, role, dom)
	if err != nil {
		return err
	}
	if added {
		p.e.fireAudit(ctx, "AddUserToRoleInDomain", role, userID, dom)
	}
	return nil
}

// RemoveUserFromRoleInDomain removes a binding in domain.
func (p *PolicyService) RemoveUserFromRoleInDomain(ctx context.Context, userID, role, domain string) error {
	if err := rejectGlobalAsTenant(domain, "RemoveUserFromRoleInDomain"); err != nil {
		return err
	}
	dom := normalizeDomain(domain)
	removed, err := p.e.enforcer.DeleteRoleForUserInDomain(userID, role, dom)
	if err != nil {
		return err
	}
	if removed {
		p.e.fireAudit(ctx, "RemoveUserFromRoleInDomain", role, userID, dom)
	}
	return nil
}

// UserRolesInDomain lists the roles bound to userID in domain.
func (p *PolicyService) UserRolesInDomain(ctx context.Context, userID, domain string) ([]string, error) {
	if err := rejectGlobalAsTenant(domain, "UserRolesInDomain"); err != nil {
		return nil, err
	}
	return p.e.enforcer.GetRolesForUserInDomain(userID, normalizeDomain(domain)), nil
}

// DomainsForUser lists the domains where userID holds a role, "*" for a
// global one. Casbin does not expose it, so it filters the grouping
// policies.
func (p *PolicyService) DomainsForUser(ctx context.Context, userID string) ([]string, error) {
	policies, err := p.e.enforcer.GetGroupingPolicy()
	if err != nil {
		return nil, fmt.Errorf("DomainsForUser: %w", err)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, row := range policies {
		// row format: [user, role, domain]
		if len(row) < 3 || row[0] != userID {
			continue
		}
		if !seen[row[2]] {
			seen[row[2]] = true
			out = append(out, row[2])
		}
	}
	return out, nil
}

// GrantRole gives role the action on obj in the global scope, which
// every domain inherits.
func (p *PolicyService) GrantRole(ctx context.Context, role, obj, act string) error {
	added, err := p.e.enforcer.AddPolicy(role, globalDomain, obj, act)
	if err != nil {
		return err
	}
	if added {
		p.e.fireAudit(ctx, "GrantRole", role, obj, act)
	}
	return nil
}

// RevokeRole takes a global permission back.
func (p *PolicyService) RevokeRole(ctx context.Context, role, obj, act string) error {
	removed, err := p.e.enforcer.RemovePolicy(role, globalDomain, obj, act)
	if err != nil {
		return err
	}
	if removed {
		p.e.fireAudit(ctx, "RevokeRole", role, obj, act)
	}
	return nil
}

// RolePermissions lists role's global permissions.
func (p *PolicyService) RolePermissions(ctx context.Context, role string) ([]Permission, error) {
	rows, err := p.e.enforcer.GetFilteredPolicy(0, role, globalDomain)
	if err != nil {
		return nil, fmt.Errorf("RolePermissions: %w", err)
	}
	perms := make([]Permission, 0, len(rows))
	for _, r := range rows {
		if len(r) < 4 {
			continue
		}
		perms = append(perms, Permission{Object: r[2], Action: r[3]})
	}
	return perms, nil
}

// GrantRoleInDomain gives role the action on obj in domain.
func (p *PolicyService) GrantRoleInDomain(ctx context.Context, role, obj, act, domain string) error {
	if err := rejectGlobalAsTenant(domain, "GrantRoleInDomain"); err != nil {
		return err
	}
	dom := normalizeDomain(domain)
	added, err := p.e.enforcer.AddPolicy(role, dom, obj, act)
	if err != nil {
		return err
	}
	if added {
		p.e.fireAudit(ctx, "GrantRoleInDomain", role, obj, act)
	}
	return nil
}

// RevokeRoleInDomain takes a permission in domain back.
func (p *PolicyService) RevokeRoleInDomain(ctx context.Context, role, obj, act, domain string) error {
	if err := rejectGlobalAsTenant(domain, "RevokeRoleInDomain"); err != nil {
		return err
	}
	dom := normalizeDomain(domain)
	removed, err := p.e.enforcer.RemovePolicy(role, dom, obj, act)
	if err != nil {
		return err
	}
	if removed {
		p.e.fireAudit(ctx, "RevokeRoleInDomain", role, obj, act)
	}
	return nil
}

// RolePermissionsInDomain lists role's permissions in domain.
func (p *PolicyService) RolePermissionsInDomain(ctx context.Context, role, domain string) ([]Permission, error) {
	if err := rejectGlobalAsTenant(domain, "RolePermissionsInDomain"); err != nil {
		return nil, err
	}
	dom := normalizeDomain(domain)
	rows, err := p.e.enforcer.GetFilteredPolicy(0, role, dom)
	if err != nil {
		return nil, fmt.Errorf("RolePermissionsInDomain: %w", err)
	}
	perms := make([]Permission, 0, len(rows))
	for _, r := range rows {
		// r = [sub, dom, obj, act]
		if len(r) < 4 {
			continue
		}
		perms = append(perms, Permission{Object: r[2], Action: r[3]})
	}
	return perms, nil
}

// GrantUser gives userID the action on obj directly, without a role,
// for a one-off privilege: the model matches a policy whose subject is
// the user.
func (p *PolicyService) GrantUser(ctx context.Context, userID, obj, act string) error {
	added, err := p.e.enforcer.AddPolicy(userID, globalDomain, obj, act)
	if err != nil {
		return err
	}
	if added {
		p.e.fireAudit(ctx, "GrantUser", "(direct)", userID, obj+"/"+act)
	}
	return nil
}

// RevokeUser takes a direct grant back.
func (p *PolicyService) RevokeUser(ctx context.Context, userID, obj, act string) error {
	removed, err := p.e.enforcer.RemovePolicy(userID, globalDomain, obj, act)
	if err != nil {
		return err
	}
	if removed {
		p.e.fireAudit(ctx, "RevokeUser", "(direct)", userID, obj+"/"+act)
	}
	return nil
}

// HasPermission decides as Require does: userID's roles and grants,
// global ones included.
func (p *PolicyService) HasPermission(ctx context.Context, userID, obj, act string) (bool, error) {
	return p.e.authorize(userID, obj, act)
}

// HasPermissionInDomain decides as RequireInDomain does.
func (p *PolicyService) HasPermissionInDomain(ctx context.Context, userID, obj, act, domain string) (bool, error) {
	if err := rejectGlobalAsTenant(domain, "HasPermissionInDomain"); err != nil {
		return false, err
	}
	return p.e.authorizeInDomain(userID, domain, obj, act)
}

// ReloadPolicy reads the policies from the database again — after a
// change another replica made while the watcher was down, say.
func (p *PolicyService) ReloadPolicy(ctx context.Context) error {
	return p.e.enforcer.LoadPolicy()
}

// normalizeDomain maps the domain vocabulary onto storage: "" and "*"
// are the global scope, stored as "*"; anything else is a tenant.
//
// Every writer and the in-domain decision funnel through it, so storage
// holds one sentinel for global: with "" and "*" side by side, policies
// would not see each other (the matcher compares domains literally).
func normalizeDomain(d string) string {
	if d == "" {
		return globalDomain
	}
	return d
}

// rejectGlobalAsTenant refuses "*" as a tenant id: a call such as
// AddUserToRoleInDomain(ctx, u, r, "*"), meant for "this tenant", gets an
// error instead of a silent global grant.
func rejectGlobalAsTenant(domain, methodName string) error {
	if domain == globalDomain {
		return fmt.Errorf("rbac: tenant domain cannot be %q (reserved for global); use %s for global",
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
