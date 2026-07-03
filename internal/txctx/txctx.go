// Package txctx carries the context plumbing of the RunInTx
// transaction model: the active *gorm.DB transaction handle that
// db.RunInTx attaches and store joins automatically.
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

// WithDB returns ctx carrying tx as the active transaction handle.
func WithDB(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, dbKey{}, tx)
}

// DB returns the transaction handle carried by ctx, or nil when no
// RunInTx transaction is active.
func DB(ctx context.Context) *gorm.DB {
	tx, _ := ctx.Value(dbKey{}).(*gorm.DB)
	return tx
}
