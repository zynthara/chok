package store

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

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

// lockProbe carries the dialect-specific SQL for observing a lock wait
// on the server: sessionID runs inside the second locker's transaction
// and returns its connection's server-side session identifier;
// isBlocked reports whether that session is currently parked in the
// server's lock-wait state.
type lockProbe struct {
	sessionID func(tx *gorm.DB) (int64, error)
	isBlocked func(raw *gorm.DB, id int64) (bool, error)
}

// assertGetForUpdateBlocksSecondLocker proves the row lock on a server
// dialect: a second transaction attempting GetForUpdate on the same row
// must be observed by the server in a lock wait before the first
// transaction commits, and must acquire only afterwards. The
// commit-side handshake (server-confirmed wait, not a fixed sleep)
// means a dropped or ineffective FOR UPDATE fails the wait probe
// deterministically instead of racing the scheduler.
//
// One end-to-end deadline context covers both transactions, every probe
// query and every channel wait. It flows into gorm (effectiveDB /
// Unsafe bind it), so even a hang at the driver or network layer is cut
// by the context instead of stalling the suite; a first-transaction
// failure releases the second goroutine through the same context.
func assertGetForUpdateBlocksSecondLocker(t *testing.T, h *db.DB, s *Store[Product], alice context.Context, probe lockProbe) {
	t.Helper()
	ctx, cancel := context.WithTimeout(alice, 60*time.Second)
	defer cancel()

	p := &Product{Name: "widget"}
	if err := s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	var committed atomic.Bool
	locked := make(chan struct{})
	session := make(chan int64, 1)
	second := make(chan error, 1)

	go func() {
		select {
		case <-locked:
		case <-ctx.Done():
			second <- ctx.Err()
			return
		}
		second <- s.Tx(ctx, func(tx *Store[Product]) error {
			g, err := tx.Unsafe(ctx)
			if err != nil {
				return err
			}
			id, err := probe.sessionID(g)
			if err != nil {
				return err
			}
			session <- id
			if _, err := tx.GetForUpdate(ctx, RID(p.RID)); err != nil {
				return err
			}
			if !committed.Load() {
				return errors.New("second locker acquired the row before the first transaction committed")
			}
			return nil
		})
	}()

	err := s.Tx(ctx, func(tx *Store[Product]) error {
		if _, err := tx.GetForUpdate(ctx, RID(p.RID)); err != nil {
			return err
		}
		close(locked)

		var id int64
		select {
		case id = <-session:
		case <-ctx.Done():
			return fmt.Errorf("second locker never reported its session: %w", ctx.Err())
		}
		// Commit only after the server confirms the second session is
		// parked in a lock wait — the strict proof that FOR UPDATE is
		// in effect. The shorter dedicated deadline separates "the lock
		// never took effect" from the helper-wide timeout.
		deadline := time.Now().Add(15 * time.Second)
		for {
			blocked, err := probe.isBlocked(h.Unsafe(ctx), id)
			if err != nil {
				return err
			}
			if blocked {
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				return errors.New("second locker never entered the server's lock-wait state — FOR UPDATE ineffective?")
			}
			time.Sleep(10 * time.Millisecond)
		}
		committed.Store(true)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("second locker did not finish after the first transaction committed: %v", ctx.Err())
	}
}

// TestGetForUpdate_BlocksSecondLocker: PostgreSQL half of the row-lock
// contract (runs in the Postgres lane; sqlite serializes all
// transactions on the single write connection, so there is no
// second-session contention to observe there).
func TestGetForUpdate_BlocksSecondLocker(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("row-lock contention is observable only on a server dialect")
	}
	s, gdb, alice := setupLockStore(t)
	assertGetForUpdateBlocksSecondLocker(t, gdb, s, alice, lockProbe{
		sessionID: func(tx *gorm.DB) (int64, error) {
			var id int64
			err := tx.Raw("SELECT pg_backend_pid()").Scan(&id).Error
			return id, err
		},
		isBlocked: func(raw *gorm.DB, id int64) (bool, error) {
			var evt string
			err := raw.Raw(
				"SELECT COALESCE(wait_event_type, '') FROM pg_stat_activity WHERE pid = ?", id,
			).Scan(&evt).Error
			return evt == "Lock", err
		},
	})
}

// TestGetForUpdate_MySQLBlocksSecondLocker: MySQL half of the row-lock
// contract — entry guard plus real InnoDB contention observed through
// information_schema.innodb_trx. Runs in the MySQL lane (make
// test-mysql); skips without CHOK_TEST_MYSQL_DSN. Beyond the lane's
// usual create-database grant, the wait probe needs the global PROCESS
// privilege (MySQL gates the INNODB_* information_schema tables on it;
// CI's root has it, custom lane users must be granted it).
func TestGetForUpdate_MySQLBlocksSecondLocker(t *testing.T) {
	h := dbtest.OpenMySQL(t)
	if err := h.Migrate(t.Context(), db.Table(&Product{})); err != nil {
		t.Fatal(err)
	}
	s := New[Product](h, log.Empty(),
		WithQueryFields("id", "name"), WithUpdateFields("name"))
	alice := userCtx("alice")

	if _, err := s.GetForUpdate(alice, RID("prd_x")); !errors.Is(err, ErrLockRequiresTx) {
		t.Fatalf("outside tx: err = %v, want ErrLockRequiresTx", err)
	}

	assertGetForUpdateBlocksSecondLocker(t, h, s, alice, lockProbe{
		sessionID: func(tx *gorm.DB) (int64, error) {
			var id int64
			err := tx.Raw("SELECT CONNECTION_ID()").Scan(&id).Error
			return id, err
		},
		isBlocked: func(raw *gorm.DB, id int64) (bool, error) {
			var state string
			err := raw.Raw(
				"SELECT COALESCE(MAX(trx_state), '') FROM information_schema.innodb_trx WHERE trx_mysql_thread_id = ?", id,
			).Scan(&state).Error
			if err != nil {
				return false, fmt.Errorf("querying information_schema.innodb_trx (needs the global PROCESS privilege for the CHOK_TEST_MYSQL_DSN user): %w", err)
			}
			return state == "LOCK WAIT", nil
		},
	})
}
