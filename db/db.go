package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v3/internal/conf"
	"github.com/zynthara/chok/v3/internal/dbx"
)

// DB is an open database: a gorm handle behind a narrow API, the schemas
// registered on it and, for SQLite, its maintenance. Register it with
// app.Use: it reports health and closes last of what used it.
type DB struct {
	gdb     *gorm.DB
	readers *sql.DB
	cfg     Config
	logger  *slog.Logger
	maint   *maintenance

	mu      sync.Mutex
	schemas []*ownerSchema
}

// ownerSchema is one owner's tables and migrations: the application's, given
// to Open, and each battery's, given to RegisterSchema.
type ownerSchema struct {
	owner      string
	tables     []TableSpec
	migrations fs.FS
}

// Option configures Open.
type Option func(*openConfig)

type openConfig struct {
	tables     []TableSpec
	migrations fs.FS
	logger     *slog.Logger
	owner      string
}

// Tables declares the application's tables for migrate: auto.
func Tables(specs ...TableSpec) Option {
	return func(c *openConfig) { c.tables = append(c.tables, specs...) }
}

// Migrations gives the application's SQL files for migrate: versioned,
// named <14-digit version>_<name>.sql; a sqlite/, mysql/ or postgres/
// subdirectory is used for its dialect when present.
func Migrations(fsys fs.FS) Option { return func(c *openConfig) { c.migrations = fsys } }

// Logger is where the handle and gorm log; the default follows slog.Default.
func Logger(l *slog.Logger) Option { return func(c *openConfig) { c.logger = l } }

// Owner names the application's rows in the migration ledger; the
// default is "app".
func Owner(name string) Option { return func(c *openConfig) { c.owner = name } }

var ownerName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Open connects, applies the migrate mode to the application's schema and
// returns the handle. A Config from chok.Load carries every default; one
// built in code is used as written, except that an empty Migrate is auto.
func Open(cfg Config, opts ...Option) (*DB, error) {
	oc := openConfig{owner: "app"}
	for _, o := range opts {
		o(&oc)
	}
	if cfg.Migrate == "" {
		cfg.Migrate = "auto"
	}
	if err := conf.Check(&cfg); err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	if !ownerName.MatchString(oc.owner) {
		return nil, fmt.Errorf("db: owner %q must be lower-case letters, digits, - or _", oc.owner)
	}
	// The caller keeps its slices: the handle's policy must not change
	// when they do.
	cfg.Store = cfg.Store.clone()
	d := &DB{cfg: cfg, logger: oc.logger}
	gcfg := &gorm.Config{
		Logger: &gormLogger{d: d, slow: cfg.SlowThreshold},
		// Timestamps gorm stamps are UTC on every dialect, so SQLite, which
		// keeps them as text with their offset, orders them by instant.
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
	gdb, readers, err := openGorm(&cfg, gcfg)
	if err != nil {
		return nil, err
	}
	d.gdb, d.readers = gdb, readers
	app := &ownerSchema{owner: oc.owner, tables: oc.tables, migrations: oc.migrations}
	if err := d.migrate(context.Background(), app); err != nil {
		_ = d.closePools()
		return nil, err
	}
	d.schemas = []*ownerSchema{app}
	if cfg.Driver == "sqlite" && !sqliteIsMemory(cfg.SQLite.Path) {
		d.maint = startMaintenance(d, cfg.SQLite)
	}
	return d, nil
}

// MustOpen is Open that panics on error.
func MustOpen(cfg Config, opts ...Option) *DB {
	d, err := Open(cfg, opts...)
	if err != nil {
		panic(err)
	}
	return d
}

// RegisterSchema adds a battery's tables and migrations under its own
// owner and applies the migrate mode to them now. Batteries call it from
// their constructors; applications declare theirs with Tables and
// Migrations.
func (d *DB) RegisterSchema(owner string, tables []TableSpec, migrations fs.FS) error {
	if !ownerName.MatchString(owner) {
		return fmt.Errorf("db: owner %q must be lower-case letters, digits, - or _", owner)
	}
	d.mu.Lock()
	for _, s := range d.schemas {
		if s.owner == owner {
			d.mu.Unlock()
			return fmt.Errorf("db: schema %q is already registered", owner)
		}
	}
	d.mu.Unlock()
	s := &ownerSchema{owner: owner, tables: tables, migrations: migrations}
	if err := d.migrate(context.Background(), s); err != nil {
		return err
	}
	d.mu.Lock()
	d.schemas = append(d.schemas, s)
	d.mu.Unlock()
	return nil
}

func (d *DB) registered() []*ownerSchema {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.schemas)
}

