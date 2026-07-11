package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/internal/txctx"
)

// DB is the chok-owned thin handle over the connection pool — the
// only database type that appears on v2 public surfaces (SPEC §5.2).
// It is what store.New takes, what db.From returns, and what carries
// the blessed transaction entrypoint (RunInTx). gorm stays internal;
// Unsafe is the single escape hatch.
type DB struct {
	gdb *gorm.DB

	readOnly bool

	// readPool is the sqlite read pool when the file-database
	// read/write split is active (nil otherwise — memory sqlite and
	// the network drivers). dbresolver owns the routing; the handle
	// owns the lifetime.
	readPool *sql.DB

	// storePolicy is the db.store config block this handle was opened
	// with — plain data the store package reads as its defaults, not a
	// kernel dependency (the bus stays explicitly injected, SPEC §3.5).
	storePolicy StorePolicy
}

// StorePolicy reports the app-level store defaults this handle was
// opened with (the "db.store" config block). store.New consults it
// for every knob the construction site leaves unset; the zero value
// leaves store behaviour exactly as documented on store.New.
func (h *DB) StorePolicy() StorePolicy { return h.storePolicy }

// ReadOnly reports whether this handle was opened with read_only: true.
func (h *DB) ReadOnly() bool { return h.readOnly }

// Open builds a handle from Options — the same constructor the db
// module uses at Init. Library-level use (tests, tools, kernel-less
// embedding):
//
//	h, err := db.Open(db.Options{Driver: "sqlite",
//	    SQLite: db.SQLiteOptions{Path: ":memory:"}})
//
// Validation runs first, so a misconfigured Options fails here rather
// than at first query.
func Open(opts Options) (*DB, error) {
	o := opts
	o.Enabled = true // library-level Open means "use it"; the kill switch is a module concern
	if o.Migrate == "" {
		o.Migrate = MigrateAuto
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if o.ReadOnly {
		o.Migrate = MigrateOff
	}
	gdb, readPool, err := openGorm(&o)
	if err != nil {
		return nil, err
	}
	if err := registerReadOnlyCallbacks(gdb, o.ReadOnly); err != nil {
		_ = Close(gdb)
		if readPool != nil {
			_ = readPool.Close()
		}
		return nil, fmt.Errorf("db: install read-only guards: %w", err)
	}
	return &DB{gdb: gdb, readOnly: o.ReadOnly, readPool: readPool, storePolicy: o.Store}, nil
}

// Unsafe returns the effective raw gorm handle: the context's
// transaction owned by this handle when one is active (RunInTx), the root
// pool otherwise, WithContext applied either way. Transactions from another
// database handle are deliberately ignored. It is the only sanctioned way to
// reach gorm from application code — the name is the warning: nothing
// above this line (whitelists, scopes, owner enforcement) applies to
// what you do with it. On a read-only handle, write callbacks still reject
// GORM writes; database credentials/driver mode are the final raw-SQL guard.
func (h *DB) Unsafe(ctx context.Context) *gorm.DB {
	if tx := txctx.DB(ctx, h); tx != nil {
		return tx.WithContext(ctx)
	}
	return h.gdb.WithContext(ctx)
}

// RunInTx runs fn inside a transaction carried by the derived context
// — the v2 transaction model (context propagation, SPEC §5.1). Store
// operations called with txCtx join automatically; nested calls reuse
// the outer transaction. See the package-level RunInTx for details.
func (h *DB) RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error {
	return RunInTx(ctx, h, fn)
}

// Migrate runs gorm AutoMigrate plus SoftUnique index creation over
// the given specs — the auto-mode primitive (also handy in tests).
// Versioned/off scheduling is the module's job; this method always
// migrates what it is given.
func (h *DB) Migrate(ctx context.Context, specs ...TableSpec) error {
	if h.readOnly {
		return ErrReadOnly
	}
	return Migrate(ctx, h.gdb, specs...)
}

// Ping verifies connectivity (health probes) — both pools when the
// sqlite read/write split is active, so a broken read DSN fails
// startup rather than the first query.
func (h *DB) Ping(ctx context.Context) error {
	sqlDB, err := h.gdb.DB()
	if err != nil {
		return fmt.Errorf("db: get underlying sql.DB: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return err
	}
	if h.readPool != nil {
		return h.readPool.PingContext(ctx)
	}
	return nil
}

// Close terminates the underlying connection pools. Idempotent at the
// sql.DB layer.
func (h *DB) Close() error {
	err := Close(h.gdb)
	if h.readPool != nil {
		err = errors.Join(err, h.readPool.Close())
	}
	return err
}
