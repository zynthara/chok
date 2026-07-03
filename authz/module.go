package authz

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/v2/authz/casbin"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
)

// Compile-time assertions for the casbin engine room, which
// deliberately does not import this package (structural satisfaction;
// the one-way street lets authz.Module exist without an import cycle).
var (
	_ Authorizer       = (*casbin.Engine)(nil)
	_ DomainAuthorizer = (*casbin.Engine)(nil)
	_ io.Closer        = (*casbin.Engine)(nil)
)

// AuditSink is the primitive-typed emit surface the authz module
// requires of the audit module when casbin.audit_enabled is true. It
// is consumed structurally (kernel.Get against kind "audit") so authz
// never imports the audit package — audit reaches authz transitively
// through middleware, and a nominal import here would close a cycle.
//
// LogEventSync writes through to storage before returning (authz uses
// it as the startup probe: decision audit that cannot land an entry
// must fail startup, not silently no-op). LogEvent enqueues on the
// async sink — the per-mutation hook path.
type AuditSink interface {
	LogEvent(ctx context.Context, action, resource, result string, metadata map[string]string) error
	LogEventSync(ctx context.Context, action, resource, result string, metadata map[string]string) error
}

// Module returns the authz component for chok.Use. The web module
// attaches the Authorizer to every request context via the
// Authorizer() role accessor (soft dependency, M2 mechanism);
// application code gates routes with middleware.RequireAuthz.
//
// Lifecycle (SPEC §5.3 + M4 mini-SPEC): Init validates configuration
// coherence — including the audit_enabled truth table — and captures
// peer handles; Migrate creates casbin_rule (honouring the db migrate
// mode), constructs the engine (eager LoadPolicy), attaches the
// watcher and audit hook, then seeds bootstrap. Every step failing
// fails startup: policies are available before serve or the app does
// not come up (fail-closed).
func Module() kernel.Component { return &Component{} }

// Component owns the process-wide authorization engine.
type Component struct {
	opts   Options
	logger log.Logger

	h       *db.DB
	mode    string // db migrate mode captured at Init
	rclient *goredis.Client
	sink    AuditSink // non-nil iff casbin.audit_enabled

	// engine is published atomically at the end of Migrate so the
	// Authorizer() accessor (read by web Init, later level) never
	// observes a half-wired engine.
	engine atomic.Pointer[casbin.Engine]
}

// Describe implements kernel.Component. audit stays a soft dependency
// even though audit_enabled=true makes it a hard prerequisite —
// Describe is pure and cannot read config, so the hardening happens in
// Init (SPEC §6 truth table). The edge still orders audit's Init
// before authz's whenever audit is assembled.
func (c *Component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "authz",
		ConfigKey: "authz",
		Options:   Options{},
		Needs: []kernel.Dep{
			{Kind: "db"},
			{Kind: "redis", Optional: true},
			{Kind: "audit", Optional: true},
			{Kind: "log", Optional: true},
		},
	}
}

// Init decodes config, enforces the audit_enabled truth table and
// captures peer handles. No database access happens here — the
// casbin_rule table may not exist yet (Migrate owns schema).
func (c *Component) Init(ctx context.Context, k kernel.Kernel) error {
	if err := k.Config().Section("authz", &c.opts); err != nil {
		return fmt.Errorf("authz: decode section: %w", err)
	}
	if l, ok := k.Logger().(log.Logger); ok {
		c.logger = l.With("component", "authz")
	} else {
		c.logger = log.Empty()
	}

	// SPEC §6 truth table: audit_enabled=true upgrades audit to a hard
	// prerequisite. kernel.Get only surfaces assembled+enabled+ready
	// components, so "not assembled", "disabled by config" and "Init
	// failed" (which already aborted startup) all collapse into the
	// miss branch; a present component with a broken sink fails the
	// synchronous probe in Migrate.
	if c.opts.Casbin.AuditEnabled {
		sink, ok := kernel.Get[AuditSink](k, "audit")
		if !ok {
			return fmt.Errorf("authz: casbin.audit_enabled=true requires the audit module — assemble chok.Use(audit.Module()) and keep audit.enabled true (policy-mutation audit never silently degrades)")
		}
		c.sink = sink
	}

	dbc, ok := kernel.Get[interface {
		Handle() *db.DB
		MigrateMode() string
	}](k, "db")
	if !ok {
		return fmt.Errorf("authz: db module not available")
	}
	c.h = dbc.Handle()
	if c.h == nil {
		return fmt.Errorf("authz: db handle not initialised")
	}
	c.mode = dbc.MigrateMode()

	if c.opts.Casbin.RedisWatcher {
		rc, ok := kernel.Get[interface{ Client() *goredis.Client }](k, "redis")
		if !ok {
			return fmt.Errorf("authz: casbin.redis_watcher=true requires the redis module (assemble chok.Use(redis.Module()) or disable the watcher) — silently degrading to single-pod scope would mask the misconfig")
		}
		c.rclient = rc.Client()
		if c.rclient == nil {
			return fmt.Errorf("authz: casbin.redis_watcher=true but the redis module has no client")
		}
	}
	return nil
}

