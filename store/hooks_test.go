package store

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/store/where"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func setupHookDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gdb
}

// ---------------------------------------------------------------------------
// BeforeCreate hook
// ---------------------------------------------------------------------------

func TestBeforeCreate_Fires(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithBeforeCreate(func(ctx context.Context, obj *Item) error {
			called.Add(1)
			return nil
		}),
	)

	if err := s.Create(context.Background(), &Item{Code: "BC1"}); err != nil {
		t.Fatal(err)
	}
	if called.Load() != 1 {
		t.Fatalf("BeforeCreate should fire once, got %d", called.Load())
	}
}

func TestBeforeCreate_AbortsPreventsRow(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	hookErr := errors.New("validation failed")
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithBeforeCreate(func(ctx context.Context, obj *Item) error {
			return hookErr
		}),
	)

	item := &Item{Code: "BC2"}
	err := s.Create(context.Background(), item)
	if !errors.Is(err, hookErr) {
		t.Fatalf("expected hook error, got %v", err)
	}

	// Row should NOT exist — before-hook aborted the write.
	var count int64
	gdb.Model(&Item{}).Count(&count)
	if count != 0 {
		t.Fatalf("before-hook abort should prevent row insertion, got %d rows", count)
	}
}

// ---------------------------------------------------------------------------
// BeforeUpdate hook
// ---------------------------------------------------------------------------

func TestBeforeUpdate_AbortsUpdate(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	hookErr := errors.New("update rejected")
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithUpdateFields("code"),
		WithBeforeUpdate(func(ctx context.Context, loc Locator, changes Changes) error {
			return hookErr
		}),
	)

	item := &Item{Code: "BU1"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	err := s.Update(context.Background(), RID(item.RID), Set(map[string]any{"code": "BU2"}))
	if !errors.Is(err, hookErr) {
		t.Fatalf("expected hook error, got %v", err)
	}

	// Row should still have old value.
	got, err := s.Get(context.Background(), RID(item.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != "BU1" {
		t.Fatalf("before-hook abort should prevent update, got code=%s", got.Code)
	}
}

// ---------------------------------------------------------------------------
// BeforeDelete hook
// ---------------------------------------------------------------------------

func TestBeforeDelete_AbortsDelete(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	hookErr := errors.New("delete rejected")
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithBeforeDelete(func(ctx context.Context, loc Locator) error {
			return hookErr
		}),
	)

	item := &Item{Code: "BD1"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	err := s.Delete(context.Background(), RID(item.RID))
	if !errors.Is(err, hookErr) {
		t.Fatalf("expected hook error, got %v", err)
	}

	// Row should still exist.
	got, err := s.Get(context.Background(), RID(item.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != "BD1" {
		t.Fatal("before-hook abort should prevent delete")
	}
}

// ---------------------------------------------------------------------------
// AfterCreate hook (fire-and-forget, no error return)
// ---------------------------------------------------------------------------

func TestAfterCreate_Fires(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	var capturedCode string

	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithAfterCreate(func(ctx context.Context, obj *Item) {
			called.Add(1)
			capturedCode = obj.Code
		}),
	)

	item := &Item{Code: "HOOK1"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	if called.Load() != 1 {
		t.Fatalf("AfterCreate should fire once, got %d", called.Load())
	}
	if capturedCode != "HOOK1" {
		t.Fatalf("AfterCreate should see the created object, got code=%s", capturedCode)
	}
}

func TestAfterCreate_MultipleHooks(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	var order []string
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithAfterCreate(func(ctx context.Context, obj *Item) {
			order = append(order, "first")
		}),
		WithAfterCreate(func(ctx context.Context, obj *Item) {
			order = append(order, "second")
		}),
	)

	if err := s.Create(context.Background(), &Item{Code: "MULTI"}); err != nil {
		t.Fatal(err)
	}

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("hooks should fire in registration order, got %v", order)
	}
}

// ---------------------------------------------------------------------------
// AfterUpdate hook (fire-and-forget)
// ---------------------------------------------------------------------------

func TestAfterUpdate_Fires(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithUpdateFields("code"),
		WithAfterUpdate(func(ctx context.Context, loc Locator, changes Changes) {
			called.Add(1)
		}),
	)

	item := &Item{Code: "UPD1"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	if err := s.Update(context.Background(), RID(item.RID), Set(map[string]any{"code": "UPD2"})); err != nil {
		t.Fatal(err)
	}

	if called.Load() != 1 {
		t.Fatalf("AfterUpdate should fire once, got %d", called.Load())
	}
}

func TestAfterUpdate_NotFiredOnNotFound(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithUpdateFields("code"),
		WithAfterUpdate(func(ctx context.Context, loc Locator, changes Changes) {
			called.Add(1)
		}),
	)

	err := s.Update(context.Background(), RID("itm_nonexistent"), Set(map[string]any{"code": "X"}))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if called.Load() != 0 {
		t.Fatal("AfterUpdate should not fire on not-found update")
	}
}

// ---------------------------------------------------------------------------
// AfterDelete hook (fire-and-forget)
// ---------------------------------------------------------------------------

func TestAfterDelete_Fires(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithAfterDelete(func(ctx context.Context, loc Locator) {
			called.Add(1)
		}),
	)

	item := &Item{Code: "DEL1"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(context.Background(), RID(item.RID)); err != nil {
		t.Fatal(err)
	}

	if called.Load() != 1 {
		t.Fatalf("AfterDelete should fire once, got %d", called.Load())
	}
}

func TestAfterDelete_FiredOnIdempotentNoop(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithAfterDelete(func(ctx context.Context, loc Locator) {
			called.Add(1)
		}),
	)

	// Delete non-existent item (idempotent, no WithVersion → returns nil).
	// Hook must NOT fire when no row was actually deleted (RowsAffected == 0).
	err := s.Delete(context.Background(), RID("itm_nonexistent"))
	if err != nil {
		t.Fatalf("idempotent delete should not error, got %v", err)
	}
	if called.Load() != 0 {
		t.Fatalf("AfterDelete should not fire on idempotent no-op, got %d calls", called.Load())
	}
}

