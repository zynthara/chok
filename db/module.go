package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zynthara/chok/v2/kernel"
	choklog "github.com/zynthara/chok/v2/log"
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
// reads its config from db.instances.<name>. Use for read replicas
// (with read_only: true), analytics databases, or any second connection.
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

	// metrics is nil when the optional metrics module is absent. migmon
	// refreshes versioned migration state after the initial Migrate sample.
	metrics *dbMetrics
	migmon  *migrationMonitor
	seqMu   sync.RWMutex
	seqs    map[string]Sequence
}

// Describe implements kernel.Component.
func (c *Component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "db",
		Instance:  c.instance,
		ConfigKey: "db",
		Options:   Options{},
		Schema:    kernel.SchemaOwner{Tables: []string{ledgerTable}},
		Needs: []kernel.Dep{
			{Kind: "log", Optional: true},
			{Kind: "tracing", Optional: true},
			{Kind: "metrics", Optional: true},
		},
	}
}

// Handle returns the thin database handle (nil before Init and after
// Close). db.From is the panicking convenience wrapper over this.
func (c *Component) Handle() *DB { return c.handle.Load() }

// MigrateMode reports this instance's schema strategy (MigrateAuto /
// MigrateVersioned / MigrateOff) once Init has decoded the section.
// Battery modules consult it from their own Migrators to honour the
// SPEC §5.3 ownership semantics uniformly: battery tables AutoMigrate in auto,
// use their independent sequences in versioned, and remain untouched in off.
func (c *Component) MigrateMode() string { return c.opts.Migrate }

// ApplyOwnedMigrations applies a component-owned sequence to this database
// instance. It is only available under migrate: versioned; auto and off keep
// their explicit schema ownership semantics.
func (c *Component) ApplyOwnedMigrations(ctx context.Context, seq Sequence) (*ApplyReport, error) {
	if c.opts.Migrate != MigrateVersioned {
		return nil, fmt.Errorf("db: apply owned sequence %s requires migrate: versioned (effective mode %s)", seq.Kind(), c.opts.Migrate)
	}
	h := c.handle.Load()
	if h == nil {
		return nil, fmt.Errorf("db: apply owned sequence %s: connection not initialised", seq.Kind())
	}
	if err := c.registerOwnedSequence(h, seq); err != nil {
		return nil, fmt.Errorf("db: sequence=%s ledger=%s dialect=%s: %w",
			seq.Kind(), seq.Ledger(), h.gdb.Dialector.Name(), err)
	}
	report, err := ApplySequence(ctx, h, seq)
	c.logMigrationReport(report)
	if err == nil && c.metrics != nil {
		if metricErr := c.sampleOwnedMigrationMetrics(ctx, h, seq); metricErr != nil {
			c.logger.Warn("db: owned migration metrics sample failed",
				"instance", displayInstance(c.instance),
				"sequence", seq.Kind(),
				"ledger", seq.Ledger(),
				"dialect", h.gdb.Dialector.Name(),
				"error", metricErr)
		}
	}
	return report, err
}

func (c *Component) registerOwnedSequence(h *DB, seq Sequence) error {
	c.seqMu.Lock()
	defer c.seqMu.Unlock()
	if c.seqs == nil {
		c.seqs = make(map[string]Sequence)
	}
	if existing, ok := c.seqs[seq.Kind()]; ok {
		if existing.Ledger() != seq.Ledger() || !sameBaseline(existing.baseline, seq.baseline) {
			return fmt.Errorf("db: migration sequence %s registered with conflicting metadata", seq.Kind())
		}
		oldEngine, oldErr := resolveOwnedSequence(h, existing)
		newEngine, newErr := resolveOwnedSequence(h, seq)
		if oldErr != nil || newErr != nil {
			return errors.Join(oldErr, newErr)
		}
		oldFiles, oldErr := LoadMigrations(oldEngine.seq.fsys)
		newFiles, newErr := LoadMigrations(newEngine.seq.fsys)
		if oldErr != nil || newErr != nil {
			return errors.Join(oldErr, newErr)
		}
		if !sameMigrationFiles(oldFiles, newFiles) {
			return fmt.Errorf("db: migration sequence %s registered with conflicting migration bytes", seq.Kind())
		}
		return nil
	}
	for kind, existing := range c.seqs {
		if existing.Ledger() == seq.Ledger() {
			return fmt.Errorf("db: migration sequences %s and %s claim ledger %s", kind, seq.Kind(), seq.Ledger())
		}
	}
	c.seqs[seq.Kind()] = seq
	return nil
}

