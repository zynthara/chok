package parts

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"gorm.io/gorm"

	"github.com/zynthara/chok/audit"
	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/log"
)

// AuditResolver extracts AuditOptions from the application config.
// Returning nil disables the component: Init becomes a no-op,
// Logger() returns nil, Health reports OK.
type AuditResolver func(appConfig any) *config.AuditOptions

// AuditComponent owns the chok-blessed audit log sink. Other
// Components reach the *audit.Logger via Kernel.Get("audit").(*AuditComponent).Logger().
//
// Lifecycle (SPEC parts-audit-claude.md §8):
//   - Init     : AutoMigrate audit_logs table; start async DB sink
//                worker (single goroutine drains a buffered channel
//                and batches inserts).
//   - Migrate  : AutoMigrate audit_logs table (idempotent; run by
//                Registry between Init and any other component's
//                Migrate that depends on audit being queryable).
//   - Health   : pending / dropped / written / failed counters +
//                last sink error (if any).
//   - Reload   : reload-safe = RetentionDays / PurgeInterval /
//                PurgeBatchSize; restart-only = AsyncBufferSize +
//                DropOnFull (mid-flight changes would surprise
//                producer-side callers).
//   - Close    : close producer channel, wait for worker to flush
//                in-flight batch, return.
//
// Dependencies declaration (SPEC §8 v0.3.5 revision):
//   - hard:     "db"
//   - optional: "authz" (admin API gating), "http" (mount admin
//                route), "scheduler" (purge cron — landed in 7.D)
//
// Pool was removed from Dependencies in v0.3.5 because audit is a
// single-producer-MPSC batched sink, not a generic any-func task —
// running it on PoolComponent would couple audit-flush ordering to
// pool teardown and prevent the Component from drainage-on-Close.
type AuditComponent struct {
	resolve AuditResolver

	// Cached references taken in Init. db is borrowed from
	// DBComponent (the underlying *gorm.DB; we don't own its
	// lifecycle). logger is the chok-owned async sink.
	db     *gorm.DB
	logger *audit.DBLogger

	// opts.Load() returns the latest config snapshot the resolver
	// produced. Reload swaps it; reload-safe fields are read on
	// each access by the relevant code path (purge cron etc),
	// restart-only fields are baked into the live worker at Init
	// and warned about on Reload.
	opts atomic.Pointer[config.AuditOptions]

	// disabled is set when Init resolves a nil/disabled config so
	// Close / Health / Logger short-circuit. atomic so Health can
	// be read concurrently with shutdown.
	disabled atomic.Bool

	// kernel is captured during Init so Reload can re-resolve
	// against ConfigSnapshot() and emit warnings via the kernel
	// logger when restart-only fields change.
	kernel component.Kernel
	// chokLogger is the per-component log.Logger used for Reload
	// warnings. Distinct from the audit.Logger sink — this is the
	// chok structured logger, not the audit sink.
	chokLogger log.Logger
}

// NewAuditComponent constructs the component. The resolver is
// re-consulted on every Init/Reload — same pattern as
// NewLoggerComponent / NewRedisComponent.
func NewAuditComponent(resolve AuditResolver) *AuditComponent {
	return &AuditComponent{resolve: resolve}
}

// Name implements component.Component.
func (a *AuditComponent) Name() string { return "audit" }

// ConfigKey implements component.ConfigKeyer.
func (a *AuditComponent) ConfigKey() string { return "audit" }

// Dependencies declares "db" as the only hard dependency. Audit
// cannot start without a live *gorm.DB.
func (a *AuditComponent) Dependencies() []string { return []string{"db"} }

// OptionalDependencies declares the soft tier. Missing entries
// degrade gracefully:
//
//   - authz absent ⇒ admin API still mounts but rejects all
//     requests via middleware fail-closed (matches the SPEC
//     guarantee that no audit data leaks unauthenticated)
//   - http absent  ⇒ admin API not mounted; Logger() still works
//   - scheduler absent ⇒ purge cron not started; entries live
//     forever until the operator wires a scheduler
//
// http is needed only for EnableAdminAPI=true; we declare it
// regardless so Init ordering is stable.
func (a *AuditComponent) OptionalDependencies() []string {
	return []string{"authz", "http", "scheduler"}
}