// ---------------------------------------------------------------------------
// AfterDelete with soft-delete model
// ---------------------------------------------------------------------------

func TestAfterDelete_SoftDelete_Fires(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&User{}, db.SoftUnique("uk_email", "email"))); err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	s := New[User](gdb, log.Empty(),
		WithQueryFields("id", "name", "email"),
		WithUpdateFields("name"),
		WithAfterDelete(func(ctx context.Context, loc Locator) {
			called.Add(1)
		}),
	)

	u := &User{Name: "alice", Email: "alice@test.com"}
	if err := s.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(context.Background(), RID(u.RID)); err != nil {
		t.Fatal(err)
	}

	if called.Load() != 1 {
		t.Fatalf("AfterDelete should fire on soft delete, got %d calls", called.Load())
	}
}

// ---------------------------------------------------------------------------
// Hooks survive WithTx / withDB
// ---------------------------------------------------------------------------

func TestHooks_PreservedAcrossWithTx(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithAfterCreate(func(ctx context.Context, obj *Item) {
			called.Add(1)
		}),
	)

	err := s.Tx(context.Background(), func(tx *Store[Item]) error {
		return tx.Create(context.Background(), &Item{Code: "TX1"})
	})
	if err != nil {
		t.Fatal(err)
	}

	if called.Load() != 1 {
		t.Fatalf("hook should fire inside transaction, got %d", called.Load())
	}
}

// ---------------------------------------------------------------------------
// ListWithCursor basic test
// ---------------------------------------------------------------------------

func TestListWithCursor_FirstPage(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
	)

	// Create 5 items.
	for i := range 5 {
		code := "item" + string(rune('A'+i))
		if err := s.Create(context.Background(), &Item{Code: code}); err != nil {
			t.Fatal(err)
		}
	}

	// First page (no cursor value).
	page, err := s.ListWithCursor(context.Background(), "id", where.CursorAfter, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if page == nil {
		t.Fatal("expected non-nil page")
	}
	if len(page.Items) != 3 {
		t.Fatalf("expected 3 items on first page, got %d", len(page.Items))
	}
	if page.NextCursor == "" {
		t.Fatal("expected NextCursor for first page with more items")
	}
}

func TestListWithCursor_LastPage_NoCursor(t *testing.T) {
	gdb := setupHookDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}

	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
	)

	// Create 2 items.
	for i := range 2 {
		code := "item" + string(rune('A'+i))
		if err := s.Create(context.Background(), &Item{Code: code}); err != nil {
			t.Fatal(err)
		}
	}

	// Request more than available.
	page, err := s.ListWithCursor(context.Background(), "id", where.CursorAfter, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(page.Items))
	}
	if page.NextCursor != "" {
		t.Fatalf("expected empty NextCursor for last page, got %q", page.NextCursor)
	}
}
