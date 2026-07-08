package db

import (
	"context"
	"fmt"
	"io/fs"
	"sync/atomic"
	"time"

	"github.com/zynthara/chok/v2/kernel"
)

// Module returns the db component for chok.Use. One call per database
// instance:
//
//	chok.Use(
//	    db.Module(db.WithTables(model.Tables()...)),          // (db, default)
//	    db.Module(db.As("read")),                             // (db, read) → db.instances.read
//	)
//
// Configuration lives in the "db" yaml section (named instances under
// db.instances.<name>, M1 nesting); the driver discriminator selects
// sqlite/mysql/postgres and db.migrate selects the schema strategy
// (SPEC §5.3). Handles are consumed via db.From(k) or the two-value
// chok.Get[*db.Component] path.
func Module(opts ...ModuleOption) kernel.Component {
	c := &Component{}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ModuleOption configures a Module at assembly time.
type ModuleOption func(*Component)

// As names the instance: the component registers as (db, <name>) and
// reads its config from db.instances.<name>. Use for read replicas,
// analytics databases, or any second connection.
func As(instance string) ModuleOption {
	return func(c *Component) { c.instance = instance }
}

// WithTables declares the schema this app owns for migrate: auto —
// gorm AutoMigrate over the specs during the kernel Migrate phase (v1
// semantics, dev default). Ignored (with a warning) under versioned
// and off.
func WithTables(specs ...TableSpec) ModuleOption {
	return func(c *Component) { c.tables = append(c.tables, specs...) }
}

// WithMigrations supplies the embedded forward-only migration set for
// migrate: versioned — conventionally:
//
//	//go:embed migrations/*.sql
//	var migrationsFS embed.FS
//	// the files live at the FS root the engine reads: sub to the dir
//	sub, _ := fs.Sub(migrationsFS, "migrations")
//	chok.Use(db.Module(db.WithMigrations(sub)))
//
// versioned mode without a migration source is an assembly error
// (fail-fast at Migrate phase).
func WithMigrations(fsys fs.FS) ModuleOption {
	return func(c *Component) { c.migrations = fsys }
}

// From returns the instance's *db.DB handle — the blessed accessor
// for Routes callbacks and custom components:
//
//	posts := store.New[model.Post](db.From(k), k.Logger())
//	replica := db.From(k, "read")
//
// A missing, disabled or not-ready db module is an assembly error, so
// From panics with instructions (fail-fast — a Routes callback that
// can't get its database has nothing sensible to do). Code that wants
// to handle absence gracefully uses the two-value form instead:
//
//	if c, ok := chok.Get[*db.Component](k, "db"); ok { h := c.Handle() }
func From(k kernel.Kernel, instance ...string) *DB {
	c, ok := kernel.Get[*Component](k, "db", instance...)
	if !ok {
		name := "db"
		if len(instance) > 0 && instance[0] != "" {
			name = "db@" + instance[0]
		}
		panic(fmt.Sprintf(
			"db.From: %s is not available — assemble it with chok.Use(db.Module(...)), check %s is enabled, and only call From after Init (Routes callbacks and component Init are safe); handle absence explicitly with chok.Get[*db.Component]",
			name, name))
	}
	h := c.Handle()
	if h == nil {
		panic("db.From: component present but connection not initialised (Init has not run?)")
	}
	return h
}

// Component is the db module. Exported so peers and the two-value
// accessor path can type-assert it; the handle accessor is Handle()
// (there is deliberately no method named DB on the v2 surface).
type Component struct {
	instance   string
	tables     []TableSpec
	migrations fs.FS

	opts   Options
	logger kernel.Logger

	// handle is published atomically so Health probes racing Close
	// observe either a live handle or nil, never a torn one.
	handle atomic.Pointer[DB]

	// maint is the sqlite background caretaker (WAL checkpoint +
	// optimize); nil for other drivers, memory databases, or when
	// both intervals are disabled. Init/Close only — no concurrent
	// access.
	maint *sqliteMaintenance
}

// Describe implements kernel.Component.
func (c *Component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "db",
		Instance:  c.instance,
		ConfigKey: "db",
		Options:   Options{},
		Needs: []kernel.Dep{
			{Kind: "log", Optional: true},
			{Kind: "tracing", Optional: true},
		},
	}
}

// Handle returns the thin database handle (nil before Init and after
// Close). db.From is the panicking convenience wrapper over this.
func (c *Component) Handle() *DB { return c.handle.Load() }

