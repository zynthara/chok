// Package txctx carries the context plumbing of the RunInTx transaction
// model: the active *gorm.DB transaction plus its owning *db.DB identity.
// Stores join automatically only when their handle owns the transaction.
//
// It is internal on purpose (M5 §5.2 re-review verdict): the raw
// transaction handle must not be reachable through a public API, so
// the only gorm-typed doors on the v2 surface are the two escape
// hatches that carry the warning in their name — Store.Unsafe
// (scopes applied) and db.DB.Unsafe (raw, tx-aware). Public code that
// only needs to know whether a transaction is active uses db.InTx.
package txctx

import (
	"context"

	"gorm.io/gorm"
)

type dbKey struct{}

type entry struct {
	owner any
	db    *gorm.DB
}

// WithDB returns ctx carrying tx as the active transaction handle.
func WithDB(ctx context.Context, owner any, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, dbKey{}, entry{owner: owner, db: tx})
}

// DB returns the transaction carried by ctx only when it belongs to owner.
// Keeping affinity here prevents one database handle from silently executing
// on another instance's transaction.
func DB(ctx context.Context, owner any) *gorm.DB {
	e, _ := ctx.Value(dbKey{}).(entry)
	if e.owner != owner {
		return nil
	}
	return e.db
}

// AnyDB reports the active transaction without exposing it to public callers.
func AnyDB(ctx context.Context) *gorm.DB {
	e, _ := ctx.Value(dbKey{}).(entry)
	return e.db
}
