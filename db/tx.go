package db

import (
	"context"
	"sync"
)

// txPending is the after-commit staging buffer RunInTx attaches to the
// transaction context. Callbacks staged during the transaction run —
// in staging order — only after COMMIT succeeds; a rollback (error or
// panic) discards the whole buffer. This is the anchor store.WithBus
// event publication hangs off (SPEC §3.5: no phantom events from
// rolled-back writes), but it is deliberately generic: any code inside
// a transaction can stage commit-dependent work.
//
// The mutex is defensive: a transaction handle is single-goroutine by
// gorm contract, but staging is cheap enough that we don't make that
// assumption load-bearing here.
type txPending struct {
	mu  sync.Mutex
	fns []func(context.Context)
}

func (p *txPending) stage(fn func(context.Context)) {
	p.mu.Lock()
	p.fns = append(p.fns, fn)
	p.mu.Unlock()
}

// flush runs the staged callbacks in order. ctx is the parent (non-
// transactional) context — the transaction is over by the time these
// run.
func (p *txPending) flush(ctx context.Context) {
	p.mu.Lock()
	fns := p.fns
	p.fns = nil
	p.mu.Unlock()
	for _, fn := range fns {
		fn(ctx)
	}
}

type txPendingKey struct{}

// pendingFromContext returns the staging buffer carried by a RunInTx
// context, or nil outside a transaction.
func pendingFromContext(ctx context.Context) *txPending {
	p, _ := ctx.Value(txPendingKey{}).(*txPending)
	return p
}

// AfterCommit stages fn to run after the transaction carried by ctx
// commits, and reports whether staging happened. Outside a transaction
// it returns false and does nothing — the caller decides whether to
// run fn immediately (that split is exactly how store anchors
// EntityChanged publication to commit).
//
// Staged callbacks run in staging order, receive the transaction's
// parent context, and are discarded wholesale when the transaction
// rolls back or panics.
func AfterCommit(ctx context.Context, fn func(ctx context.Context)) bool {
	p := pendingFromContext(ctx)
	if p == nil {
		return false
	}
	p.stage(fn)
	return true
}
