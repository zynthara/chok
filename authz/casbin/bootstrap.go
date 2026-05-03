package casbin

import (
	"context"
	"errors"
	"fmt"
)

// BootstrapConfig configures the idempotent admin seeding helper.
// Bootstrap binds a single user as the global admin and grants that
// admin role full permissions (`*` on `*`). Re-running Bootstrap with
// the same config is a no-op — Service writes use Casbin's
// "AddPolicy returns false on duplicate" semantics underneath, which
// the Service implementation reports as success without re-firing
// the audit hook.
//
// AdminRole defaults to "admin" when empty.
// AdminPerms defaults to a single (`*`, `*`) wildcard permission;
// override only when the deployment wants a narrower bootstrap
// admin (e.g. `admin` can only read, not write, until a real
// administrator assigns more permissions).
type BootstrapConfig struct {
	// AdminUserID is the chok User RID to bind as admin. Required.
	// Empty AdminUserID returns an error so a misconfigured
	// chok.yaml's missing bootstrap_admin_user_id surfaces at
	// startup instead of silently skipping admin seeding.
	AdminUserID string

	// AdminRole names the role bound to AdminUserID. Default "admin".
	AdminRole string

	// AdminPerms lists the (object, action) permissions granted to
	// AdminRole. Default [{Object: "*", Action: "*"}].
	AdminPerms []Permission
}

// Bootstrap is the canonical idempotent admin-seeding helper.
// chok's autoregister calls it after AuthzComponent.Init when
// chok.yaml provides authz.casbin.bootstrap_admin_user_id; setup-
// driven users invoke it themselves from an a.On(EventAfterStart, ...)
// hook with their own BootstrapConfig.
//
// The implementation walks Service writes that are themselves no-op
// on duplicate (Casbin's storage layer returns affected=false when
// the same g/p tuple already exists). Bootstrap therefore re-runs
// safely on every startup — useful when the AdminUserID changes or
// when an operator widens AdminPerms.
func Bootstrap(ctx context.Context, svc Service, cfg BootstrapConfig) error {
	if svc == nil {
		return errors.New("authz/casbin Bootstrap: nil Service")
	}
	if cfg.AdminUserID == "" {
		return errors.New("authz/casbin Bootstrap: AdminUserID is required")
	}
	role := cfg.AdminRole
	if role == "" {
		role = "admin"
	}
	perms := cfg.AdminPerms
	if len(perms) == 0 {
		perms = []Permission{{Object: "*", Action: "*"}}
	}

	if err := svc.AddUserToRole(ctx, cfg.AdminUserID, role); err != nil {
		return fmt.Errorf("authz/casbin Bootstrap AddUserToRole(%s, %s): %w", cfg.AdminUserID, role, err)
	}
	for _, p := range perms {
		if err := svc.GrantRole(ctx, role, p.Object, p.Action); err != nil {
			return fmt.Errorf("authz/casbin Bootstrap GrantRole(%s, %s, %s): %w", role, p.Object, p.Action, err)
		}
	}
	return nil
}
