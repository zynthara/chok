package store

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/log"
)

func setupLockStore(t *testing.T) (*Store[Product], *db.DB, context.Context) {
	t.Helper()
	gdb := setupDB(t)
	if err := gdb.Migrate(t.Context(), db.Table(&Product{})); err != nil {
		t.Fatal(err)
	}
	s := New[Product](gdb, log.Empty(),
		WithQueryFields("id", "name"), WithUpdateFields("name"))
	return s, gdb, userCtx("alice")
}

// TestGetForUpdate_RequiresSameHandleTx: the entry point refuses to run
// under autocommit (the lock would be released before the caller could
// act on the row) and accepts both blessed transaction shapes — the tx
// clone from Store.Tx and a db.RunInTx context on the same handle.
func TestGetForUpdate_RequiresSameHandleTx(t *testing.T) {
	s, gdb, alice := setupLockStore(t)
	p := &Product{Name: "widget"}
	if err := s.Create(alice, p); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetForUpdate(alice, RID(p.RID)); !errors.Is(err, ErrLockRequiresTx) {
		t.Fatalf("outside tx: err = %v, want ErrLockRequiresTx", err)
	}

	err := s.Tx(alice, func(tx *Store[Product]) error {
		got, err := tx.GetForUpdate(alice, RID(p.RID))
		if err != nil {
			return err
		}
		if got.RID != p.RID {
			t.Fatalf("locked read returned %q, want %q", got.RID, p.RID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = db.RunInTx(alice, gdb, func(txCtx context.Context) error {
		_, err := s.GetForUpdate(txCtx, RID(p.RID))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestGetForUpdate_RejectsPreload: association queries run outside the
// row lock, so combining them with a locked read is refused outright.
func TestGetForUpdate_RejectsPreload(t *testing.T) {
	s, _, alice := setupLockStore(t)
	p := &Product{Name: "widget"}
	if err := s.Create(alice, p); err != nil {
		t.Fatal(err)
	}
	err := s.Tx(alice, func(tx *Store[Product]) error {
		_, err := tx.GetForUpdate(alice, RID(p.RID), WithPreload("Owner"))
		return err
	})
	if !errors.Is(err, ErrLockPreload) {
		t.Fatalf("err = %v, want ErrLockPreload", err)
	}
}

// TestGetForUpdate_ReadOnlyStore: a lock is write intent — read-only
// stores refuse it before touching the database.
func TestGetForUpdate_ReadOnlyStore(t *testing.T) {
	gdb := setupDB(t)
	if err := gdb.Migrate(t.Context(), db.Table(&Product{})); err != nil {
		t.Fatal(err)
	}
	s := New[Product](gdb, log.Empty(),
		WithQueryFields("id", "name"), WithUpdateFields("name"), WithReadOnly())
	alice := userCtx("alice")
	err := db.RunInTx(alice, gdb, func(txCtx context.Context) error {
		_, err := s.GetForUpdate(txCtx, RID("prd_x"))
		return err
	})
	if !errors.Is(err, db.ErrReadOnly) {
		t.Fatalf("err = %v, want db.ErrReadOnly", err)
	}
}

// TestGetForUpdate_NotFound: the locked read keeps Get's not-found
// contract (structured NotFoundError compatible with ErrNotFound).
func TestGetForUpdate_NotFound(t *testing.T) {
	s, _, alice := setupLockStore(t)
	err := s.Tx(alice, func(tx *Store[Product]) error {
		_, err := tx.GetForUpdate(alice, RID("prd_missing"))
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestGetForUpdate_BlocksSecondLocker: on a server dialect a second
// transaction attempting to lock the same row must wait for the first
// to commit. If FOR UPDATE were dropped or ineffective, the second
// locker would acquire inside the first transaction's hold window and
// observe committed == false.
func TestGetForUpdate_BlocksSecondLocker(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("row-lock contention is observable only on a server dialect (sqlite transactions serialize on the single write connection)")
	}
	s, _, alice := setupLockStore(t)
	p := &Product{Name: "widget"}
	if err := s.Create(alice, p); err != nil {
		t.Fatal(err)
	}

	var committed atomic.Bool
	locked := make(chan struct{})
	second := make(chan error, 1)

	go func() {
		<-locked
		second <- s.Tx(alice, func(tx *Store[Product]) error {
			if _, err := tx.GetForUpdate(alice, RID(p.RID)); err != nil {
				return err
			}
			if !committed.Load() {
				return errors.New("second locker acquired the row before the first transaction committed")
			}
			return nil
		})
	}()

	err := s.Tx(alice, func(tx *Store[Product]) error {
		if _, err := tx.GetForUpdate(alice, RID(p.RID)); err != nil {
			return err
		}
		close(locked)
		// Give the second locker time to reach and block on FOR UPDATE;
		// were the lock ineffective it would acquire within this window.
		time.Sleep(300 * time.Millisecond)
		committed.Store(true)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}