// Migrate implements kernel.Migrator: ensure the casbin_rule schema
// (honouring the framework migrate mode, SPEC §5.3), then bring the
// policy engine fully up — eager LoadPolicy, watcher, audit hook,
// bootstrap seeding. Running in the Migrate phase puts all of this
// strictly before web Init (a later topological level) and serve, so
// "policies available before serve" holds structurally; any failure
// aborts startup.
func (c *Component) Migrate(ctx context.Context) error {
	if c.mode == db.MigrateOff {
		c.logger.Info("authz: migrate mode off — casbin_rule schema untouched (operations own DDL)")
	} else {
		// casbin_rule is wire-compatible with gorm-adapter v3 — a
		// foreign-shaped table with no chok RID model, so it rides raw
		// AutoMigrate through the sanctioned escape hatch rather than
		// the db.Table spec path (which enforces db.Model embedding).
		if err := c.h.Unsafe(ctx).AutoMigrate(&casbin.CasbinRule{}); err != nil {
			return fmt.Errorf("authz: migrate casbin_rule: %w", err)
		}
	}

	// The engine's adapter is a lifetime handle: bind it to a
	// non-cancellable context (values kept for correlation) — the
	// Migrate ctx dies when the phase ends and must not poison every
	// later policy query.
	eng, err := casbin.NewEngine(c.opts.Casbin.ModelOrDefault(), c.h.Unsafe(context.WithoutCancel(ctx)), c.logger)
	if err != nil {
		return fmt.Errorf("authz: %w", err)
	}
	// Tear the engine down if any later wiring step fails — its
	// watcher/goroutines must not outlive a failed startup.
	wired := false
	defer func() {
		if !wired {
			_ = eng.Close()
		}
	}()

	if c.rclient != nil {
		// The watcher attaches after the initial LoadPolicy so a peer
		// publish can never race a not-yet-created table.
		if err := eng.AttachRedisWatcher(context.WithoutCancel(ctx), c.rclient, c.opts.Casbin.DefaultedChannel()); err != nil {
			return fmt.Errorf("authz: %w", err)
		}
	}

	if c.sink != nil {
		// Synchronous probe: decision audit that cannot land an entry
		// fails startup (truth-table branch 4 — sink present but
		// unavailable). The entry itself is honest evidence that
		// policy-mutation audit switched on.
		if err := c.sink.LogEventSync(ctx, "authz.audit.enabled", "authz", "success", nil); err != nil {
			return fmt.Errorf("authz: casbin.audit_enabled=true but the audit sink is unavailable: %w", err)
		}
		sink := c.sink
		logger := c.logger
		eng.AttachAuditHook(func(hctx context.Context, action, role, obj, act string) {
			if err := sink.LogEvent(hctx, "authz."+action, "casbin_rule", "success", map[string]string{
				"role":   role,
				"object": obj,
				"action": act,
			}); err != nil {
				// Best-effort by contract: audit failures are logged,
				// never undo the policy change (v1-documented trade-off).
				logger.Warn("authz: audit hook emit failed", "action", action, "error", err.Error())
			}
		})
	}

	if admin := c.opts.Casbin.BootstrapAdminUserID; admin != "" {
		// Seeding runs after hook attach so even day-one grants are
		// audited. (SPEC §6 wrote "Init tail" before the schema-timing
		// question surfaced; the seed writes casbin_rule, so it lives
		// here — still strictly pre-serve. Recorded as a SPEC
		// deviation in the M4 mini-SPEC §11.)
		if err := casbin.Bootstrap(ctx, eng, casbin.BootstrapConfig{AdminUserID: admin}); err != nil {
			return fmt.Errorf("authz: bootstrap seeding: %w", err)
		}
	}

	wired = true
	c.engine.Store(eng)
	return nil
}

// ReadyCheck implements kernel.Readier — defence in depth on top of
// the Migrate-phase ordering: readiness reports failure until the
// engine is published.
func (c *Component) ReadyCheck(ctx context.Context) error {
	if c.engine.Load() == nil {
		return fmt.Errorf("authz: policy engine not loaded")
	}
	return nil
}

// Close releases the engine (watcher subscription, audit hook).
func (c *Component) Close(ctx context.Context) error {
	eng := c.engine.Swap(nil)
	if eng == nil {
		return nil
	}
	return eng.Close()
}

// Authorizer is the role accessor the web module asserts for
// middleware.AttachAuthz. nil until Migrate has published the engine
// — under normal assembly web Init runs on a later level, so consumers
// always observe the wired engine.
func (c *Component) Authorizer() Authorizer {
	eng := c.engine.Load()
	if eng == nil {
		return nil
	}
	return eng
}

// Service exposes the runtime policy-management face (grants, role
// bindings, queries). nil until Migrate has published the engine.
func (c *Component) Service() casbin.Service {
	eng := c.engine.Load()
	if eng == nil {
		return nil
	}
	return eng
}

// WatcherStats surfaces the Redis watcher's best-effort counters
// (zero value when the watcher is off or the engine is not yet up).
func (c *Component) WatcherStats() casbin.WatcherStats {
	eng := c.engine.Load()
	if eng == nil {
		return casbin.WatcherStats{}
	}
	return eng.WatcherStats()
}
