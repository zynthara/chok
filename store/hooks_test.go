package store

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/kernel/event"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// setupHookDB rides the dbtest lane switch (SQLite default,
// Postgres under CHOK_TEST_DRIVER=postgres — M3 dual-run).
func setupHookDB(t *testing.T) *db.DB {
	t.Helper()
	return dbtest.Open(t)
}

// ---------------------------------------------------------------------------
// BeforeCreate hook
// ---------------------------------------------------------------------------

func TestBeforeCreate_Fires(t *testing.T) {
	gdb := setupHookDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&Item{})); err != nil {
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
	if err := gdb.Migrate(context.Background(), db.Table(&Item{})); err != nil {
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
	gdb.Unsafe(context.Background()).Model(&Item{}).Count(&count)
	if count != 0 {
		t.Fatalf("before-hook abort should prevent row insertion, got %d rows", count)
	}
}

// ---------------------------------------------------------------------------
// BeforeUpdate hook
// ---------------------------------------------------------------------------

func TestBeforeUpdate_AbortsUpdate(t *testing.T) {
	gdb := setupHookDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&Item{})); err != nil {
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
	if err := gdb.Migrate(context.Background(), db.Table(&Item{})); err != nil {
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
// WithBus — EntityChanged publication (replaces v1 after-hooks, SPEC §3.5)
//
// Subscribers use WithSync so delivery happens inline at publish/flush
// time — assertions need no async waiting. The three commit-anchoring
// branches (non-tx immediate / commit flush / rollback drop) are the
// M3 DoD tests.
// ---------------------------------------------------------------------------

type itemEvent = EntityChanged[Item]

// busAndLog subscribes a synchronous recorder for Item events.
func busAndLog(t *testing.T) (*event.Bus, *[]itemEvent) {
	t.Helper()
	bus := event.NewBus()
	t.Cleanup(func() { bus.Close(context.Background()) })
	var seen []itemEvent
	event.Subscribe(bus, func(_ context.Context, ev itemEvent) {
		seen = append(seen, ev)
	}, event.WithSync())
	return bus, &seen
}

func setupBusItemStore(t *testing.T) (*Store[Item], *[]itemEvent) {
	t.Helper()
	gdb := setupHookDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}
	bus, seen := busAndLog(t)
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithUpdateFields("code"),
		WithBus(bus),
	)
	return s, seen
}