// Migrate applies the migrate mode to every registered schema: auto
// migrates the tables and their SoftUnique indexes, versioned applies the
// pending files, off checks that none is pending. Open and RegisterSchema
// already did this; the CLI and tests call it again.
func (d *DB) Migrate(ctx context.Context) error {
	for _, s := range d.registered() {
		if err := d.migrate(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) migrate(ctx context.Context, s *ownerSchema) error {
	for _, t := range s.tables {
		if len(t.indexes) > 0 {
			var idx []dbx.Index
			for _, i := range t.indexes {
				idx = append(idx, dbx.Index{Name: i.name, Columns: i.columns})
			}
			dbx.SetIndexes(t.typ, idx)
		}
	}
	switch d.cfg.Migrate {
	case "auto":
		return autoMigrate(ctx, d.gdb, s.tables)
	case "versioned":
		if s.migrations == nil {
			if len(s.tables) > 0 {
				return fmt.Errorf("db: migrate is versioned, but %s gave tables and no migrations (db.Migrations)", s.owner)
			}
			return nil
		}
		files, err := loadMigrations(s.migrations, s.owner, d.gdb.Name())
		if err != nil {
			return err
		}
		return d.apply(ctx, s.owner, files)
	default: // off
		sts, err := d.statusOf(ctx, s)
		if err != nil {
			return err
		}
		var behind []string
		for _, st := range sts {
			if st.State != "applied" {
				behind = append(behind, st.Version+"_"+st.Name+" ("+st.State+")")
			}
		}
		if len(behind) > 0 {
			return fmt.Errorf("%w: %s %s; apply them (chok migrate up) before starting with migrate: off", ErrNotMigrated, s.owner, strings.Join(behind, ", "))
		}
		return nil
	}
}

func (d *DB) statusOf(ctx context.Context, s *ownerSchema) ([]MigrationStatus, error) {
	if s.migrations == nil {
		return nil, nil
	}
	files, err := loadMigrations(s.migrations, s.owner, d.gdb.Name())
	if err != nil {
		return nil, err
	}
	rows, err := readLedger(ctx, d.gdb, s.owner)
	if err != nil {
		return nil, err
	}
	return status(files, rows), nil
}

// MigrationStatus lists the versioned migrations of every registered
// schema with their state.
func (d *DB) MigrationStatus(ctx context.Context) ([]MigrationStatus, error) {
	var out []MigrationStatus
	for _, s := range d.registered() {
		sts, err := d.statusOf(ctx, s)
		if err != nil {
			return nil, err
		}
		out = append(out, sts...)
	}
	return out, nil
}

// MarkApplied records the migration with version as applied without
// running it — the one manual intervention, for a schema repaired by
// hand. The version must belong to exactly one registered schema.
func (d *DB) MarkApplied(ctx context.Context, version string) error {
	var found []migration
	for _, s := range d.registered() {
		if s.migrations == nil {
			continue
		}
		files, err := loadMigrations(s.migrations, s.owner, d.gdb.Name())
		if err != nil {
			return err
		}
		for _, f := range files {
			if f.version == version {
				found = append(found, f)
			}
		}
	}
	switch len(found) {
	case 0:
		return fmt.Errorf("db: no registered migration has version %s", version)
	case 1:
		if err := markApplied(ctx, d.gdb, found[0]); err != nil {
			return fmt.Errorf("db: mark %s %s applied: %w", found[0].owner, found[0].file, err)
		}
		d.Logger().WarnContext(ctx, "db: migration marked applied without running", "owner", found[0].owner, "migration", found[0].file)
		return nil
	}
	return fmt.Errorf("db: version %s belongs to %d schemas; it is ambiguous", version, len(found))
}

// StorePolicy returns db.store, the posture stores on this handle start from.
func (d *DB) StorePolicy() StorePolicy { return d.cfg.Store.clone() }

// Name is "db", the handle's name in the health report.
func (d *DB) Name() string { return "db" }

// Logger returns the handle's logger.
func (d *DB) Logger() *slog.Logger {
	if d.logger != nil {
		return d.logger
	}
	return slog.Default()
}

// Health pings the database, and the read pool of a SQLite file.
func (d *DB) Health(ctx context.Context) error {
	pool, err := d.gdb.DB()
	if err != nil {
		return err
	}
	if err := pool.PingContext(ctx); err != nil {
		return err
	}
	if d.readers != nil {
		return d.readers.PingContext(ctx)
	}
	return nil
}

// Close stops the SQLite maintenance, running a last PRAGMA optimize when
// ctx allows, and closes the pools.
func (d *DB) Close(ctx context.Context) error {
	if d.maint != nil {
		d.maint.close(ctx)
	}
	return d.closePools()
}

func (d *DB) closePools() error {
	var errs []error
	if pool, err := d.gdb.DB(); err == nil {
		errs = append(errs, pool.Close())
	}
	if d.readers != nil {
		errs = append(errs, d.readers.Close())
	}
	return errors.Join(errs...)
}
