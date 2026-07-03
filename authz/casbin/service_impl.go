package casbin

import (
	"context"
	"fmt"
)

// This file implements the Service interface methods on
// *Engine. Two patterns dominate:
//
//   - normalizeDomain on every domain input so storage uses a single
//     vocabulary (incoming "" → "*"). Reads use the same
//     normalisation so the reverse transform never matters.
//
//   - rejectGlobalAsTenant on every "InDomain" method so calls like
//     AddUserToRoleInDomain(ctx, u, r, "*") are an error, not a
//     silent global grant. Operators who genuinely want global use
//     the no-suffix variant (AddUserToRole).
//
//   - audit hook fires AFTER a successful mutation so no-op /
//     duplicate writes don't generate spurious events. Casbin's
//     AddPolicy returns (true, nil) on insert, (false, nil) on dup;
//     we treat the dup case as a no-op (no audit entry).
//     fireAudit() does an atomic.Pointer.Load under the hood so the
//     read is race-free against Close.
//
// Errors propagate verbatim from the Casbin SDK — wrapped with the
// method name + parameters for log readability when needed.

// AddUserToRole implements Service. The no-suffix global variants
// bypass *InDomain — those reject `"*"` as a tenant id, while the
// global writers always use `"*"` as the storage domain. Routing
// through the underlying Casbin primitive directly keeps the rule
// "operators never pass `*` as a tenant" enforceable on the
// *InDomain methods without breaking the global ones.
func (a *Engine) AddUserToRole(ctx context.Context, userID, role string) error {
	added, err := a.enforcer.AddRoleForUserInDomain(userID, role, globalDomain)
	if err != nil {
		return err
	}
	if added {
		a.fireAudit(ctx, "AddUserToRole", role, userID, globalDomain)
	}
	return nil
}

// RemoveUserFromRole implements Service.
func (a *Engine) RemoveUserFromRole(ctx context.Context, userID, role string) error {
	removed, err := a.enforcer.DeleteRoleForUserInDomain(userID, role, globalDomain)
	if err != nil {
		return err
	}
	if removed {
		a.fireAudit(ctx, "RemoveUserFromRole", role, userID, globalDomain)
	}
	return nil
}

// UserRoles implements Service.
func (a *Engine) UserRoles(ctx context.Context, userID string) ([]string, error) {
	return a.enforcer.GetRolesForUserInDomain(userID, globalDomain), nil
}

// AddUserToRoleInDomain implements Service.
func (a *Engine) AddUserToRoleInDomain(ctx context.Context, userID, role, domain string) error {
	if err := rejectGlobalAsTenant(domain, "AddUserToRoleInDomain"); err != nil {
		return err
	}
	dom := normalizeDomain(domain)
	added, err := a.enforcer.AddRoleForUserInDomain(userID, role, dom)
	if err != nil {
		return err
	}
	if added {
		a.fireAudit(ctx, "AddUserToRoleInDomain", role, userID, dom)
	}
	return nil
}

// RemoveUserFromRoleInDomain implements Service.
func (a *Engine) RemoveUserFromRoleInDomain(ctx context.Context, userID, role, domain string) error {
	if err := rejectGlobalAsTenant(domain, "RemoveUserFromRoleInDomain"); err != nil {
		return err
	}
	dom := normalizeDomain(domain)
	removed, err := a.enforcer.DeleteRoleForUserInDomain(userID, role, dom)
	if err != nil {
		return err
	}
	if removed {
		a.fireAudit(ctx, "RemoveUserFromRoleInDomain", role, userID, dom)
	}
	return nil
}

// UserRolesInDomain implements Service.
func (a *Engine) UserRolesInDomain(ctx context.Context, userID, domain string) ([]string, error) {
	if err := rejectGlobalAsTenant(domain, "UserRolesInDomain"); err != nil {
		return nil, err
	}
	return a.enforcer.GetRolesForUserInDomain(userID, normalizeDomain(domain)), nil
}

