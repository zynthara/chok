package db

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// DB is the chok-owned thin handle over the connection pool — the
// only database type that appears on v2 public surfaces (SPEC §5.2).
// It is what store.New takes, what db.From returns, and what carries
// the blessed transaction entrypoint (RunInTx). gorm stays internal;
// Unsafe is the single escape hatch.
type DB struct {
	gdb *gorm.DB
}

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
	gdb, err := openGorm(&o)
	if err != nil {
		return nil, err
	}
	return &DB{gdb: gdb}, nil
}

// Wrap adopts an existing gorm handle.
//
// Transition shim (M3-M4): it exists so v1-residue batteries (account,
// parts glue) can hand their *gorm.DB to the v2 store signature. New
// code uses db.Module + db.From, or db.Open. Removed when the last
// battery migrates (M4/M5).
func Wrap(gdb *gorm.DB) *DB {
	if gdb == nil {
		panic("db.Wrap: nil *gorm.DB")
	}
	return &DB{gdb: gdb}
}

// Unsafe returns the effective raw gorm handle: the context's
// transaction when one is active (RunInTx), the root pool otherwise,
// WithContext applied either way. It is the only sanctioned way to
// reach gorm from application code — the name is the warning: nothing
// above this line (whitelists, scopes, owner enforcement) applies to
// what you do with it.
func (h *DB) Unsafe(ctx context.Context) *gorm.DB {
	if tx := DBFromContext(ctx); tx != nil {
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
	return Migrate(ctx, h.gdb, specs...)
}

// Ping verifies connectivity (health probes).
func (h *DB) Ping(ctx context.Context) error {
	sqlDB, err := h.gdb.DB()
	if err != nil {
		return fmt.Errorf("db: get underlying sql.DB: %w", err)
	}
	return sqlDB.PingContext(ctx)
}

// Close terminates the underlying connection pool. Idempotent at the
// sql.DB layer.
func (h *DB) Close() error {
	return Close(h.gdb)
}
