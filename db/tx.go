package db

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/internal/dbx"
)

// txKey carries the transaction of one handle in a context: a transaction
// of another *DB in the same context is not this handle's.
type txKey struct{}

type txEntry struct {
	owner   *DB
	tx      *gorm.DB
	pending *pending
}

func txOf(ctx context.Context, d *DB) *txEntry {
	e, _ := ctx.Value(txKey{}).(*txEntry)
	if e == nil || e.owner != d {
		return nil
	}
	return e
}

// RunInTx runs fn in a transaction whose context is txCtx: every store and
// Unsafe call made with txCtx joins it. fn's error rolls it back, so does a
// panic, which is re-raised. A RunInTx inside another joins the outer
// transaction of the same handle; the outermost commit is the only one.
// Nesting a different handle is rejected because its context would hide
// the outer transaction. There is no cross-database atomicity.
func (d *DB) RunInTx(ctx context.Context, fn func(txCtx context.Context) error) error {
	if txOf(ctx, d) != nil {
		return fn(ctx)
	}
	if e, _ := ctx.Value(txKey{}).(*txEntry); e != nil && e.owner != d {
		return fmt.Errorf("db: nesting transactions from different handles is not supported; use the outer transaction context for its handle")
	}
	p := &pending{}
	var ob dbx.Observer
	if o := d.obs.Load(); o != nil {
		ob = o.o
	}
	err := runTransaction(ctx, d.gdb, func(tx *gorm.DB) error {
		return fn(context.WithValue(ctx, txKey{}, &txEntry{owner: d, tx: tx, pending: p}))
	}, ob)
	if err != nil {
		return err
	}
	p.flush(ctx)
	return nil
}

func runTransaction(ctx context.Context, gdb *gorm.DB, fn func(tx *gorm.DB) error, observers ...dbx.Observer) (err error) {
	phase := func(op string, fn func() error) error {
		begin := time.Now()
		err := fn()
		if len(observers) > 0 {
			if o, ok := observers[0].(interface {
				Phase(context.Context, string, time.Time, error)
			}); ok {
				o.Phase(ctx, op, begin, err)
			}
		}
		return err
	}
	var tx *gorm.DB
	_ = phase("BEGIN", func() error { tx = gdb.WithContext(ctx).Begin(&sql.TxOptions{}); return tx.Error })
	if tx.Error != nil {
		return tx.Error
	}
	defer func() {
		if p := recover(); p != nil {
			_ = phase("ROLLBACK", func() error { return tx.Rollback().Error })
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		if rb := phase("ROLLBACK", func() error { return tx.Rollback().Error }); rb != nil {
			return fmt.Errorf("%w (rollback also failed: %v)", err, rb)
		}
		return err
	}
	return phase("COMMIT", func() error { return tx.Commit().Error })
}

// InTx reports whether ctx carries a transaction of this handle.
func (d *DB) InTx(ctx context.Context) bool { return txOf(ctx, d) != nil }

// AfterCommit stages fn to run after the transaction in ctx commits, in
// staging order, with a context that is no longer transactional; a
// rollback drops it. Outside a transaction it does nothing and returns
// false. Callbacks are synchronous and best effort, retain the caller's
// cancellation, and are lost on a crash; use a transactional outbox when
// execution must be durable.
func (d *DB) AfterCommit(ctx context.Context, fn func(ctx context.Context)) bool {
	e := txOf(ctx, d)
	if e == nil {
		return false
	}
	e.pending.add(fn)
	return true
}

// Unsafe returns the *gorm.DB for ctx: the transaction RunInTx started, or
// the handle. It is the one way to gorm and raw SQL; nothing outside db
// and store hands *gorm.DB around.
func (d *DB) Unsafe(ctx context.Context) *gorm.DB {
	if e := txOf(ctx, d); e != nil {
		return e.tx.WithContext(ctx)
	}
	return d.gdb.WithContext(ctx)
}

type pending struct {
	mu  sync.Mutex
	fns []func(context.Context)
}

func (p *pending) add(fn func(context.Context)) {
	p.mu.Lock()
	p.fns = append(p.fns, fn)
	p.mu.Unlock()
}

func (p *pending) flush(ctx context.Context) {
	p.mu.Lock()
	fns := p.fns
	p.fns = nil
	p.mu.Unlock()
	for _, fn := range fns {
		fn(ctx)
	}
}
