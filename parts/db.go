package parts

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/db"
)

// DBBuilder opens a *gorm.DB, typically by calling db.NewMySQL or
// db.NewSQLite with options pulled from the application config. Kernel
// is provided for components that need access to other components
// (e.g. a logger for GORM integration).
type DBBuilder func(k component.Kernel) (*gorm.DB, error)

// DBComponent owns the application-wide *gorm.DB. It implements
// Migratable and runs the supplied TableSpecs automatically during
// Registry.Start — removing the need for user code to call
// db.Migrate separately.
//
// The component does NOT declare Dependencies: in principle it could
// depend on "log" for a shared logger, but GORM's logger is wired at
// build time inside DBBuilder, so the extra ordering constraint would
// buy nothing.
type DBComponent struct {
	build  DBBuilder
	tables []db.TableSpec
	pingTO time.Duration
	// skipClose is toggled by WithoutClose — used when the caller
	// shares a pre-existing *gorm.DB across multiple Component
	// lifecycles (e.g. tests that reuse one sqlite :memory:
	// connection) and doesn't want Component.Close to drop it.
	skipClose bool

	// gdb is published atomically so Health probes racing with Close
	// observe either a valid handle or nil, never a half-closed one.
	gdb atomic.Pointer[gorm.DB]
}

// NewDBComponent constructs a DBComponent. tables are passed to
// db.Migrate during Migratable.Migrate — omit for components that don't
// own any schema (migrations happen elsewhere, e.g. AccountComponent).
func NewDBComponent(build DBBuilder, tables ...db.TableSpec) *DBComponent {
	return &DBComponent{
		build:  build,
		tables: tables,
		pingTO: 500 * time.Millisecond,
	}
}

// Name implements component.Component.
func (d *DBComponent) Name() string { return "db" }

// ConfigKey implements component.Component.
func (d *DBComponent) ConfigKey() string { return "db" }

// OptionalDependencies declares tracing as a soft dependency so the
// TracerProvider is available when DB Init enables query tracing.
func (d *DBComponent) OptionalDependencies() []string { return []string{"tracing"} }

// Init opens the connection pool. A nil builder result is treated as a
// programmer error (there's no "disabled DB" mode — unlike Redis, a
// running app without a DB is unusual enough that silent nil would
// hide bugs).
func (d *DBComponent) Init(ctx context.Context, k component.Kernel) error {
	gdb, err := d.build(k)
	if err != nil {
		return fmt.Errorf("db init: %w", err)
	}
	if gdb == nil {
		return fmt.Errorf("db init: builder returned nil *gorm.DB")
	}
	d.gdb.Store(gdb)

	// Auto-enable GORM query tracing when TracingComponent is active.
	if tc, ok := k.Get("tracing").(*TracingComponent); ok && tc != nil && tc.Enabled() {
		db.EnableTracing(gdb)
	}

	return nil
}

// Migrate runs db.Migrate over the registered TableSpecs. Called by
// Registry.Start immediately after Init succeeds.
func (d *DBComponent) Migrate(ctx context.Context) error {
	if len(d.tables) == 0 {
		return nil
	}
	gdb := d.gdb.Load()
	if gdb == nil {
		return fmt.Errorf("db migrate: connection not initialised")
	}
	return db.Migrate(ctx, gdb, d.tables...)
}

// Close terminates the underlying sql.DB pool — unless WithoutClose
// was used, in which case Close is a no-op and the caller retains
// ownership of the connection. Idempotent: concurrent or repeat calls
// only close the handle once.
func (d *DBComponent) Close(ctx context.Context) error {
	if d.skipClose {
		return nil
	}
	gdb := d.gdb.Swap(nil)
	if gdb == nil {
		return nil
	}
	return db.Close(gdb)
}

// WithoutClose instructs Close to leave the connection open. Use when
// the builder returned a shared *gorm.DB that should outlive the
// component (test helpers, multi-App topologies).
func (d *DBComponent) WithoutClose() *DBComponent {
	d.skipClose = true
	return d
}

// Health runs a ping against the sql.DB. Down on failure — unlike
// Redis, a DB we can't reach is almost always application-fatal.
func (d *DBComponent) Health(ctx context.Context) component.HealthStatus {
	gdb := d.gdb.Load()
	if gdb == nil {
		return component.HealthStatus{Status: component.HealthDown, Error: "db not initialised"}
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return component.HealthStatus{Status: component.HealthDown, Error: err.Error()}
	}
	// Pick the tighter of the caller's deadline and the probe's own
	// pingTO. A /healthz wrapped by middleware.Timeout(200ms) shouldn't
	// spend 500ms on the ping and then return 504 to the client; a
	// probe-controlled pingTO shouldn't allow a parent ctx's generous
	// deadline to stretch it either.
	pingDeadline := d.pingTO
	if dl, ok := ctx.Deadline(); ok {
		remaining := time.Until(dl)
		if remaining <= 0 {
			// Caller's deadline has already expired. Running Ping
			// against an already-cancelled ctx produces an immediate
			// "context deadline exceeded" that gets reported as Down —
			// misleading, since the DB is probably fine. Short-circuit
			// to Degraded instead.
			return component.HealthStatus{
				Status: component.HealthDegraded,
				Error:  "health probe deadline exceeded before ping",
			}
		}
		if remaining < pingDeadline {
			pingDeadline = remaining
		}
	}
	pctx, cancel := context.WithTimeout(ctx, pingDeadline)
	defer cancel()

	start := time.Now()
	if err := sqlDB.PingContext(pctx); err != nil {
		return component.HealthStatus{Status: component.HealthDown, Error: err.Error()}
	}
	stats := sqlDB.Stats()
	details := map[string]any{
		"latency_ms":    time.Since(start).Milliseconds(),
		"open_conns":    stats.OpenConnections,
		"in_use":        stats.InUse,
		"idle":          stats.Idle,
		"max_open":      stats.MaxOpenConnections,
		"wait_count":    stats.WaitCount,
		"wait_duration": stats.WaitDuration.String(),
	}

	status := component.HealthOK
	var errMsg string

	// Pool saturation check: >80% in-use signals connection pressure.
	// Flagged as Degraded so operators get early warning before the pool
	// is fully exhausted and new requests start blocking.
	if stats.MaxOpenConnections > 0 {
		ratio := float64(stats.InUse) / float64(stats.MaxOpenConnections)
		details["pool_utilization"] = fmt.Sprintf("%.1f%%", ratio*100)
		if ratio > 0.8 {
			status = component.HealthDegraded
			errMsg = fmt.Sprintf("connection pool utilization %.0f%% exceeds 80%% threshold", ratio*100)
		}
	}

	return component.HealthStatus{
		Status:  status,
		Details: details,
		Error:   errMsg,
	}
}

// DB returns the underlying *gorm.DB. nil before Init and after Close.
func (d *DBComponent) DB() *gorm.DB { return d.gdb.Load() }

// SetPingTimeout overrides the Health probe timeout (test hook).
func (d *DBComponent) SetPingTimeout(t time.Duration) { d.pingTO = t }
