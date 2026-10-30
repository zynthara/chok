package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/internal/dbtest"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

func TestGetForUpdate_NeedsATransaction(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	p := &post{Title: "x"}
	mustCreate(t, ctx, s, p)
	if _, err := s.GetForUpdate(ctx, store.RID(p.RID)); err == nil || !strings.Contains(err.Error(), "needs a transaction") {
		t.Fatalf("outside a transaction: %v", err)
	}
	err := s.Tx(ctx, func(tx context.Context) error {
		if _, err := s.GetForUpdate(tx, store.RID("pst_none")); !errors.Is(err, chok.ErrNotFound) {
			t.Errorf("missing row: %v", err)
		}
		if _, err := s.GetForUpdate(tx, store.RID(p.RID), where.Preload("Nothing")); err == nil {
			t.Error("a preload runs outside the lock")
		}
		got, err := s.GetForUpdate(tx, store.RID(p.RID))
		if err != nil || got.RID != p.RID {
			t.Errorf("locked read: %+v %v", got, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestGetForUpdate_SerializesLockers: a second locker waits for the first
// to commit and then reads its write. Postgres and MySQL hold a row lock;
// SQLite's transaction holds the only write connection.
func TestGetForUpdate_SerializesLockers(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	p := &post{Title: "counter"}
	mustCreate(t, ctx, s, p)
	locked, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- s.Tx(ctx, func(tx context.Context) error {
			if _, err := s.GetForUpdate(tx, store.RID(p.RID)); err != nil {
				close(locked)
				return err
			}
			close(locked)
			<-release
			return s.Update(tx, store.RID(p.RID), store.Set(map[string]any{"views": 1}))
		})
	}()
	<-locked
	second := make(chan int64, 1)
	secondErr := make(chan error, 1)
	go func() {
		secondErr <- s.Tx(ctx, func(tx context.Context) error {
			got, err := s.GetForUpdate(tx, store.RID(p.RID))
			if err == nil {
				second <- got.Views
			}
			return err
		})
	}()
	select {
	case v := <-second:
		t.Fatalf("the second locker read views=%d while the first held the lock", v)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	// The second transaction sends its read before it returns, so once its
	// error has arrived the read is there too. Selecting on both at once
	// let a nil error win the race and fail the test for nothing (seen on
	// the MySQL lane in CI).
	select {
	case err := <-secondErr:
		if err != nil {
			t.Fatalf("second locker: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second locker never ran")
	}
	select {
	case v := <-second:
		if v != 1 {
			t.Fatalf("the second locker read views=%d, want the committed 1", v)
		}
	default:
		t.Fatal("the second locker returned without reading")
	}
}

// TestGetForUpdate_OnlyItsOwnTransaction: a transaction of another handle
// does not count — the read would run outside it, unlocked.
func TestGetForUpdate_OnlyItsOwnTransaction(t *testing.T) {
	s := posts(t)
	other := openDB(t)
	ctx := t.Context()
	p := &post{Title: "x"}
	mustCreate(t, ctx, s, p)
	err := other.RunInTx(ctx, func(tx context.Context) error {
		_, err := s.GetForUpdate(tx, store.RID(p.RID))
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "needs a transaction") {
		t.Fatalf("another handle's transaction: %v", err)
	}
}

// TestGetForUpdate_RendersForUpdate: MySQL and PostgreSQL lock the row
// with FOR UPDATE; SQLite's transaction already holds its one writer, so
// the clause, which it would reject, is left out.
func TestGetForUpdate_RendersForUpdate(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	p := &post{Title: "x"}
	mustCreate(t, ctx, s, p)
	var sqls []string
	gdb := s.Unsafe(ctx)
	if err := gdb.Callback().Query().After("gorm:query").Register("test:capture", func(tx *gorm.DB) {
		sqls = append(sqls, tx.Statement.SQL.String())
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx context.Context) error {
		_, err := s.GetForUpdate(tx, store.RID(p.RID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(sqls) != 1 {
		t.Fatalf("statements %q", sqls)
	}
	locked := strings.HasSuffix(strings.TrimSpace(sqls[0]), "FOR UPDATE")
	if want := dbtest.Driver() != "sqlite"; locked != want {
		t.Fatalf("%s: %q", dbtest.Driver(), sqls[0])
	}
}
