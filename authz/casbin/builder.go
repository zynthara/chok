package casbin

import (
	"errors"
	"fmt"

	gormadapter "github.com/casbin/gorm-adapter/v3"
	"gorm.io/gorm"

	"github.com/zynthara/chok/authz"
	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/parts"
)

// Builder returns a parts.AuthzBuilder that constructs a
// Casbin-backed authz.Authorizer + Service + io.Closer wired against
// chok's component graph.
//
// Hard dependency: "db" — the gorm-adapter persists Casbin policies
// in the same database as chok's domain models. Builder fails when
// no DBComponent is present, which the parts.AuthzComponent's
// WithDependencies("db") declaration enforces at startup time.
//
// Optional dependency: "redis" — when Options.RedisWatcher is true
// the Builder pulls the existing RedisComponent and attaches a
// Casbin Watcher so policy edits broadcast to peer instances.
// Missing redis with RedisWatcher=true is a fatal Build error so the
// operator notices the misconfig at startup, not after a policy
// change silently fails to propagate.
//
// Optional dependency: "audit" — left as a stub in Phase 6;
// Options.AuditEnabled toggles it for forward-compat once an
// AuditComponent ships.
func Builder(opts Options) parts.AuthzBuilder {
	return func(k component.Kernel) (authz.Authorizer, error) {
		// Pull *gorm.DB from chok's DBComponent. We resolve via the
		// Kernel rather than holding the DB directly so Reload-style
		// swap-of-DB future work doesn't require Builder rewrites.
		gdb, err := dbFromKernel(k)
		if err != nil {
			return nil, fmt.Errorf("authz/casbin: %w", err)
		}

		// gorm-adapter creates the casbin_rule table on first use.
		adapter, err := gormadapter.NewAdapterByDBWithCustomTable(gdb, &gormadapter.CasbinRule{})
		if err != nil {
			return nil, fmt.Errorf("authz/casbin: gorm-adapter: %w", err)
		}

		auth, err := newAuthorizer(opts.modelOrDefault(), adapter)
		if err != nil {
			return nil, err
		}

		// Watcher is optional — wire only when explicitly enabled.
		// Phase 6 ships without a default Redis watcher implementation
		// because Casbin's redis-watcher pulls a separate dep tree we
		// haven't approved yet; the integration point is the Watcher
		// field on *casbinAuthorizer and Builder is ready to attach
		// one when a future PR brings it in.
		if opts.RedisWatcher {
			return nil, errors.New("authz/casbin: RedisWatcher=true requires the redis-watcher integration which is not yet shipped — disable in chok.yaml or await the follow-up PR")
		}

		// Audit hook is similarly stubbed: enable-time validation for
		// forward-compat without wiring a no-op callback. When Audit
		// component ships, Builder will attach auth.withAuditHook here.
		if opts.AuditEnabled {
			return nil, errors.New("authz/casbin: AuditEnabled=true requires the audit component which is not yet shipped — disable in chok.yaml or await the follow-up PR")
		}

		return auth, nil
	}
}

// dbFromKernel pulls *gorm.DB out of chok's DBComponent. Returns a
// structured error when the kernel doesn't carry one — chok's
// AuthzComponent.WithDependencies("db") declaration catches this at
// dependency-planning time, but a defence-in-depth check here makes
// the error message explicit when WithDependencies was forgotten.
func dbFromKernel(k component.Kernel) (*gorm.DB, error) {
	c := k.Get("db")
	if c == nil {
		return nil, errors.New("DBComponent not registered (declare AuthzComponent.WithDependencies(\"db\"))")
	}
	dbc, ok := c.(*parts.DBComponent)
	if !ok {
		return nil, fmt.Errorf("expected *parts.DBComponent, got %T", c)
	}
	if dbc == nil {
		return nil, errors.New("DBComponent registered as nil pointer")
	}
	gdb := dbc.DB()
	if gdb == nil {
		return nil, errors.New("DBComponent.DB() returned nil — DB Init may have failed")
	}
	return gdb, nil
}