func sameBaseline(left, right Baseline) bool {
	if left.EquivalentVersion != right.EquivalentVersion || !sameStringSet(left.Tables, right.Tables) {
		return false
	}
	if len(left.Fingerprints) != len(right.Fingerprints) {
		return false
	}
	for dialect, fingerprint := range left.Fingerprints {
		if right.Fingerprints[dialect] != fingerprint {
			return false
		}
	}
	return true
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	want := make(map[string]struct{}, len(left))
	for _, item := range left {
		want[item] = struct{}{}
	}
	for _, item := range right {
		if _, ok := want[item]; !ok {
			return false
		}
	}
	return true
}

func sameMigrationFiles(left, right []Migration) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Version != right[i].Version || left[i].Name != right[i].Name || left[i].Checksum != right[i].Checksum {
			return false
		}
	}
	return true
}

func (c *Component) ownedSequenceSnapshot() []Sequence {
	c.seqMu.RLock()
	defer c.seqMu.RUnlock()
	out := make([]Sequence, 0, len(c.seqs))
	for _, seq := range c.seqs {
		out = append(out, seq)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind() < out[j].Kind() })
	return out
}

// Init decodes the instance's section, opens the pool and verifies
// connectivity — a wrong DSN must fail startup, not the first query.
func (c *Component) Init(ctx context.Context, k kernel.Kernel) error {
	c.logger = k.Logger()
	key := kernel.SectionKeyOf(c.Describe())
	if err := k.Config().Section(key, &c.opts); err != nil {
		return fmt.Errorf("db: decode section %s: %w", key, err)
	}
	if c.opts.ReadOnly {
		if c.opts.Migrate == MigrateAuto {
			c.logger.Info("db: migrate forced off for read-only instance",
				"instance", displayInstance(c.instance))
		}
		c.opts.Migrate = MigrateOff
	}

	h, err := Open(c.opts)
	if err != nil {
		return err
	}
	if err := h.Ping(ctx); err != nil {
		_ = h.Close()
		return fmt.Errorf("db: connectivity check (%s): %w", c.opts.Driver, err)
	}

	// Library-level Open is deliberately silent. A module-managed pool,
	// however, participates in the app's logging contract. The adapter keeps
	// SQL parameterized, logs errors independently, and treats 0 as "slow
	// logging off" rather than "all query logging off".
	h.gdb.Logger = newGORMLogger(choklog.From(k), c.opts.SlowThreshold)

	if provider, ok := kernel.Get[interface{ Registry() *prometheus.Registry }](k, "metrics"); ok {
		m, metricErr := newDBMetrics(provider.Registry(), h, displayInstance(c.instance))
		c.metrics = m
		if metricErr != nil {
			c.logger.Warn("db: metrics registration incomplete",
				"instance", displayInstance(c.instance), "error", metricErr)
		}
		if err := enableQueryMetrics(h.gdb, m); err != nil {
			c.logger.Warn("db: query metrics callbacks incomplete",
				"instance", displayInstance(c.instance), "error", err)
		}
	}

	// Query tracing rides the tracing module when it is assembled and
	// enabled — discovery by role interface, never by import (M2
	// pattern).
	if tc, ok := kernel.Get[interface{ Enabled() bool }](k, "tracing"); ok && tc.Enabled() {
		EnableTracing(h.gdb)
	}

	// A long-lived process must play the maintenance role a database
	// server would otherwise own — file-backed sqlite only.
	if c.opts.Driver == "sqlite" && !c.opts.ReadOnly && !sqliteIsMemory(c.opts.SQLite.Path) {
		var observer func(string, string)
		if c.metrics != nil {
			observer = c.metrics.observeMaintenance
		}
		c.maint = startSQLiteMaintenance(ctx, h, &c.opts.SQLite, c.logger, displayInstance(c.instance), observer)
	}

	c.handle.Store(h)
	c.logger.Info("db: connected",
		"instance", displayInstance(c.instance),
		"driver", c.opts.Driver,
		"read_only", c.opts.ReadOnly,
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
		if c.opts.ReadOnly && (len(c.tables) > 0 || c.migrations != nil) {
			c.logger.Warn("db: migration inputs ignored for read-only instance",
				"instance", displayInstance(c.instance),
				"tables", len(c.tables), "migrations", c.migrations != nil)
		}
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
		report, err := ApplyMigrationsWithReport(ctx, h, c.migrations)
		c.logMigrationReport(report)
		if err != nil {
			return err
		}
		if c.metrics != nil {
			if metricErr := c.sampleMigrationMetrics(ctx, h); metricErr != nil {
				c.logger.Warn("db: initial migration metrics sample failed",
					"instance", displayInstance(c.instance),
					"sequence", report.Sequence,
					"ledger", report.Ledger,
					"dialect", report.Dialect,
					"error", metricErr)
			}
			c.migmon = startMigrationMonitor(ctx, c.opts.MigrationStatusInterval,
				func(sampleCtx context.Context) error { return c.sampleMigrationMetrics(sampleCtx, h) },
				c.metrics, c.logger)
		}
		c.logger.Info("db: versioned migrations up to date",
			"instance", displayInstance(c.instance),
			"sequence", report.Sequence,
			"ledger", report.Ledger,
			"dialect", report.Dialect,
			"applied_now", len(report.Applied),
			"checksums_adopted", len(report.Adopted),
			"duration", time.Since(start).String(),
			"framework_owned_tables", FrameworkTables())
		return nil

	default: // MigrateAuto — validated enum, the zero-config dev path
		if len(c.tables) == 0 {
			return nil
		}
		return h.Migrate(ctx, c.tables...)
	}
}

