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

	h := wrapForTest(gdb)
	err := RunInTx(parent, h, func(txCtx context.Context) error {
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
		return h.Unsafe(txCtx).Create(&TestItem{Code: "AC1"}).Error
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
	h := wrapForTest(gdb)
	err := RunInTx(context.Background(), h, func(txCtx context.Context) error {
		AfterCommit(txCtx, func(context.Context) { ran = true })
		if err := h.Unsafe(txCtx).Create(&TestItem{Code: "AC2"}).Error; err != nil {
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
		_ = RunInTx(context.Background(), wrapForTest(gdb), func(txCtx context.Context) error {
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
	h := wrapForTest(gdb)
	err := RunInTx(context.Background(), h, func(outerCtx context.Context) error {
		AfterCommit(outerCtx, func(context.Context) { order = append(order, "outer") })
		innerErr := RunInTx(outerCtx, h, func(innerCtx context.Context) error {
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
	h := wrapForTest(gdb)

	ran := false
	err := h.RunInTx(context.Background(), func(txCtx context.Context) error {
		if !InTx(txCtx) {
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

// TestInTx_IntrospectionOnly pins the M5 §5.2 narrowing: InTx answers
// "is a transaction active" without handing out the raw handle, and
// the tx-aware Unsafe door observes the same transaction.
func TestInTx_IntrospectionOnly(t *testing.T) {
	gdb := openTestDB(t)
	if err := Migrate(context.Background(), gdb, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}
	h := wrapForTest(gdb)

	if InTx(context.Background()) {
		t.Fatal("InTx must be false outside RunInTx")
	}
	err := h.RunInTx(context.Background(), func(txCtx context.Context) error {
		if !InTx(txCtx) {
			t.Error("InTx must be true inside RunInTx")
		}
		// The write goes through the tx: visible inside, and rolled
		// back with it when fn errors below.
		if err := h.Unsafe(txCtx).Create(&TestItem{Code: "ITX"}).Error; err != nil {
			return err
		}
		var n int64
		if err := h.Unsafe(txCtx).Model(&TestItem{}).Count(&n).Error; err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("tx-aware Unsafe must see the uncommitted write, count=%d", n)
		}
		return errors.New("force rollback")
	})
	if err == nil {
		t.Fatal("expected forced rollback error")
	}
	var n int64
	if err := h.Unsafe(context.Background()).Model(&TestItem{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rollback must discard the write observed through Unsafe, count=%d", n)
	}
}
