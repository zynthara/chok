package casbin

import (
	"context"
	"errors"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
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
// the Builder pulls the existing RedisComponent and attaches a
// Casbin Watcher (watcher.go) so policy edits broadcast to peer
// instances. RedisWatcher=true with no RedisComponent (or a nil
// Client()) is a fatal Build error — the flag is an explicit
// "I want multi-instance sync", silently degrading to single-pod
// scope would mask the misconfig.
//
// Optional dependency: "audit" — when Options.AuditEnabled is true
// the Builder will pull a parts.AuditComponent and attach a per-
// policy-change hook. Currently stubbed pending parts/audit landing
// (no AuditComponent exists yet); Builder fail-fast keeps yaml
// writers from setting the flag and getting silent no-op behaviour.
//
// Why we don't use github.com/casbin/gorm-adapter/v3: that library's
// init code blank-imports gorm.io/driver/postgres and
// gorm.io/driver/sqlserver, which transitively pulls jackc/pgx/v5 +
// microsoft/go-mssqldb + glebarez/sqlite + modernc/sqlite. On
// darwin/arm64 stripped, those drivers cost +8.72 MB above the
// underlying Casbin runtime. chok's bundled adapter (adapter.go)
// reuses whichever GORM driver the application already configured;
// nothing more, nothing extra.
//
// Failure ordering: AuditEnabled is rejected BEFORE dbFromKernel +
// newGormAdapter so a misconfigured startup doesn't leave a freshly-
// migrated casbin_rule table behind when the flag would have failed
// anyway. RedisWatcher needs the kernel access path so its checks
// run later, but if it fails the enforcer hasn't been wired into
// any other component yet — the partially-constructed adapter +
// table is harmless (subsequent successful boot reuses the table).
func Builder(opts Options) parts.AuthzBuilder {
	return func(k component.Kernel) (authz.Authorizer, error) {
		if opts.AuditEnabled {
			return nil, errors.New("authz/casbin: AuditEnabled=true requires the audit component which is not yet shipped — disable in chok.yaml or await the follow-up PR")
		}

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

		if opts.RedisWatcher {
			rc, err := redisClientFromKernel(k)
			if err != nil {
				return nil, fmt.Errorf("authz/casbin RedisWatcher: %w", err)
			}
			w, err := newRedisWatcher(context.Background(), rc, opts.defaultedChannel())
			if err != nil {
				return nil, fmt.Errorf("authz/casbin RedisWatcher: %w", err)
			}
			if err := auth.withWatcher(w); err != nil {
				// Constructed watcher is unowned at this point — release it
				// so the subscriber goroutine doesn't outlive a failed Build.
				w.Close()
				return nil, fmt.Errorf("authz/casbin RedisWatcher attach: %w", err)
			}
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

// redisClientFromKernel pulls *goredis.Client out of chok's
// RedisComponent. Returns a structured error when the kernel
// doesn't carry one — AuthzComponent.WithOptionalDependencies("redis")
// only ensures Init ordering, not presence; an explicit
// "RedisWatcher requires redis" message guides the operator who
// turned the flag on without configuring redis.
func redisClientFromKernel(k component.Kernel) (*goredis.Client, error) {
	c := k.Get("redis")
	if c == nil {
		return nil, errors.New("RedisComponent not registered (configure redis in chok.yaml or disable RedisWatcher)")
	}
	rc, ok := c.(*parts.RedisComponent)
	if !ok {
		return nil, fmt.Errorf("expected *parts.RedisComponent, got %T", c)
	}
	if rc == nil {
		return nil, errors.New("RedisComponent registered as nil pointer")
	}
	cli := rc.Client()
	if cli == nil {
		return nil, errors.New("RedisComponent.Client() returned nil — RedisOptions may be unset; RedisWatcher requires an active redis connection")
	}
	return cli, nil
}
