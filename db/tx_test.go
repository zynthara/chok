package db

import (
	"context"
	"errors"
	"testing"
)

// The AfterCommit staging buffer is the anchor store.WithBus hangs
// event publication off (SPEC §3.5): staged callbacks must run only
// after COMMIT, in order, and never after a rollback.

func TestAfterCommit_OutsideTx_ReturnsFalse(t *testing.T) {
	ran := false
	if AfterCommit(context.Background(), func(context.Context) { ran = true }) {
		t.Fatal("AfterCommit outside a transaction must return false")
	}
	if ran {
		t.Fatal("callback must not run when staging fails")
	}
}

func TestAfterCommit_FlushesInOrderAfterCommit(t *testing.T) {
	gdb := openTestDB(t)
	if err := Migrate(context.Background(), gdb, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}

	type ctxKey string
	parent := context.WithValue(context.Background(), ctxKey("k"), "v")

	var order []int
	var flushCtxVal string
	var committedAtFlush bool

	err := RunInTx(parent, gdb, func(txCtx context.Context) error {
		if !AfterCommit(txCtx, func(ctx context.Context) {
			order = append(order, 1)
			flushCtxVal, _ = ctx.Value(ctxKey("k")).(string)
			// By flush time the write must be visible outside the tx.
			var count int64
			gdb.Model(&TestItem{}).Count(&count)
			committedAtFlush = count == 1
		}) {
			t.Error("AfterCommit inside tx must stage")
		}
		AfterCommit(txCtx, func(context.Context) { order = append(order, 2) })
		return DBFromContext(txCtx).Create(&TestItem{Code: "AC1"}).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("staged callbacks must flush in order, got %v", order)
	}
	if flushCtxVal != "v" {
		t.Fatalf("flush must receive the parent context, got key=%q", flushCtxVal)
	}
	if !committedAtFlush {
		t.Fatal("flush ran before the transaction committed")
	}
}

func TestAfterCommit_DroppedOnRollback(t *testing.T) {
	gdb := openTestDB(t)
	if err := Migrate(context.Background(), gdb, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}

	ran := false
	err := RunInTx(context.Background(), gdb, func(txCtx context.Context) error {
		AfterCommit(txCtx, func(context.Context) { ran = true })
		if err := DBFromContext(txCtx).Create(&TestItem{Code: "AC2"}).Error; err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if ran {
		t.Fatal("staged callback must be dropped on rollback — phantom event")
	}
}

func TestAfterCommit_DroppedOnPanic(t *testing.T) {
	gdb := openTestDB(t)
	if err := Migrate(context.Background(), gdb, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}

	ran := false
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic must propagate")
			}
		}()
		_ = RunInTx(context.Background(), gdb, func(txCtx context.Context) error {
			AfterCommit(txCtx, func(context.Context) { ran = true })
			panic("boom")
		})
	}()
	if ran {
		t.Fatal("staged callback must be dropped when the transaction panics")
	}
}

func TestAfterCommit_NestedTxFlushesOnceAtOuterCommit(t *testing.T) {
	gdb := openTestDB(t)
	if err := Migrate(context.Background(), gdb, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}

	var order []string
	err := RunInTx(context.Background(), gdb, func(outerCtx context.Context) error {
		AfterCommit(outerCtx, func(context.Context) { order = append(order, "outer") })
		innerErr := RunInTx(outerCtx, gdb, func(innerCtx context.Context) error {
			AfterCommit(innerCtx, func(context.Context) { order = append(order, "inner") })
			return nil
		})
		if innerErr != nil {
			return innerErr
		}
		if len(order) != 0 {
			t.Error("inner RunInTx return must not flush — only the outermost commit does")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "outer" || order[1] != "inner" {
		t.Fatalf("expected [outer inner] at outermost commit, got %v", order)
	}
}

func TestHandle_RunInTx_JoinsSameMachinery(t *testing.T) {
	gdb := openTestDB(t)
	if err := Migrate(context.Background(), gdb, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}
	h := Wrap(gdb)

	ran := false
	err := h.RunInTx(context.Background(), func(txCtx context.Context) error {
		if DBFromContext(txCtx) == nil {
			t.Error("handle RunInTx must put the tx into the context")
		}
		AfterCommit(txCtx, func(context.Context) { ran = true })
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("handle RunInTx commit must flush staged callbacks")
	}
}