// DomainsForUser implements Service. Walks every g(...) row and
// extracts the third column (domain) where the first column matches
// the user. Casbin doesn't expose this directly so we filter
// GetGroupingPolicy ourselves.
func (a *Engine) DomainsForUser(ctx context.Context, userID string) ([]string, error) {
	policies, err := a.enforcer.GetGroupingPolicy()
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

// GrantRole implements Service. Writes a global role permission.
// Bypasses the *InDomain "no `*` as tenant" guard for the same
// reason as AddUserToRole.
func (a *Engine) GrantRole(ctx context.Context, role, obj, act string) error {
	added, err := a.enforcer.AddPolicy(role, globalDomain, obj, act)
	if err != nil {
		return err
	}
	if added {
		a.fireAudit(ctx, "GrantRole", role, obj, act)
	}
	return nil
}

// RevokeRole implements Service.
func (a *Engine) RevokeRole(ctx context.Context, role, obj, act string) error {
	removed, err := a.enforcer.RemovePolicy(role, globalDomain, obj, act)
	if err != nil {
		return err
	}
	if removed {
		a.fireAudit(ctx, "RevokeRole", role, obj, act)
	}
	return nil
}

// RolePermissions implements Service.
func (a *Engine) RolePermissions(ctx context.Context, role string) ([]Permission, error) {
	rows, err := a.enforcer.GetFilteredPolicy(0, role, globalDomain)
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

// GrantRoleInDomain implements Service.
func (a *Engine) GrantRoleInDomain(ctx context.Context, role, obj, act, domain string) error {
	if err := rejectGlobalAsTenant(domain, "GrantRoleInDomain"); err != nil {
		return err
	}
	dom := normalizeDomain(domain)
	added, err := a.enforcer.AddPolicy(role, dom, obj, act)
	if err != nil {
		return err
	}
	if added {
		a.fireAudit(ctx, "GrantRoleInDomain", role, obj, act)
	}
	return nil
}

// RevokeRoleInDomain implements Service.
func (a *Engine) RevokeRoleInDomain(ctx context.Context, role, obj, act, domain string) error {
	if err := rejectGlobalAsTenant(domain, "RevokeRoleInDomain"); err != nil {
		return err
	}
	dom := normalizeDomain(domain)
	removed, err := a.enforcer.RemovePolicy(role, dom, obj, act)
	if err != nil {
		return err
	}
	if removed {
		a.fireAudit(ctx, "RevokeRoleInDomain", role, obj, act)
	}
	return nil
}

// RolePermissionsInDomain implements Service.
func (a *Engine) RolePermissionsInDomain(ctx context.Context, role, domain string) ([]Permission, error) {
	if err := rejectGlobalAsTenant(domain, "RolePermissionsInDomain"); err != nil {
		return nil, err
	}
	dom := normalizeDomain(domain)
	rows, err := a.enforcer.GetFilteredPolicy(0, role, dom)
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

// GrantUser implements Service. Writes p(userID, "*", obj, act) so
// the matcher's r.sub == p.sub clause picks it up without a role
// binding. SPEC §7.7 v0.3.4 added that clause specifically so direct
// user grants work.
func (a *Engine) GrantUser(ctx context.Context, userID, obj, act string) error {
	added, err := a.enforcer.AddPolicy(userID, globalDomain, obj, act)
	if err != nil {
		return err
	}
	if added {
		a.fireAudit(ctx, "GrantUser", "(direct)", userID, obj+"/"+act)
	}
	return nil
}

// RevokeUser implements Service.
func (a *Engine) RevokeUser(ctx context.Context, userID, obj, act string) error {
	removed, err := a.enforcer.RemovePolicy(userID, globalDomain, obj, act)
	if err != nil {
		return err
	}
	if removed {
		a.fireAudit(ctx, "RevokeUser", "(direct)", userID, obj+"/"+act)
	}
	return nil
}

// HasPermission implements Service. Goes through Authorize so global
// queries get the same matcher (`p.dom == "*"` passthrough) without
// the *InDomain rejection of "*".
func (a *Engine) HasPermission(ctx context.Context, userID, obj, act string) (bool, error) {
	return a.Authorize(ctx, userID, obj, act)
}

// HasPermissionInDomain implements Service. The reject check happens
// here; AuthorizeInDomain already normalises the domain on its own,
// so we pass the raw input through.
func (a *Engine) HasPermissionInDomain(ctx context.Context, userID, obj, act, domain string) (bool, error) {
	if err := rejectGlobalAsTenant(domain, "HasPermissionInDomain"); err != nil {
		return false, err
	}
	return a.AuthorizeInDomain(ctx, userID, domain, obj, act)
}

// ReloadPolicy implements Service.
func (a *Engine) ReloadPolicy(ctx context.Context) error {
	return a.enforcer.LoadPolicy()
}
