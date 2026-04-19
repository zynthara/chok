package store

import (
	"context"
	"errors"
	"testing"

	"github.com/zynthara/chok/store/where"
)

// ---------------------------------------------------------------------------
// Locator tests
// ---------------------------------------------------------------------------

func TestLocator_RID_Fetch(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	got, err := s.Get(context.Background(), RID(u.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.RID != u.RID {
		t.Fatalf("want %q, got %q", u.RID, got.RID)
	}
}

func TestLocator_ID_Fetch(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	got, err := s.Get(context.Background(), ID(u.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != u.ID {
		t.Fatalf("want %d, got %d", u.ID, got.ID)
	}
}

func TestLocator_Where_Fetch(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "alice@test.com")

	got, err := s.Get(context.Background(), Where(where.WithFilter("email", "alice@test.com")))
	if err != nil {
		t.Fatal(err)
	}
	if got.RID != u.RID {
		t.Fatalf("expected to retrieve alice's record")
	}
}

func TestLocator_Where_Empty_ErrMissingConditions(t *testing.T) {
	s, _ := setupUserStore(t)
	createUser(t, s, "alice", "a@test.com")

	_, err := s.Get(context.Background(), Where())
	if !errors.Is(err, ErrMissingConditions) {
		t.Fatalf("expected ErrMissingConditions, got %v", err)
	}
}

func TestLocator_Where_OrderOnly_ErrMissingConditions(t *testing.T) {
	s, _ := setupUserStore(t)
	createUser(t, s, "alice", "a@test.com")

	_, err := s.Get(context.Background(), Where(where.WithOrder("created_at")))
	if !errors.Is(err, ErrMissingConditions) {
		t.Fatalf("expected ErrMissingConditions, got %v", err)
	}
}

func TestLocator_RID_NotFound(t *testing.T) {
	s, _ := setupUserStore(t)
	_, err := s.Get(context.Background(), RID("usr_missing"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Changes tests
// ---------------------------------------------------------------------------

func TestSet_UnknownField_ErrUnknownUpdateField(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	err := s.Update(context.Background(), RID(u.RID), Set(map[string]any{"nope": "x"}))
	if !errors.Is(err, ErrUnknownUpdateField) {
		t.Fatalf("expected ErrUnknownUpdateField, got %v", err)
	}
}

func TestSet_Empty_ErrMissingColumns(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	err := s.Update(context.Background(), RID(u.RID), Set(map[string]any{}))
	if !errors.Is(err, ErrMissingColumns) {
		t.Fatalf("expected ErrMissingColumns, got %v", err)
	}
}

func TestFields_WithoutFieldNames_UsesFullWhitelist(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	// Modify both whitelisted columns and call Fields without listing them.
	u.Name = "Alice Updated"
	u.Email = "alice.updated@test.com"

	if err := s.Update(context.Background(), RID(u.RID), Fields(u)); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(context.Background(), RID(u.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Alice Updated" || got.Email != "alice.updated@test.com" {
		t.Fatalf("full-whitelist update did not persist: %+v", got)
	}
}

func TestFields_ZeroValuesArePersisted(t *testing.T) {
	// GORM's Updates(&struct) skips zero values by default. chok's Fields path
	// must override that so clearing a field (e.g. empty Bio) actually lands.
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	// Wipe name (zero value for string).
	u.Name = ""
	if err := s.Update(context.Background(), RID(u.RID), Fields(u, "name")); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(context.Background(), RID(u.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "" {
		t.Fatalf("expected name cleared, got %q", got.Name)
	}
}

func TestFields_UnknownField_ErrUnknownUpdateField(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	err := s.Update(context.Background(), RID(u.RID), Fields(u, "nope"))
	if !errors.Is(err, ErrUnknownUpdateField) {
		t.Fatalf("expected ErrUnknownUpdateField, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Update tests — optimistic locking
// ---------------------------------------------------------------------------

func TestUpdate_Fields_AutoLock_Success(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")
	oldVersion := u.Version

	u.Name = "Alice v2"
	if err := s.Update(context.Background(), RID(u.RID), Fields(u, "name")); err != nil {
		t.Fatal(err)
	}
	if u.Version != oldVersion+1 {
		t.Fatalf("version should bump in-memory, got %d want %d", u.Version, oldVersion+1)
	}
}

func TestUpdate_Fields_AutoLock_StaleVersion(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	// First update takes the v1 → v2 slot.
	clone := *u
	clone.Name = "winner"
	if err := s.Update(context.Background(), RID(u.RID), Fields(&clone, "name")); err != nil {
		t.Fatal(err)
	}

	// Second update with the stale original version should fail.
	u.Name = "loser"
	err := s.Update(context.Background(), RID(u.RID), Fields(u, "name"))
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("expected ErrStaleVersion, got %v", err)
	}
}

func TestUpdate_Fields_NoLock_SkipsVersionCheck(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	// Another writer increments the version out from under us.
	clone := *u
	clone.Name = "racer"
	if err := s.Update(context.Background(), RID(u.RID), Fields(&clone, "name")); err != nil {
		t.Fatal(err)
	}

	// With NoLock the stale version doesn't matter.
	u.Name = "force"
	err := s.Update(context.Background(), RID(u.RID), Fields(u, "name").NoLock())
	if err != nil {
		t.Fatalf("NoLock should bypass stale version, got %v", err)
	}
}

func TestUpdate_Set_ExplicitVersion(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	// Set + WithVersion matches current version → success, version bumps.
	err := s.Update(context.Background(), RID(u.RID),
		Set(map[string]any{"name": "via-set"}),
		WithVersion(u.Version),
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(context.Background(), RID(u.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "via-set" {
		t.Fatalf("set did not persist: got %q", got.Name)
	}
	if got.Version != u.Version+1 {
		t.Fatalf("expected DB version %d, got %d", u.Version+1, got.Version)
	}
}

func TestUpdate_Set_StaleVersion(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	err := s.Update(context.Background(), RID(u.RID),
		Set(map[string]any{"name": "x"}),
		WithVersion(u.Version+99),
	)
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("expected ErrStaleVersion, got %v", err)
	}
}

func TestUpdate_Set_NoLock_Idempotent(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	// Without WithVersion, two consecutive Sets both succeed.
	if err := s.Update(context.Background(), RID(u.RID), Set(map[string]any{"name": "v1"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(context.Background(), RID(u.RID), Set(map[string]any{"name": "v2"})); err != nil {
		t.Fatal(err)
	}
}

func TestUpdate_ID_Locator(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	err := s.Update(context.Background(), ID(u.ID), Set(map[string]any{"name": "by-id"}))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(context.Background(), ID(u.ID))
	if got.Name != "by-id" {
		t.Fatalf("expected %q, got %q", "by-id", got.Name)
	}
}

func TestUpdate_NotFound(t *testing.T) {
	s, _ := setupUserStore(t)
	err := s.Update(context.Background(), RID("usr_missing"), Set(map[string]any{"name": "x"}))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Delete tests
// ---------------------------------------------------------------------------

func TestDelete_RID_Idempotent(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	if err := s.Delete(context.Background(), RID(u.RID)); err != nil {
		t.Fatal(err)
	}
	// Second delete of the same record: idempotent.
	if err := s.Delete(context.Background(), RID(u.RID)); err != nil {
		t.Fatalf("idempotent delete should return nil, got %v", err)
	}
}

func TestDelete_ID(t *testing.T) {
	s := setupItemStore(t)
	it := &Item{Code: "A"}
	if err := s.Create(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), ID(it.ID)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get(context.Background(), ID(it.ID))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestDelete_Where_Batch(t *testing.T) {
	s := setupItemStore(t)
	for _, code := range []string{"A", "B", "C"} {
		if err := s.Create(context.Background(), &Item{Code: code}); err != nil {
			t.Fatal(err)
		}
	}

	err := s.Delete(context.Background(), Where(where.WithFilter("code", "A")))
	if err != nil {
		t.Fatal(err)
	}

	page, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 remaining, got %d", len(page.Items))
	}
}

func TestDelete_Where_Empty_ErrMissingConditions(t *testing.T) {
	s := setupItemStore(t)
	if err := s.Create(context.Background(), &Item{Code: "X"}); err != nil {
		t.Fatal(err)
	}

	err := s.Delete(context.Background(), Where())
	if !errors.Is(err, ErrMissingConditions) {
		t.Fatalf("empty Where must refuse to delete, got %v", err)
	}
}

func TestDelete_WithVersion_Success(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	err := s.Delete(context.Background(), RID(u.RID), WithVersion(u.Version))
	if err != nil {
		t.Fatal(err)
	}
}

func TestDelete_WithVersion_Stale(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	err := s.Delete(context.Background(), RID(u.RID), WithVersion(u.Version+99))
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("expected ErrStaleVersion, got %v", err)
	}
}

func TestDelete_WithVersion_NotFound(t *testing.T) {
	s, _ := setupUserStore(t)
	err := s.Delete(context.Background(), RID("usr_missing"), WithVersion(1))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tx tests
// ---------------------------------------------------------------------------

func TestTx_CommitsOnSuccess(t *testing.T) {
	s, _ := setupUserStore(t)

	err := s.Tx(context.Background(), func(tx *Store[User]) error {
		return tx.Create(context.Background(), &User{Name: "tx", Email: "tx@b.com"})
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(context.Background(), Where(where.WithFilter("email", "tx@b.com")))
	if err != nil {
		t.Fatalf("Tx commit lost: %v", err)
	}
	if got.Name != "tx" {
		t.Fatalf("unexpected record: %+v", got)
	}
}

func TestTx_RollsBackOnError(t *testing.T) {
	s, _ := setupUserStore(t)
	sentinel := errors.New("rollback")

	err := s.Tx(context.Background(), func(tx *Store[User]) error {
		if err := tx.Create(context.Background(), &User{Name: "rb", Email: "rb@b.com"}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}

	_, err = s.Get(context.Background(), Where(where.WithFilter("email", "rb@b.com")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("rollback should leave no row, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// extractModelSafe tests (unit level)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Exists tests
// ---------------------------------------------------------------------------

func TestExists_Found(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@test.com")

	ok, err := s.Exists(context.Background(), RID(u.RID))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected Exists=true for created record")
	}
}

func TestExists_NotFound(t *testing.T) {
	s, _ := setupUserStore(t)

	ok, err := s.Exists(context.Background(), RID("usr_missing"))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected Exists=false for non-existent record")
	}
}

func TestExists_Where(t *testing.T) {
	s, _ := setupUserStore(t)
	createUser(t, s, "alice", "alice@test.com")

	ok, err := s.Exists(context.Background(), Where(where.WithFilter("email", "alice@test.com")))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected Exists=true for matching filter")
	}

	ok, err = s.Exists(context.Background(), Where(where.WithFilter("email", "nobody@test.com")))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected Exists=false for non-matching filter")
	}
}

// ---------------------------------------------------------------------------
// Upsert tests
// ---------------------------------------------------------------------------

func TestUpsert_Insert(t *testing.T) {
	s := setupItemStore(t)

	item := &Item{Code: "UPS1"}
	if err := s.Upsert(context.Background(), item, []string{"code"}, "code"); err != nil {
		t.Fatal(err)
	}
	if item.RID == "" {
		t.Fatal("Upsert insert should populate RID")
	}

	got, err := s.Get(context.Background(), RID(item.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != "UPS1" {
		t.Fatalf("expected code UPS1, got %s", got.Code)
	}
}

func TestUpsert_UpdateOnConflict(t *testing.T) {
	s := setupItemStore(t)

	// First insert.
	item1 := &Item{Code: "CONFLICT"}
	if err := s.Create(context.Background(), item1); err != nil {
		t.Fatal(err)
	}

	// Upsert with same unique code — should update, not error.
	item2 := &Item{Code: "CONFLICT"}
	if err := s.Upsert(context.Background(), item2, []string{"code"}, "code"); err != nil {
		t.Fatal(err)
	}

	// Should still have exactly 1 record.
	page, err := s.List(context.Background(), where.WithCount())
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Fatalf("expected 1 record after upsert, got %d", page.Total)
	}
}

func TestUpsert_UnknownUpdateColumn(t *testing.T) {
	s := setupItemStore(t)

	item := &Item{Code: "UPS2"}
	err := s.Upsert(context.Background(), item, []string{"code"}, "nonexistent")
	if !errors.Is(err, ErrUnknownUpdateField) {
		t.Fatalf("expected ErrUnknownUpdateField, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// extractModelSafe tests (unit level)
// ---------------------------------------------------------------------------

func TestExtractModelSafe(t *testing.T) {
	u := &User{Name: "x"}
	m := extractModelSafe(u)
	if m == nil {
		t.Fatal("should extract from *User embedding SoftDeleteModel")
	}
	m.Version = 42
	if u.Version != 42 {
		t.Fatal("extractModelSafe should return a live pointer, not a copy")
	}

	if got := extractModelSafe(nil); got != nil {
		t.Fatalf("nil should yield nil, got %+v", got)
	}
	if got := extractModelSafe((*User)(nil)); got != nil {
		t.Fatalf("typed nil pointer should yield nil, got %+v", got)
	}
	if got := extractModelSafe(struct{ Name string }{}); got != nil {
		t.Fatalf("plain struct should yield nil, got %+v", got)
	}
}
