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

	"github.com/zynthara/chok/config"
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

// Transaction wraps fn in Begin/Commit/Rollback.
// ctx is propagated to all DB operations inside fn.
//
// On panic, the transaction is rolled back before re-raising. A failure
// of the rollback itself (e.g. driver hung, connection already torn) is
// surfaced through gorm's logger so the panic frame still reaches the
// caller intact — wrapping the error into the panic would change the
// observable type and confuse recover() handlers upstream.
func Transaction(ctx context.Context, gdb *gorm.DB, fn func(tx *gorm.DB) error) error {
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

// RunInTx begins a transaction on gdb, stores the *gorm.DB in ctx, and
// passes the enriched context to fn. Code inside fn — including Store
// methods — automatically detects and uses the transaction via
// DBFromContext. If fn returns an error or panics, the transaction is
// rolled back; otherwise it is committed.
//
// RunInTx enables cross-Store transactions without manual WithTx wiring:
//
//	db.RunInTx(ctx, gdb, func(txCtx context.Context) error {
//	    userStore.Create(txCtx, &user)   // uses tx from txCtx
//	    orderStore.Create(txCtx, &order) // same transaction
//	    return nil
//	})
//
// Nested RunInTx calls reuse the outermost transaction (no savepoints).
func RunInTx(ctx context.Context, gdb *gorm.DB, fn func(txCtx context.Context) error) error {
	// If there's already a transaction in context, reuse it.
	if _, ok := ctx.Value(txCtxKey{}).(*gorm.DB); ok {
		return fn(ctx)
	}

	return Transaction(ctx, gdb, func(tx *gorm.DB) error {
		txCtx := context.WithValue(ctx, txCtxKey{}, tx)
		return fn(txCtx)
	})
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
