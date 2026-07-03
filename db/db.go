package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zynthara/chok/v2/config"
)

// NewMySQL creates a GORM DB connected to MySQL.
func NewMySQL(opts *config.MySQLOptions) (*gorm.DB, error) {
	dsn := (&gomysql.Config{
		User:                 opts.Username,
		Passwd:               opts.Password,
		Net:                  "tcp",
		Addr:                 fmt.Sprintf("%s:%d", opts.Host, opts.Port),
		DBName:               opts.Database,
		Params:               map[string]string{"charset": "utf8mb4"},
		ParseTime:            true,
		Loc:                  time.Local,
		AllowNativePasswords: true,
	}).FormatDSN()

	gdb, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("db: open mysql: %w", err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("db: get underlying sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(opts.MaxOpenConns)
	sqlDB.SetMaxIdleConns(opts.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(opts.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(opts.ConnMaxIdleTime)

	return gdb, nil
}

// NewSQLite creates a GORM DB using SQLite.
func NewSQLite(opts *config.SQLiteOptions) (*gorm.DB, error) {
	gdb, err := gorm.Open(sqlite.Open(opts.Path), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("db: open sqlite: %w", err)
	}
	// Enable WAL mode for concurrency.
	if err := gdb.Exec("PRAGMA journal_mode=WAL").Error; err != nil {
		return nil, fmt.Errorf("db: sqlite enable WAL: %w", err)
	}
	return gdb, nil
}

// Close closes the underlying connection pool.
// Intended for use with App.AddCleanup.
func Close(gdb *gorm.DB) error {
	sqlDB, err := gdb.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// runTransaction wraps fn in Begin/Commit/Rollback.
// ctx is propagated to all DB operations inside fn.
//
// Internal since v2: the public transaction model is RunInTx's context
// propagation — a second bare Begin/Commit surface was exactly the
// v1 ambiguity SPEC §5.1 removes.
//
// On panic, the transaction is rolled back before re-raising. A failure
// of the rollback itself (e.g. driver hung, connection already torn) is
// surfaced through gorm's logger so the panic frame still reaches the
// caller intact — wrapping the error into the panic would change the
// observable type and confuse recover() handlers upstream.
func runTransaction(ctx context.Context, gdb *gorm.DB, fn func(tx *gorm.DB) error) error {
	tx := gdb.WithContext(ctx).Begin(&sql.TxOptions{})
	if tx.Error != nil {
		return tx.Error
	}

	defer func() {
		if r := recover(); r != nil {
			func() {
				defer func() {
					if rbPanic := recover(); rbPanic != nil {
						gdb.Logger.Error(ctx,
							"db: transaction rollback panicked during recovery: %v",
							rbPanic)
					}
				}()
				if rb := tx.Rollback(); rb.Error != nil {
					gdb.Logger.Error(ctx,
						"db: transaction rollback after panic failed: %v",
						rb.Error)
				}
			}()
			panic(r)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback().Error; rbErr != nil {
			return fmt.Errorf("%w (rollback also failed: %v)", err, rbErr)
		}
		return err
	}
	return tx.Commit().Error
}

// --- Context-scoped transaction propagation ----------------------------------

type txCtxKey struct{}

// RunInTx begins a transaction on h, stores it in the derived context,
// and passes that context to fn. Code inside fn — including Store
// methods — automatically detects and joins the transaction. If fn
// returns an error or panics, the transaction is rolled back;
// otherwise it is committed. This context propagation is the only v2
// transaction model (SPEC §5.1); cross-store atomic writes need no
// wiring beyond passing txCtx:
//
//	db.RunInTx(ctx, h, func(txCtx context.Context) error {
//	    userStore.Create(txCtx, &user)   // uses tx from txCtx
//	    orderStore.Create(txCtx, &order) // same transaction
//	    return nil
//	})
//
// Nested RunInTx calls reuse the outermost transaction (no savepoints).
//
// The derived context also carries an after-commit staging buffer (see
// AfterCommit): callbacks staged inside fn run — in order, with the
// parent ctx — only after COMMIT succeeds, and are discarded wholesale
// on rollback or panic.
func RunInTx(ctx context.Context, h *DB, fn func(txCtx context.Context) error) error {
	return runInTxGorm(ctx, h.gdb, fn)
}

// runInTxGorm is RunInTx over a raw handle — shared by the public
// entrypoint and in-package callers that predate the thin handle.
func runInTxGorm(ctx context.Context, gdb *gorm.DB, fn func(txCtx context.Context) error) error {
	// If there's already a transaction in context, reuse it — including
	// its staging buffer, so events flush only at the outermost commit.
	if _, ok := ctx.Value(txCtxKey{}).(*gorm.DB); ok {
		return fn(ctx)
	}

	pending := &txPending{}
	err := runTransaction(ctx, gdb, func(tx *gorm.DB) error {
		txCtx := context.WithValue(ctx, txCtxKey{}, tx)
		txCtx = context.WithValue(txCtx, txPendingKey{}, pending)
		return fn(txCtx)
	})
	if err != nil {
		return err // rollback: staged callbacks are dropped, never run
	}
	pending.flush(ctx)
	return nil
}

// DBFromContext returns the *gorm.DB stored in ctx by RunInTx, or nil
// if no transaction is active. Store uses this to automatically
// participate in a context-scoped transaction.
func DBFromContext(ctx context.Context) *gorm.DB {
	if tx, ok := ctx.Value(txCtxKey{}).(*gorm.DB); ok {
		return tx
	}
	return nil
}