func (c *Component) logMigrationReport(report *ApplyReport) {
	if report == nil {
		return
	}
	identity := []any{
		"instance", displayInstance(c.instance),
		"sequence", report.Sequence,
		"ledger", report.Ledger,
		"dialect", report.Dialect,
	}
	for _, adopted := range report.Adopted {
		c.logger.Info("db: migration baseline adopted", append(identity,
			"version", adopted.Version,
			"checksum", adopted.Checksum,
			"provenance", adopted.Provenance,
		)...)
	}
	for _, migration := range report.Applied {
		c.logger.Info("db: migration applied", append(identity,
			"version", migration.Version,
			"file", migration.File,
		)...)
	}
}

func (c *Component) sampleMigrationMetrics(ctx context.Context, h *DB) error {
	if c.metrics == nil {
		return nil
	}
	var errs []error
	if c.migrations != nil {
		files, err := LoadMigrations(c.migrations)
		if err != nil {
			errs = append(errs, fmt.Errorf("sequence=app ledger=%s dialect=%s: %w", ledgerTable, h.gdb.Dialector.Name(), err))
		} else {
			c.metrics.setExpectedMigrationVersion("app", files)
			st, statusErr := MigrationsStatus(ctx, h, c.migrations)
			if statusErr != nil {
				errs = append(errs, fmt.Errorf("sequence=app ledger=%s dialect=%s: %w", ledgerTable, h.gdb.Dialector.Name(), statusErr))
			} else {
				c.metrics.observeMigrationStatus("app", st)
			}
		}
	}
	for _, seq := range c.ownedSequenceSnapshot() {
		if err := c.sampleOwnedMigrationMetrics(ctx, h, seq); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", seq.Kind(), err))
		}
	}
	return errors.Join(errs...)
}

func (c *Component) sampleOwnedMigrationMetrics(ctx context.Context, h *DB, seq Sequence) error {
	e, err := resolveOwnedSequence(h, seq)
	if err != nil {
		return err
	}
	files, err := LoadMigrations(e.seq.fsys)
	if err != nil {
		return err
	}
	name := "chok_" + seq.Kind()
	c.metrics.setExpectedMigrationVersion(name, files)
	st, err := e.status(ctx, h)
	if err != nil {
		return wrapOwnedSequenceError(e, err)
	}
	c.metrics.observeMigrationStatus(name, st)
	return nil
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
// Background migration sampling stops first, followed by the maintenance
// loop — within ctx's budget, with a parting PRAGMA optimize when time
// allows; a stuck statement is interrupted rather than allowed to outlive
// registry teardown — then metrics detach and the pools go away.
func (c *Component) Close(ctx context.Context) error {
	h := c.handle.Swap(nil)
	if h == nil {
		return nil
	}
	if c.migmon != nil {
		c.migmon.close(ctx)
		c.migmon = nil
	}
	if c.maint != nil {
		c.maint.close(ctx)
		c.maint = nil
	}
	if c.metrics != nil {
		c.metrics.close()
		c.metrics = nil
	}
	return h.Close()
}

func displayInstance(instance string) string {
	if instance == "" {
		return kernel.DefaultInstance
	}
	return instance
}