// MigrateMode reports this instance's schema strategy (MigrateAuto /
// MigrateVersioned / MigrateOff) once Init has decoded the section.
// Battery modules consult it from their own Migrators to honour the
// SPEC §5.3 whitelist semantics uniformly: battery tables AutoMigrate
// in auto and versioned modes; in off mode the framework — battery
// tables included — touches no schema.
func (c *Component) MigrateMode() string { return c.opts.Migrate }

// Init decodes the instance's section, opens the pool and verifies
// connectivity — a wrong DSN must fail startup, not the first query.
func (c *Component) Init(ctx context.Context, k kernel.Kernel) error {
	c.logger = k.Logger()
	key := kernel.SectionKeyOf(c.Describe())
	if err := k.Config().Section(key, &c.opts); err != nil {
		return fmt.Errorf("db: decode section %s: %w", key, err)
	}

	h, err := Open(c.opts)
	if err != nil {
		return err
	}
	if err := h.Ping(ctx); err != nil {
		_ = h.Close()
		return fmt.Errorf("db: connectivity check (%s): %w", c.opts.Driver, err)
	}

	// Query tracing rides the tracing module when it is assembled and
	// enabled — discovery by role interface, never by import (M2
	// pattern).
	if tc, ok := kernel.Get[interface{ Enabled() bool }](k, "tracing"); ok && tc.Enabled() {
		EnableTracing(h.gdb)
	}

	// A long-lived process must play the maintenance role a database
	// server would otherwise own — file-backed sqlite only.
	if c.opts.Driver == "sqlite" && !sqliteIsMemory(c.opts.SQLite.Path) {
		c.maint = startSQLiteMaintenance(h, &c.opts.SQLite, c.logger, displayInstance(c.instance))
	}

	c.handle.Store(h)
	c.logger.Info("db: connected",
		"instance", displayInstance(c.instance),
		"driver", c.opts.Driver,
		"migrate", c.opts.Migrate)
	return nil
}

// Migrate implements kernel.Migrator — the schema strategy dispatch
// (SPEC §5.3). Reload never reaches here: migration runs only in the
// kernel's start sequence, so "schema changes need a restart" is
// structural.
func (c *Component) Migrate(ctx context.Context) error {
	h := c.handle.Load()
	if h == nil {
		return fmt.Errorf("db: migrate: connection not initialised")
	}
	switch c.opts.Migrate {
	case MigrateOff:
		c.logger.Info("db: migrate off — framework touches no schema (battery tables included)",
			"instance", displayInstance(c.instance))
		return nil

	case MigrateVersioned:
		if len(c.tables) > 0 {
			c.logger.Warn("db: WithTables is ignored under migrate: versioned — application schema comes from the migration files",
				"instance", displayInstance(c.instance), "tables", len(c.tables))
		}
		if c.migrations == nil {
			return fmt.Errorf("db: migrate: versioned requires db.WithMigrations(embedded fs) on the module")
		}
		start := time.Now()
		applied, err := ApplyMigrations(ctx, h, c.migrations)
		for _, m := range applied {
			c.logger.Info("db: migration applied", "instance", displayInstance(c.instance),
				"version", m.Version, "file", m.File)
		}
		if err != nil {
			return err
		}
		c.logger.Info("db: versioned migrations up to date",
			"instance", displayInstance(c.instance),
			"applied_now", len(applied),
			"duration", time.Since(start).String(),
			"automigrate_exempt_framework_tables", FrameworkTables())
		return nil

	default: // MigrateAuto — validated enum, the zero-config dev path
		if len(c.tables) == 0 {
			return nil
		}
		return h.Migrate(ctx, c.tables...)
	}
}

// Health implements kernel.Healther: a bounded ping. (v1's pool
// saturation Degraded detail has no home in the error-shaped v2
// Healther — pool gauges belong to metrics.)
func (c *Component) Health(ctx context.Context) error {
	h := c.handle.Load()
	if h == nil {
		return fmt.Errorf("db: not initialised")
	}
	pctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	return h.Ping(pctx)
}

// Close terminates the pool. Idempotent; Health racing Close sees nil.
// The maintenance loop stops (synchronously, with a parting PRAGMA
// optimize) before the pools go away.
func (c *Component) Close(ctx context.Context) error {
	h := c.handle.Swap(nil)
	if h == nil {
		return nil
	}
	if c.maint != nil {
		c.maint.close()
		c.maint = nil
	}
	return h.Close()
}

func displayInstance(instance string) string {
	if instance == "" {
		return kernel.DefaultInstance
	}
	return instance
}
