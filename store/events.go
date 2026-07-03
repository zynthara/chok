package store

import (
	"context"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/kernel/event"
)

// Op names the write that produced an EntityChanged event.
type Op string

// Write operations carried by EntityChanged.Op. Upsert publishes
// OpCreate — mirroring v1's after-create hook semantics for upserts —
// regardless of whether the row was inserted or updated on conflict.
const (
	OpCreate Op = "create"
	OpUpdate Op = "update"
	OpDelete Op = "delete"
)

// EntityChanged is the typed event a WithBus store publishes after a
// successful write. Which fields are set depends on the operation,
// mirroring the information v1's after-hooks received:
//
//	OpCreate — Object (a shallow copy; safe to read after the caller
//	           mutates or reuses the original)
//	OpUpdate — Locator + Changes
//	OpDelete — Locator
//
// Subscribe by concrete entity type:
//
//	event.Subscribe(bus, func(ctx context.Context, ev store.EntityChanged[model.Post]) {
//	    ...
//	})
//
// Publication is anchored to transaction commit (see WithBus): events
// from rolled-back writes never fire.
type EntityChanged[T any] struct {
	Op      Op
	Object  *T
	Locator Locator
	Changes Changes
}

// publishChanged emits ev on the Store's bus, if any. Inside a
// transaction the event is staged on the after-commit buffer — via the
// operation ctx when it carries the transaction (db.RunInTx
// propagation), else via the clone's captured txCtx (Store.Tx callers
// use their own outer ctx with the tx-bound clone) — so rollbacks drop
// it. Outside any transaction it publishes immediately.
func (s *Store[T]) publishChanged(ctx context.Context, ev EntityChanged[T]) {
	if s.bus == nil {
		return
	}
	publish := func(c context.Context) { event.Publish(c, s.bus, ev) }
	if db.AfterCommit(ctx, publish) {
		return
	}
	if s.txCtx != nil && db.AfterCommit(s.txCtx, publish) {
		return
	}
	publish(ctx)
}

// createdEvent builds the OpCreate event with a shallow copy of obj so
// asynchronous subscribers never race the caller's ongoing use of the
// original (the same copy discipline v1's WithAsyncAfterCreate had).
func createdEvent[T any](obj *T) EntityChanged[T] {
	cp := *obj
	return EntityChanged[T]{Op: OpCreate, Object: &cp}
}