func TestWithBus_CreatePublishesImmediately_NonTx(t *testing.T) {
	s, seen := setupBusItemStore(t)

	item := &Item{Code: "EV1"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	if len(*seen) != 1 {
		t.Fatalf("non-transactional create must publish immediately, got %d events", len(*seen))
	}
	ev := (*seen)[0]
	object := ev.Object.Value()
	if ev.Op != OpCreate || object == nil || object.Code != "EV1" {
		t.Fatalf("unexpected event %+v", ev)
	}
	// The event carries a copy — asynchronous consumers must never race
	// the caller's continued use of the original.
	if object == item {
		t.Fatal("event must carry a recursive copy, not the caller's pointer")
	}
}

func TestEntityChanged_SnapshotsNestedMutablePayloads(t *testing.T) {
	type payload struct {
		Labels map[string][]string
	}
	original := &payload{Labels: map[string][]string{"role": []string{"reader"}}}
	ev := createdEvent(original)
	original.Labels["role"][0] = "admin"
	original.Labels["new"] = []string{"caller-only"}
	object := ev.Object.Value()
	if got := object.Labels["role"][0]; got != "reader" {
		t.Fatalf("create event shares nested caller state: %q", got)
	}
	if _, ok := object.Labels["new"]; ok {
		t.Fatal("create event observed a caller map mutation")
	}
	object.Labels["role"][0] = "subscriber"
	if got := ev.Object.Value().Labels["role"][0]; got != "reader" {
		t.Fatalf("object snapshot accessor exposes internal state: %q", got)
	}

	input := map[string]any{"meta": map[string]any{"tags": []string{"a"}}}
	changes := newChangeSnapshot(input)
	input["meta"].(map[string]any)["tags"].([]string)[0] = "caller"
	values := changes.Values()
	if got := values["meta"].(map[string]any)["tags"].([]string)[0]; got != "a" {
		t.Fatalf("change snapshot shares caller state: %q", got)
	}
	values["meta"].(map[string]any)["tags"].([]string)[0] = "subscriber"
	again := changes.Values()
	if got := again["meta"].(map[string]any)["tags"].([]string)[0]; got != "a" {
		t.Fatalf("change snapshot accessor exposes internal state: %q", got)
	}
}

func TestWithBus_UpdatePublishes_GatedOnRowsAffected(t *testing.T) {
	s, seen := setupBusItemStore(t)

	item := &Item{Code: "EV2"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	*seen = (*seen)[:0]

	if err := s.Update(context.Background(), RID(item.RID), Set(map[string]any{"code": "EV2b"})); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 || (*seen)[0].Op != OpUpdate || (*seen)[0].Locator.Kind != LocatorRID || (*seen)[0].Changes.Empty() {
		t.Fatalf("update must publish OpUpdate with locator+changes, got %+v", *seen)
	}
	ev := (*seen)[0]
	if ev.Locator.RID != item.RID {
		t.Fatalf("event locator RID = %q, want %q", ev.Locator.RID, item.RID)
	}
	if value, ok := ev.Changes.Value("code"); !ok || value != "EV2b" {
		t.Fatalf("event changes are not inspectable: value=%v ok=%v", value, ok)
	}

	// Not-found update: no event (v1 after-hook gate carried over).
	*seen = (*seen)[:0]
	err := s.Update(context.Background(), RID("itm_nonexistent"), Set(map[string]any{"code": "X"}))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if len(*seen) != 0 {
		t.Fatal("no event may fire for a write that touched zero rows")
	}
}

func TestWithBus_DeletePublishes_NotOnIdempotentNoop(t *testing.T) {
	s, seen := setupBusItemStore(t)

	item := &Item{Code: "EV3"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	*seen = (*seen)[:0]

	if err := s.Delete(context.Background(), RID(item.RID)); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 || (*seen)[0].Op != OpDelete || (*seen)[0].Locator.Kind != LocatorRID {
		t.Fatalf("delete must publish OpDelete with locator, got %+v", *seen)
	}

	// Idempotent no-op delete (no row, no WithVersion): nil error, no event.
	*seen = (*seen)[:0]
	if err := s.Delete(context.Background(), RID("itm_nonexistent")); err != nil {
		t.Fatalf("idempotent delete should not error, got %v", err)
	}
	if len(*seen) != 0 {
		t.Fatal("idempotent no-op delete must not publish — phantom deletion")
	}
}

func TestWithBus_SoftDeletePublishes(t *testing.T) {
	gdb := setupHookDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&User{}, db.SoftUnique("uk_email", "email"))); err != nil {
		t.Fatal(err)
	}
	bus := event.NewBus()
	t.Cleanup(func() { bus.Close(context.Background()) })
	var seen []EntityChanged[User]
	event.Subscribe(bus, func(_ context.Context, ev EntityChanged[User]) {
		seen = append(seen, ev)
	}, event.WithSync())

	s := New[User](gdb, log.Empty(),
		WithQueryFields("id", "name", "email"),
		WithUpdateFields("name"),
		WithBus(bus),
	)

	u := &User{Name: "alice", Email: "alice@test.com"}
	if err := s.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	seen = seen[:0]

	if err := s.Delete(context.Background(), RID(u.RID)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Op != OpDelete {
		t.Fatalf("soft delete must publish OpDelete, got %+v", seen)
	}
}

func TestWithBus_UpsertPublishesTruthfulPayloadFreeEvent(t *testing.T) {
	s, seen := setupBusItemStore(t)

	obj := &Item{Code: "EVUP"}
	if err := s.Upsert(context.Background(), obj, []string{"code"}); err != nil {
		t.Fatal(err)
	}
	// Conflict path: same code again.
	again := &Item{Code: "EVUP"}
	if err := s.Upsert(context.Background(), again, []string{"code"}); err != nil {
		t.Fatal(err)
	}

	if len(*seen) != 2 {
		t.Fatalf("both upsert paths must publish, got %d", len(*seen))
	}
	for i, ev := range *seen {
		if ev.Op != OpUpsert || !ev.Object.Empty() || ev.Locator.Kind != "" || !ev.Changes.Empty() {
			t.Fatalf("upsert event #%d must be payload-free OpUpsert, got %+v", i, ev)
		}
	}
}

func TestWithBus_BatchUpsertPublishesOncePerCallAfterCommit(t *testing.T) {
	s, seen := setupBusItemStore(t)
	ctx := context.Background()
	items := []*Item{{Code: "BUPE1"}, {Code: "BUPE2"}}
	if err := s.BatchUpsert(ctx, items, []string{"code"}, "code"); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 {
		t.Fatalf("got %d events, want one type-wide invalidation", len(*seen))
	}
	if ev := (*seen)[0]; ev.Op != OpUpsert || !ev.Object.Empty() {
		t.Fatalf("event is not a truthful OpUpsert: %+v", ev)
	}

	*seen = (*seen)[:0]
	boom := errors.New("rollback")
	err := s.h.RunInTx(ctx, func(txCtx context.Context) error {
		if err := s.BatchUpsert(txCtx, []*Item{{Code: "BUPE3"}}, []string{"code"}, "code"); err != nil {
			return err
		}
		if len(*seen) != 0 {
			t.Fatalf("batch upsert event published before commit: %+v", *seen)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want rollback error, got %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("rollback leaked batch upsert events: %+v", *seen)
	}
}

func TestWithBus_BatchUpdatePublishesPerRowAndDropsOnRollback(t *testing.T) {
	s, seen := setupBusItemStore(t)
	ctx := context.Background()
	a := &Item{Code: "BUE1"}
	b := &Item{Code: "BUE2"}
	if err := s.BatchCreate(ctx, []*Item{a, b}); err != nil {
		t.Fatal(err)
	}
	*seen = (*seen)[:0]
	a.Code = "BUE1-updated"
	b.Code = "BUE2-updated"
	if err := s.BatchUpdate(ctx, []*Item{a, b}, "code"); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 2 {
		t.Fatalf("got %d events, want 2", len(*seen))
	}
	for i, ev := range *seen {
		if ev.Op != OpUpdate || ev.Locator.Kind == "" || ev.Changes.Empty() {
			t.Fatalf("event #%d is not OpUpdate: %+v", i, ev)
		}
	}

	*seen = (*seen)[:0]
	a.Code = "BUE1-rollback"
	b.Code = "BUE2-rollback"
	boom := errors.New("rollback")
	err := s.h.RunInTx(ctx, func(txCtx context.Context) error {
		if err := s.BatchUpdate(txCtx, []*Item{a, b}, "code"); err != nil {
			return err
		}
		if len(*seen) != 0 {
			t.Fatalf("batch update event published before commit: %+v", *seen)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want rollback error, got %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("rollback leaked batch update events: %+v", *seen)
	}
}

func TestWithBus_BatchCreatePublishesPerObjectInOrder(t *testing.T) {
	s, seen := setupBusItemStore(t)

	items := []*Item{{Code: "B1"}, {Code: "B2"}, {Code: "B3"}}
	if err := s.BatchCreate(context.Background(), items); err != nil {
		t.Fatal(err)
	}

	if len(*seen) != 3 {
		t.Fatalf("batch create must publish one event per object, got %d", len(*seen))
	}
	for i, want := range []string{"B1", "B2", "B3"} {
		if got := (*seen)[i].Object.Value().Code; got != want {
			t.Fatalf("event #%d out of order: want %s got %s", i, want, got)
		}
	}
}

// TestWithBus_TxCommitFlushesInOrder is the commit branch of the M3
// DoD trio: writes inside Store.Tx stage their events — nothing is
// visible mid-transaction, everything flushes in write order after
// COMMIT. The tx clone is driven with the caller's outer ctx on
// purpose: that is the v1 Store.Tx calling convention, and it proves
// the clone's captured txCtx anchors staging (not just ctx
// propagation).
func TestWithBus_TxCommitFlushesInOrder(t *testing.T) {
	s, seen := setupBusItemStore(t)
	ctx := context.Background()

	err := s.Tx(ctx, func(tx *Store[Item]) error {
		if err := tx.Create(ctx, &Item{Code: "T1"}); err != nil {
			return err
		}
		if err := tx.Create(ctx, &Item{Code: "T2"}); err != nil {
			return err
		}
		if len(*seen) != 0 {
			t.Error("events must not be visible before COMMIT")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(*seen) != 2 || (*seen)[0].Object.Value().Code != "T1" || (*seen)[1].Object.Value().Code != "T2" {
		t.Fatalf("commit must flush staged events in write order, got %+v", *seen)
	}
}

// TestWithBus_TxRollbackDropsEvents is the rollback branch: a failed
// transaction must not leak a single event — subscribers recording a
// write that never committed is exactly the phantom v1's in-tx hooks
// allowed and v2 forbids.
func TestWithBus_TxRollbackDropsEvents(t *testing.T) {
	s, seen := setupBusItemStore(t)
	ctx := context.Background()

	boom := errors.New("boom")
	err := s.Tx(ctx, func(tx *Store[Item]) error {
		if err := tx.Create(ctx, &Item{Code: "RB1"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected rollback error, got %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("rollback must drop staged events wholesale, got %+v", *seen)
	}
}

// TestWithBus_RunInTxContextPropagation drives the same store through
// db.RunInTx + txCtx (the cross-store transaction idiom) instead of a
// Tx clone: staging must anchor via the context alone.
func TestWithBus_RunInTxContextPropagation(t *testing.T) {
	gdb := setupHookDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}
	bus, seen := busAndLog(t)
	h := gdb
	s := New[Item](h, log.Empty(),
		WithQueryFields("id", "code"),
		WithBus(bus),
	)

	err := h.RunInTx(context.Background(), func(txCtx context.Context) error {
		if err := s.Create(txCtx, &Item{Code: "CTX1"}); err != nil {
			return err
		}
		if len(*seen) != 0 {
			t.Error("txCtx write must stage, not publish")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 || (*seen)[0].Object.Value().Code != "CTX1" {
		t.Fatalf("commit must flush the staged event, got %+v", *seen)
	}
}

// TestWithBus_ForeignHandleTx_PublishesImmediately pins the event side
// of transaction handle affinity: a store bound to handle B, called
// inside handle A's RunInTx, writes on B's own pool (autocommit) — so
// its event must publish immediately instead of staging on A's buffer,
// where A's rollback would drop the event of a committed write.
func TestWithBus_ForeignHandleTx_PublishesImmediately(t *testing.T) {
	ctx := context.Background()
	a := setupHookDB(t)
	b := setupHookDB(t)
	if err := b.Migrate(ctx, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}
	bus, seen := busAndLog(t)
	sb := New[Item](b, log.Empty(),
		WithQueryFields("id", "code"),
		WithBus(bus),
	)

	boom := errors.New("boom")
	err := a.RunInTx(ctx, func(txCtx context.Context) error {
		if err := sb.Create(txCtx, &Item{Code: "FOREIGN"}); err != nil {
			return err
		}
		if len(*seen) != 1 {
			t.Errorf("write outside A's tx must publish immediately, got %+v", *seen)
		}
		return boom // roll A back — B's committed write keeps its event
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected rollback error, got %v", err)
	}
	if n, err := sb.Count(ctx); err != nil || n != 1 {
		t.Fatalf("write must have committed on B, n=%d err=%v", n, err)
	}
	if len(*seen) != 1 || (*seen)[0].Object.Value().Code != "FOREIGN" {
		t.Fatalf("foreign rollback must not drop the committed write's event, got %+v", *seen)
	}
}

// Without WithBus the store publishes nothing — opt-in means opt-in.
func TestWithBus_NotConfigured_NoPublish(t *testing.T) {
	gdb := setupHookDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}
	s := New[Item](gdb, log.Empty(), WithQueryFields("id", "code"))
	if err := s.Create(context.Background(), &Item{Code: "SILENT"}); err != nil {
		t.Fatal(err)
	}
	// Nothing to assert beyond "no panic without a bus" — publishChanged
	// must be nil-safe.
}

// Before-hooks and the bus survive the Tx clone (struct-copy clone
// regression guard — v1 round-7 caught a field drop here).
func TestWithBus_And_BeforeHooks_PreservedAcrossTxClone(t *testing.T) {
	gdb := setupHookDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}
	bus, seen := busAndLog(t)

	var beforeCalled atomic.Int32
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithBus(bus),
		WithBeforeCreate(func(ctx context.Context, obj *Item) error {
			beforeCalled.Add(1)
			return nil
		}),
	)

	err := s.Tx(context.Background(), func(tx *Store[Item]) error {
		return tx.Create(context.Background(), &Item{Code: "TXKEEP"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if beforeCalled.Load() != 1 {
		t.Fatalf("before-hook must run inside transaction, got %d", beforeCalled.Load())
	}
	if len(*seen) != 1 {
		t.Fatalf("bus must survive the Tx clone, got %d events", len(*seen))
	}
}

// ---------------------------------------------------------------------------
// ListWithCursor basic test
// ---------------------------------------------------------------------------

func TestListWithCursor_FirstPage(t *testing.T) {
	gdb := setupHookDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&Item{})); err != nil {
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
	if err := gdb.Migrate(context.Background(), db.Table(&Item{})); err != nil {
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