// Init resolves the options, AutoMigrates the table, and starts
// the async sink. nil resolver result is a valid disabled
// configuration (Init no-ops, Logger() returns nil) — operators
// who haven't reached the audit ship date can leave the component
// registered without fielding the table.
//
// Init ctx is NOT used as the sink worker's parent — Registry
// cancels Init ctx on return, which would immediately tear down
// the worker. The sink runs under context.Background; lifecycle
// is bounded by Close instead.
func (a *AuditComponent) Init(ctx context.Context, k component.Kernel) error {
	a.kernel = k
	a.chokLogger = k.Logger().With("component", "audit")

	opts := a.resolve(k.ConfigSnapshot())
	if opts == nil || !opts.Enabled {
		a.disabled.Store(true)
		a.chokLogger.Info("audit: disabled by config")
		return nil
	}
	a.opts.Store(opts)

	dbc, ok := k.Get("db").(*DBComponent)
	if !ok || dbc == nil {
		return errors.New("audit: DBComponent not registered (declare AuditComponent.Dependencies = [\"db\"])")
	}
	gdb := dbc.DB()
	if gdb == nil {
		return errors.New("audit: DBComponent.DB() returned nil — DB Init may have failed")
	}
	a.db = gdb

	if err := gdb.WithContext(ctx).AutoMigrate(&audit.Log{}); err != nil {
		return fmt.Errorf("audit: AutoMigrate audit_logs: %w", err)
	}

	a.logger = audit.NewDBLogger(
		context.Background(), // sink lifetime decoupled from Init ctx
		gdb,
		opts.AsyncBufferSize,
		opts.DropOnFull,
		a.chokLogger,
	)
	a.chokLogger.Info("audit sink started",
		"async_buffer_size", opts.AsyncBufferSize,
		"drop_on_full", opts.DropOnFull,
		"retention_days", opts.RetentionDays,
	)
	return nil
}

// Migrate runs AutoMigrate idempotently. Init also runs it — Migrate
// is exposed so chok components that depend on audit can rely on
// the table existing after the Migrate phase, regardless of init
// order subtleties.
func (a *AuditComponent) Migrate(ctx context.Context) error {
	if a.disabled.Load() || a.db == nil {
		return nil
	}
	if err := a.db.WithContext(ctx).AutoMigrate(&audit.Log{}); err != nil {
		return fmt.Errorf("audit: Migrate AutoMigrate: %w", err)
	}
	return nil
}

// Close drains the sink. After Close returns, Logger() invocations
// from peer components MUST stop — they would race against a closed
// channel. Registry teardown order (audit closes before db) keeps
// in-flight batches' DB writes safe.
func (a *AuditComponent) Close(_ context.Context) error {
	if a.disabled.Load() || a.logger == nil {
		return nil
	}
	a.logger.Close()
	return nil
}

// Health reports the sink's lifetime counters. Disabled = OK with
// zero counters (operators who turned audit off don't want it
// reported as down).
func (a *AuditComponent) Health(_ context.Context) component.HealthStatus {
	if a.disabled.Load() || a.logger == nil {
		return component.HealthStatus{Status: component.HealthOK}
	}
	stats := a.logger.Stats()
	status := component.HealthOK
	if stats.Failed > 0 || stats.Dropped > 0 {
		status = component.HealthDegraded
	}
	details := map[string]any{
		"pending": stats.Pending,
		"dropped": stats.Dropped,
		"written": stats.Written,
		"failed":  stats.Failed,
	}
	if err, at := a.logger.LastError(); err != nil {
		details["last_error"] = err.Error()
		details["last_error_at"] = at.Format("2006-01-02T15:04:05Z07:00")
	}
	return component.HealthStatus{Status: status, Details: details}
}

// Reload re-reads the resolver and applies the reload-safe fields.
// AsyncBufferSize and DropOnFull are restart-only — changing them
// in yaml emits a warning but does not affect the running sink.
//
// Currently the reload-safe fields (RetentionDays / PurgeInterval /
// PurgeBatchSize) only matter to the purge cron, which is wired in
// 7.D. Until then Reload mostly serves as the warning-emission path
// and a smoke test for the option-swap logic.
func (a *AuditComponent) Reload(_ context.Context) error {
	if a.disabled.Load() {
		return nil
	}
	next := a.resolve(a.kernel.ConfigSnapshot())
	if next == nil {
		return nil
	}
	prev := a.opts.Load()
	a.opts.Store(next)

	if prev != nil {
		if prev.AsyncBufferSize != next.AsyncBufferSize {
			a.chokLogger.Warn("audit: async_buffer_size change requires restart",
				"old", prev.AsyncBufferSize, "new", next.AsyncBufferSize)
		}
		if prev.DropOnFull != next.DropOnFull {
			a.chokLogger.Warn("audit: drop_on_full change requires restart",
				"old", prev.DropOnFull, "new", next.DropOnFull)
		}
	}
	return nil
}

// Logger returns the chok-blessed audit Logger for caller code.
// Returns nil when the component is disabled — callers MUST nil-
// check and either skip the Log call or use a no-op fallback. The
// account / authz packages have a small helper for this.
func (a *AuditComponent) Logger() audit.Logger {
	if a.disabled.Load() || a.logger == nil {
		return nil
	}
	return a.logger
}
