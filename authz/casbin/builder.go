package casbin

import (
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/zynthara/chok/authz"
	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/parts"
)

// Builder returns a parts.AuthzBuilder that constructs a
// Casbin-backed authz.Authorizer + Service + io.Closer wired against
// chok's component graph.
//
// Hard dependency: "db" — chok's bundled adapter (adapter.go)
// persists Casbin policies in the same database as the application's
// domain models, riding whichever GORM driver the operator already
// configured. Builder fails when no DBComponent is present, which
// parts.AuthzComponent's WithDependencies("db") declaration enforces
// at dependency-planning time.
//
// Optional dependency: "redis" — when Options.RedisWatcher is true
// the Builder is meant to pull the existing RedisComponent and
// attach a Casbin Watcher so policy edits broadcast to peer
// instances. The integration is stubbed for Phase 6; an explicit
// fail-fast error keeps yaml writers from thinking the flag works.
//
// Optional dependency: "audit" — same shape; stubbed until the
// audit component lands.
//
// Why we don't use github.com/casbin/gorm-adapter/v3: that library's
// init code blank-imports gorm.io/driver/postgres and
// gorm.io/driver/sqlserver, which transitively pulls jackc/pgx/v5 +
// microsoft/go-mssqldb + glebarez/sqlite + modernc/sqlite. On
// darwin/arm64 stripped, those drivers cost +8.72 MB above the
// underlying Casbin runtime. chok's bundled adapter (adapter.go)
// reuses whichever GORM driver the application already configured;
// nothing more, nothing extra.
func Builder(opts Options) parts.AuthzBuilder {
	return func(k component.Kernel) (authz.Authorizer, error) {
		gdb, err := dbFromKernel(k)
		if err != nil {
			return nil, fmt.Errorf("authz/casbin: %w", err)
		}

		adapter, err := newGormAdapter(gdb)
		if err != nil {
			return nil, fmt.Errorf("authz/casbin: %w", err)
		}

		auth, err := newAuthorizer(opts.modelOrDefault(), adapter)
		if err != nil {
			return nil, err
		}

		// Watcher and audit integrations land in follow-up PRs; until
		// then, surface "you set the flag but nothing will fire" as a
		// startup error rather than letting policy changes silently
		// stay local.
		if opts.RedisWatcher {
			return nil, errors.New("authz/casbin: RedisWatcher=true requires the redis-watcher integration which is not yet shipped — disable in chok.yaml or await the follow-up PR")
		}
		if opts.AuditEnabled {
			return nil, errors.New("authz/casbin: AuditEnabled=true requires the audit component which is not yet shipped — disable in chok.yaml or await the follow-up PR")
		}

		return auth, nil
	}
}

// dbFromKernel pulls *gorm.DB out of chok's DBComponent. Returns a
// structured error when the kernel doesn't carry one —
// AuthzComponent.WithDependencies("db") catches this at dependency-
// planning time, but the defence-in-depth check here surfaces an
// explicit message when WithDependencies was forgotten.
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
