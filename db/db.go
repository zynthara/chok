package db

import (
	"context"
	"database/sql"
	"fmt"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/internal/txctx"
)

// Close closes the underlying connection pool ((*DB).Close is the
// public face; this gorm-typed sibling serves the db tree internally).
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
// Nested RunInTx calls on the same handle reuse the outermost transaction
// (no savepoints). A transaction never propagates across database handles.
//
// The derived context also carries an after-commit staging buffer (see
// AfterCommit): callbacks staged inside fn run — in order, with the
// parent ctx — only after COMMIT succeeds, and are discarded wholesale
// on rollback or panic.
func RunInTx(ctx context.Context, h *DB, fn func(txCtx context.Context) error) error {
	if h.readOnly {
		return ErrReadOnly
	}
	return runInTxGorm(ctx, h, h.gdb, fn)
}

// runInTxGorm is RunInTx over a raw handle — shared by the public
// entrypoint and in-package callers that predate the thin handle.
func runInTxGorm(ctx context.Context, owner *DB, gdb *gorm.DB, fn func(txCtx context.Context) error) error {
	// If there's already a transaction in context, reuse it — including
	// its staging buffer, so events flush only at the outermost commit.
	if txctx.DB(ctx, owner) != nil {
		return fn(ctx)
	}

	pending := &txPending{}
	err := runTransaction(ctx, gdb, func(tx *gorm.DB) error {
		txCtx := txctx.WithDB(ctx, owner, tx)
		txCtx = context.WithValue(txCtx, txPendingKey{}, pending)
		return fn(txCtx)
	})
	if err != nil {
		return err // rollback: staged callbacks are dropped, never run
	}
	pending.flush(ctx)
	return nil
}

// InTx reports whether ctx carries an active RunInTx transaction.
// It is the introspection face ("assert this helper runs
// transactionally") of the transaction context, deliberately without
// the raw handle: code that needs SQL against the transaction goes
// through the tx-aware escape hatches — Store.Unsafe (scopes applied)
// or DB.Unsafe (raw) — the only public gorm doors (M5 §5.2 verdict).
//
// InTx is deliberately handle-agnostic, while joining is not: a true
// result means some handle's transaction is active, not that a given
// store or handle will execute inside it — only operations on the
// owning handle join (same-handle affinity).
func InTx(ctx context.Context) bool {
	return txctx.AnyDB(ctx) != nil
}
