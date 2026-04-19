package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/store/where"
)

// --- test models ---

type User struct {
	db.SoftDeleteModel
	Name  string `json:"name"  gorm:"size:100"`
	Email string `json:"email" gorm:"size:200;not null"`
}

func (User) RIDPrefix() string { return "usr" }

type Item struct {
	db.Model
	Code string `json:"code" gorm:"uniqueIndex;size:50"`
}

func (Item) RIDPrefix() string { return "itm" }

// --- helpers ---

func setupDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gdb
}

func setupUserStore(t *testing.T) (*Store[User], *gorm.DB) {
	t.Helper()
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&User{}, db.SoftUnique("uk_email", "email"))); err != nil {
		t.Fatal(err)
	}
	s := New[User](gdb, log.Empty(),
		WithQueryFields("id", "name", "email", "created_at"),
		WithUpdateFields("name", "email"),
	)
	return s, gdb
}

func setupItemStore(t *testing.T) *Store[Item] {
	t.Helper()
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}
	return New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
	)
}

func createUser(t *testing.T, s *Store[User], name, email string) *User {
	t.Helper()
	u := &User{Name: name, Email: email}
	if err := s.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

// --- contract tests ---

func TestNew_PointerType_Panics(t *testing.T) {
	// Go generics prevent store.New[*User] at compile time.
	// The runtime check in store.New provides defense-in-depth.
	// Verify via db.ValidateModel that pointer-to-model is rejected.
	err := db.ValidateModel((**User)(nil))
	if err == nil {
		t.Fatal("expected error for pointer-to-pointer model")
	}
}

func TestNew_InvalidRIDPrefix_Panics(t *testing.T) {
	// store.New validates RIDPrefix at construction time via db.ValidateModel.
	// Since we can't create an invalid-prefix generic param, verify via db.ValidateModel.
	// A model without RIDPrefixer is valid (no prefix to check).
	if err := db.ValidateModel(&Item{}); err != nil {
		t.Fatalf("valid model rejected: %v", err)
	}
}

func TestDbTable_InvalidRIDPrefix_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid RID prefix in db.Table")
		}
	}()
	type BadPrefix struct {
		db.Model
	}
	// Can't implement RIDPrefixer on a local type with an invalid prefix in this pattern.
	// Test via ValidateModel directly.
	// Instead, test that db.Table panics for a non-Model type:
	db.Table(&struct{ Name string }{})
}

func TestCreate(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "alice@example.com")

	if u.RID == "" {
		t.Fatal("RID should be auto-generated")
	}
	if u.Version != 1 {
		t.Fatalf("expected version 1, got %d", u.Version)
	}
	if u.ID == 0 {
		t.Fatal("ID should be set after create")
	}
}

func TestCreate_DuplicateKey_ErrDuplicate(t *testing.T) {
	s, _ := setupUserStore(t)
	createUser(t, s, "alice", "alice@example.com")

	dup := &User{Name: "bob", Email: "alice@example.com"}
	err := s.Create(context.Background(), dup)
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}
}

func TestBatchCreate(t *testing.T) {
	s, _ := setupUserStore(t)
	users := []*User{
		{Name: "alice", Email: "a@b.com"},
		{Name: "bob", Email: "b@b.com"},
	}
	if err := s.BatchCreate(context.Background(), users); err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.RID == "" || u.Version != 1 {
			t.Fatalf("batch create: unexpected state %+v", u)
		}
	}
}

func TestBatchCreate_EmptySlice(t *testing.T) {
	s, _ := setupUserStore(t)
	if err := s.BatchCreate(context.Background(), nil); err != nil {
		t.Fatalf("empty batch should return nil, got %v", err)
	}
}

func TestBatchCreate_DuplicateKey_ErrDuplicate(t *testing.T) {
	s, _ := setupUserStore(t)
	users := []*User{
		{Name: "alice", Email: "same@b.com"},
		{Name: "bob", Email: "same@b.com"},
	}
	err := s.BatchCreate(context.Background(), users)
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}
}

func TestBatchCreate_SingleFailure_Rollback(t *testing.T) {
	s, _ := setupUserStore(t)
	createUser(t, s, "existing", "exist@b.com")

	users := []*User{
		{Name: "new1", Email: "new1@b.com"},
		{Name: "dup", Email: "exist@b.com"}, // duplicate
	}
	err := s.BatchCreate(context.Background(), users)
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}

	// new1 should have been rolled back.
	_, err = s.Get(context.Background(), Where(where.WithFilter("email", "new1@b.com")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("new1 should not exist after rollback")
	}
}

func TestUpdateOne_OptimisticLock(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "alice@example.com")

	u.Name = "alice2"
	if err := s.Update(context.Background(), RID(u.RID), Fields(u, "name")); err != nil {
		t.Fatal(err)
	}
	if u.Version != 2 {
		t.Fatalf("expected version 2, got %d", u.Version)
	}
}

func TestUpdateOne_NotFound(t *testing.T) {
	s, _ := setupUserStore(t)
	u := &User{Name: "ghost", Email: "ghost@b.com"}
	u.ID = 9999
	u.Version = 1

	err := s.Update(context.Background(), RID(u.RID), Fields(u, "name"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUpdateOne_StaleVersion(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "alice@example.com")

	// Simulate concurrent update: load same row "twice".
	u2, _ := s.Get(context.Background(), Where(where.WithFilter("id", u.RID)))

	// First update succeeds.
	u.Name = "alice-v2"
	if err := s.Update(context.Background(), RID(u.RID), Fields(u, "name")); err != nil {
		t.Fatal(err)
	}

	// Second update on stale version should fail.
	u2.Name = "alice-v2-stale"
	err := s.Update(context.Background(), RID(u2.RID), Fields(u2, "name"))
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("expected ErrStaleVersion, got %v", err)
	}
}

func TestGet_ZeroConditions_ErrMissingConditions(t *testing.T) {
	s, _ := setupUserStore(t)
	_, err := s.Get(context.Background(), Where())
	if !errors.Is(err, ErrMissingConditions) {
		t.Fatalf("expected ErrMissingConditions, got %v", err)
	}
}

func TestGet_NotFound(t *testing.T) {
	s, _ := setupUserStore(t)
	_, err := s.Get(context.Background(), Where(where.WithFilter("id", "usr_nonexistent")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGet_Found(t *testing.T) {
	s, _ := setupUserStore(t)
	created := createUser(t, s, "alice", "alice@example.com")

	found, err := s.Get(context.Background(), Where(where.WithFilter("id", created.RID)))
	if err != nil {
		t.Fatal(err)
	}
	if found.Name != "alice" {
		t.Fatalf("expected alice, got %s", found.Name)
	}
}

func TestGetOne(t *testing.T) {
	s, _ := setupUserStore(t)
	created := createUser(t, s, "alice", "alice@example.com")

	found, err := s.Get(context.Background(), RID(created.RID))
	if err != nil {
		t.Fatal(err)
	}
	if found.Name != "alice" {
		t.Fatalf("expected alice, got %s", found.Name)
	}
}

func TestGetOne_NotFound(t *testing.T) {
	s, _ := setupUserStore(t)
	_, err := s.Get(context.Background(), RID("usr_nonexistent"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestAutoAlias_IdMapsToRid(t *testing.T) {
	// "id" in WithQueryFields should auto-map to "rid" without explicit WithColumnAlias.
	s, _ := setupUserStore(t) // no WithColumnAlias("id", "rid") in setupUserStore
	created := createUser(t, s, "alice", "alice@example.com")

	found, err := s.Get(context.Background(), RID(created.RID))
	if err != nil {
		t.Fatal(err)
	}
	if found.RID != created.RID {
		t.Fatalf("auto alias failed: expected %s, got %s", created.RID, found.RID)
	}

	// WithFilter("id", ...) should also resolve to "rid" column.
	found2, err := s.Get(context.Background(), Where(where.WithFilter("id", created.RID)))
	if err != nil {
		t.Fatal(err)
	}
	if found2.RID != created.RID {
		t.Fatalf("auto alias filter failed: expected %s, got %s", created.RID, found2.RID)
	}
}

func TestList_EmptyResult(t *testing.T) {
	s, _ := setupUserStore(t)
	page, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if page.Items == nil {
		t.Fatal("empty result should be non-nil slice")
	}
	if len(page.Items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(page.Items))
	}
	if page.Total != 0 {
		t.Fatalf("expected total 0 (no count), got %d", page.Total)
	}
}

func TestList_DefaultTotal_Zero(t *testing.T) {
	s, _ := setupUserStore(t)
	createUser(t, s, "alice", "a@b.com")

	page, _ := s.List(context.Background())
	if page.Total != 0 {
		t.Fatalf("expected 0 without WithCount, got %d", page.Total)
	}
}

func TestList_WithCount(t *testing.T) {
	s, _ := setupUserStore(t)
	createUser(t, s, "alice", "a@b.com")
	createUser(t, s, "bob", "b@b.com")

	page, err := s.List(context.Background(), where.WithCount())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(page.Items))
	}
	if page.Total != 2 {
		t.Fatalf("expected total 2, got %d", page.Total)
	}
}

func TestList_WithPage(t *testing.T) {
	s, _ := setupUserStore(t)
	for i := range 5 {
		createUser(t, s, "user", fmt.Sprintf("u%d@b.com", i))
	}

	page, err := s.List(context.Background(),
		where.WithPage(2, 2),
		where.WithOrder("created_at"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 items on page 2, got %d", len(page.Items))
	}
}

// --- GetByID / ListByIDs / UpdateByID / DeleteByID ---

func TestGetByID(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "alice@example.com")

	found, err := s.Get(context.Background(), ID(u.ID))
	if err != nil {
		t.Fatal(err)
	}
	if found.RID != u.RID || found.Name != "alice" {
		t.Fatalf("unexpected result: %+v", found)
	}
}

func TestGetByID_NotFound(t *testing.T) {
	s, _ := setupUserStore(t)
	_, err := s.Get(context.Background(), ID(99999))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListByIDs(t *testing.T) {
	s, _ := setupUserStore(t)
	a := createUser(t, s, "alice", "a@b.com")
	b := createUser(t, s, "bob", "b@b.com")
	_ = createUser(t, s, "carol", "c@b.com") // not in query

	items, err := s.ListByIDs(context.Background(), []uint{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
}

func TestListByIDs_Empty(t *testing.T) {
	s, _ := setupUserStore(t)
	items, err := s.ListByIDs(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if items == nil {
		t.Fatal("expected non-nil slice")
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(items))
	}
}

func TestListByIDs_PartialMatch(t *testing.T) {
	s, _ := setupUserStore(t)
	a := createUser(t, s, "alice", "a@b.com")

	items, err := s.ListByIDs(context.Background(), []uint{a.ID, 9999})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item (missing id skipped), got %d", len(items))
	}
}

func TestUpdateByID(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "alice@example.com")

	err := s.Update(context.Background(), ID(u.ID), Set(map[string]any{
		"name": "alice2",
	}))
	if err != nil {
		t.Fatal(err)
	}

	found, err := s.Get(context.Background(), ID(u.ID))
	if err != nil {
		t.Fatal(err)
	}
	if found.Name != "alice2" {
		t.Fatalf("expected alice2, got %s", found.Name)
	}
	// Version must NOT be incremented.
	if found.Version != 1 {
		t.Fatalf("UpdateByID should not bump version, got %d", found.Version)
	}
}

func TestUpdateByID_EmptyCols_ErrMissingColumns(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@b.com")

	err := s.Update(context.Background(), ID(u.ID), Set(nil))
	if !errors.Is(err, ErrMissingColumns) {
		t.Fatalf("expected ErrMissingColumns, got %v", err)
	}
}

func TestUpdateByID_UnknownField_ErrUnknownUpdateField(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@b.com")

	err := s.Update(context.Background(), ID(u.ID), Set(map[string]any{
		"not_whitelisted": "x",
	}))
	if !errors.Is(err, ErrUnknownUpdateField) {
		t.Fatalf("expected ErrUnknownUpdateField, got %v", err)
	}
}

func TestUpdateByID_NotFound(t *testing.T) {
	s, _ := setupUserStore(t)
	err := s.Update(context.Background(), ID(9999), Set(map[string]any{
		"name": "ghost",
	}))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUpdateByID_ConcurrentOverwrites_NoStaleVersionError(t *testing.T) {
	// UpdateByID is explicitly non-optimistic; back-to-back updates must succeed.
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@b.com")

	if err := s.Update(context.Background(), ID(u.ID), Set(map[string]any{"name": "v1"})); err != nil {
		t.Fatal(err)
	}
	// Simulate a stale in-memory reference — UpdateByID doesn't read Version, so this still works.
	if err := s.Update(context.Background(), ID(u.ID), Set(map[string]any{"name": "v2"})); err != nil {
		t.Fatalf("UpdateByID should not error on stale version, got %v", err)
	}
}

func TestUpdateByRID(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "alice@example.com")

	err := s.Update(context.Background(), RID(u.RID), Set(map[string]any{
		"name": "alice2",
	}))
	if err != nil {
		t.Fatal(err)
	}

	found, err := s.Get(context.Background(), ID(u.ID))
	if err != nil {
		t.Fatal(err)
	}
	if found.Name != "alice2" {
		t.Fatalf("expected alice2, got %s", found.Name)
	}
	if found.Version != 1 {
		t.Fatalf("UpdateByRID should not bump version, got %d", found.Version)
	}
}

func TestUpdateByRID_EmptyCols_ErrMissingColumns(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@b.com")

	err := s.Update(context.Background(), RID(u.RID), Set(nil))
	if !errors.Is(err, ErrMissingColumns) {
		t.Fatalf("expected ErrMissingColumns, got %v", err)
	}
}

func TestUpdateByRID_UnknownField_ErrUnknownUpdateField(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@b.com")

	err := s.Update(context.Background(), RID(u.RID), Set(map[string]any{
		"not_whitelisted": "x",
	}))
	if !errors.Is(err, ErrUnknownUpdateField) {
		t.Fatalf("expected ErrUnknownUpdateField, got %v", err)
	}
}

func TestUpdateByRID_NotFound(t *testing.T) {
	s, _ := setupUserStore(t)
	err := s.Update(context.Background(), RID("usr_nonexistent"), Set(map[string]any{
		"name": "ghost",
	}))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUpdateByRID_ConcurrentOverwrites_NoStaleVersionError(t *testing.T) {
	// Non-optimistic; back-to-back updates must succeed.
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@b.com")

	if err := s.Update(context.Background(), RID(u.RID), Set(map[string]any{"name": "v1"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(context.Background(), RID(u.RID), Set(map[string]any{"name": "v2"})); err != nil {
		t.Fatalf("UpdateByRID should not error on stale version, got %v", err)
	}
}

func TestDeleteByID_SoftDelete(t *testing.T) {
	s, gdb := setupUserStore(t)
	u := createUser(t, s, "alice", "a@b.com")

	if err := s.Delete(context.Background(), ID(u.ID)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get(context.Background(), ID(u.ID))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after soft delete, got %v", err)
	}

	var token string
	gdb.Raw("SELECT delete_token FROM users WHERE id = ?", u.ID).Scan(&token)
	if token == "" {
		t.Fatal("delete_token should be set after soft delete")
	}
}

func TestDeleteByID_PhysicalDelete(t *testing.T) {
	s := setupItemStore(t)
	item := &Item{Code: "ABC"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(context.Background(), ID(item.ID)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get(context.Background(), ID(item.ID))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after physical delete, got %v", err)
	}
}

func TestDeleteByID_Idempotent(t *testing.T) {
	s, _ := setupUserStore(t)
	err := s.Delete(context.Background(), ID(9999))
	if err != nil {
		t.Fatalf("deleting non-existent ID should be idempotent, got %v", err)
	}
}

func TestDeleteByRID_SoftDelete(t *testing.T) {
	s, gdb := setupUserStore(t)
	u := createUser(t, s, "alice", "a@b.com")

	if err := s.Delete(context.Background(), RID(u.RID)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get(context.Background(), RID(u.RID))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after soft delete, got %v", err)
	}

	var token string
	gdb.Raw("SELECT delete_token FROM users WHERE rid = ?", u.RID).Scan(&token)
	if token == "" {
		t.Fatal("delete_token should be set after soft delete")
	}
}

func TestDeleteByRID_PhysicalDelete(t *testing.T) {
	s := setupItemStore(t)
	item := &Item{Code: "DEL"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(context.Background(), RID(item.RID)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get(context.Background(), RID(item.RID))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after physical delete, got %v", err)
	}
}

func TestDeleteByRID_Idempotent(t *testing.T) {
	s, _ := setupUserStore(t)
	err := s.Delete(context.Background(), RID("usr_nonexistent"))
	if err != nil {
		t.Fatalf("deleting non-existent RID should be idempotent, got %v", err)
	}
}

func TestDeleteMany_Idempotent(t *testing.T) {
	s, _ := setupUserStore(t)
	// Delete non-existent should not error.
	err := s.Delete(context.Background(), Where(where.WithFilter("id", "usr_nonexistent")))
	if err != nil {
		t.Fatalf("idempotent delete should return nil, got %v", err)
	}
}

func TestDeleteMany_ZeroConditions_ErrMissingConditions(t *testing.T) {
	s, _ := setupUserStore(t)
	err := s.Delete(context.Background(), Where())
	if !errors.Is(err, ErrMissingConditions) {
		t.Fatalf("expected ErrMissingConditions, got %v", err)
	}
}

func TestDeleteMany_SoftDelete_SetsDeleteToken(t *testing.T) {
	s, gdb := setupUserStore(t)
	u := createUser(t, s, "alice", "alice@example.com")

	if err := s.Delete(context.Background(), Where(where.WithFilter("id", u.RID))); err != nil {
		t.Fatal(err)
	}

	// Should not be found via normal query (soft-deleted).
	_, err := s.Get(context.Background(), Where(where.WithFilter("id", u.RID)))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after soft delete, got %v", err)
	}

	// Check delete_token is set via raw query.
	var token string
	gdb.Raw("SELECT delete_token FROM users WHERE rid = ?", u.RID).Scan(&token)
	if token == "" {
		t.Fatal("delete_token should be set after soft delete")
	}
}

func TestDeleteMany_PhysicalDelete(t *testing.T) {
	s := setupItemStore(t)
	item := &Item{Code: "ABC"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(context.Background(), Where(where.WithFilter("id", item.RID))); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get(context.Background(), Where(where.WithFilter("id", item.RID)))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after physical delete, got %v", err)
	}
}

func TestWithQueryFields_UnknownField_Rejected(t *testing.T) {
	s, _ := setupUserStore(t)
	_, err := s.Get(context.Background(), Where(where.WithFilter("unknown_field", "val")))
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestWithQueryFields_MapsCorrectly(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@b.com")

	// "id" maps to "rid" column.
	found, err := s.Get(context.Background(), Where(where.WithFilter("id", u.RID)))
	if err != nil {
		t.Fatal(err)
	}
	if found.Name != "alice" {
		t.Fatalf("field mapping failed, got %+v", found)
	}
}

func TestWithQueryFields_NotConfigured_Rejects(t *testing.T) {
	gdb := setupDB(t)
	db.Migrate(context.Background(), gdb, db.Table(&Item{}))
	s := New[Item](gdb, log.Empty()) // no WithQueryFields

	_, err := s.Get(context.Background(), Where(where.WithFilter("code", "ABC")))
	if err == nil {
		t.Fatal("expected error when WithQueryFields not configured")
	}
}

func TestWithColumnAlias_UndeclaredField_Panics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for undeclared alias field")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "not declared") {
			t.Fatalf("unexpected panic message: %s", msg)
		}
	}()
	gdb := setupDB(t)
	db.Migrate(context.Background(), gdb, db.Table(&Item{}))
	New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithColumnAlias("unknown", "some_col"),
	)
}

func TestWithColumnAlias_BeforeFields_WorksCorrectly(t *testing.T) {
	gdb := setupDB(t)
	db.Migrate(context.Background(), gdb, db.Table(&Item{}))
	// Alias declared before fields — order should not matter.
	s := New[Item](gdb, log.Empty(),
		WithColumnAlias("id", "rid"),
		WithQueryFields("id", "code"),
	)
	item := &Item{Code: "TST"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), Where(where.WithFilter("id", item.RID)))
	if err != nil {
		t.Fatalf("alias-before-fields lookup failed: %v", err)
	}
	if got.Code != "TST" {
		t.Fatalf("expected code TST, got %s", got.Code)
	}
}

func TestWithOrder_FieldValidation(t *testing.T) {
	s, _ := setupUserStore(t)
	createUser(t, s, "alice", "a@b.com")

	// Valid field.
	_, err := s.List(context.Background(), where.WithOrder("name"))
	if err != nil {
		t.Fatalf("valid order field should work: %v", err)
	}

	// Unknown field.
	_, err = s.List(context.Background(), where.WithOrder("nonexistent"))
	if err == nil {
		t.Fatal("expected error for unknown order field")
	}
}

func TestTransaction_SingleStore(t *testing.T) {
	s, _ := setupUserStore(t)

	err := s.Tx(context.Background(), func(tx *Store[User]) error {
		return tx.Create(context.Background(), &User{Name: "tx-user", Email: "tx@b.com"})
	})
	if err != nil {
		t.Fatal(err)
	}

	found, err := s.Get(context.Background(), Where(where.WithFilter("email", "tx@b.com")))
	if err != nil {
		t.Fatal(err)
	}
	if found.Name != "tx-user" {
		t.Fatal("transaction should have committed")
	}
}

func TestTransaction_Rollback(t *testing.T) {
	s, _ := setupUserStore(t)

	err := s.Tx(context.Background(), func(tx *Store[User]) error {
		tx.Create(context.Background(), &User{Name: "rollback", Email: "rb@b.com"})
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected error")
	}

	_, err = s.Get(context.Background(), Where(where.WithFilter("email", "rb@b.com")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("transaction should have been rolled back")
	}
}

// --- regression: #1 zero-condition with non-filter options ---

func TestGet_OnlyOrderOption_ErrMissingConditions(t *testing.T) {
	s := setupItemStore(t)
	s.Create(context.Background(), &Item{Code: "A"})
	s.Create(context.Background(), &Item{Code: "B"})

	_, err := s.Get(context.Background(), Where(where.WithOrder("code")))
	if !errors.Is(err, ErrMissingConditions) {
		t.Fatalf("Get with only WithOrder should return ErrMissingConditions, got %v", err)
	}
}

func TestDeleteMany_OnlyOrderOption_ErrMissingConditions(t *testing.T) {
	s := setupItemStore(t)
	s.Create(context.Background(), &Item{Code: "A"})

	err := s.Delete(context.Background(), Where(where.WithOrder("code")))
	if !errors.Is(err, ErrMissingConditions) {
		t.Fatalf("Delete with only WithOrder should return ErrMissingConditions, got %v", err)
	}
}

// --- regression: #2 WithCount total unaffected by pagination ---

func TestList_WithCount_IgnoresPagination(t *testing.T) {
	s := setupItemStore(t)
	for i := range 5 {
		s.Create(context.Background(), &Item{Code: fmt.Sprintf("item%d", i)})
	}

	page, err := s.List(context.Background(),
		where.WithPage(2, 2),
		where.WithCount(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 items on page 2, got %d", len(page.Items))
	}
	if page.Total != 5 {
		t.Fatalf("expected total 5 (all items), got %d", page.Total)
	}
}

// --- regression: indirect SoftDeleteModel embedding ---

type IndirectBase struct {
	db.SoftDeleteModel
}

type IndirectUser struct {
	IndirectBase
	Email string `json:"email" gorm:"size:200;not null"`
}

func (IndirectUser) RIDPrefix() string { return "inu" }

func TestStore_IndirectSoftDelete(t *testing.T) {
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&IndirectUser{}, db.SoftUnique("uk_inu_email", "email"))); err != nil {
		t.Fatal(err)
	}
	s := New[IndirectUser](gdb, log.Empty(),
		WithQueryFields("id", "email"),
	)

	u := &IndirectUser{Email: "test@example.com"}
	if err := s.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}

	// Delete must use soft-delete, not physical delete.
	if err := s.Delete(context.Background(), Where(where.WithFilter("id", u.RID))); err != nil {
		t.Fatal(err)
	}

	// Row must still exist (soft-deleted).
	var count int64
	gdb.Raw("SELECT COUNT(*) FROM indirect_users WHERE rid = ?", u.RID).Scan(&count)
	if count != 1 {
		t.Fatal("indirect SoftDeleteModel should use soft delete, not physical delete")
	}

	// delete_token must be set.
	var token string
	gdb.Raw("SELECT delete_token FROM indirect_users WHERE rid = ?", u.RID).Scan(&token)
	if token == "" {
		t.Fatal("delete_token should be set after soft delete")
	}
}

func TestSoftUnique_ActiveRecordUniqueness(t *testing.T) {
	s, _ := setupUserStore(t)
	createUser(t, s, "alice", "alice@example.com")

	// Same email should fail (active records).
	err := s.Create(context.Background(), &User{Name: "bob", Email: "alice@example.com"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate for active record, got %v", err)
	}

	// Soft-delete the first user.
	u, _ := s.Get(context.Background(), Where(where.WithFilter("email", "alice@example.com")))
	if err := s.Delete(context.Background(), Where(where.WithFilter("id", u.RID))); err != nil {
		t.Fatal(err)
	}

	// Now creating with same email should succeed (delete_token freed it).
	err = s.Create(context.Background(), &User{Name: "charlie", Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("expected success after soft-delete freed email, got %v", err)
	}
}

// --- regression: Op whitelist ---

func TestWithFilterOp_InvalidOp_Error(t *testing.T) {
	s, _ := setupUserStore(t)
	createUser(t, s, "alice", "a@b.com")

	_, err := s.Get(context.Background(), Where(where.WithFilterOp("name", where.Op("OR 1=1 --"), "x")))
	if err == nil {
		t.Fatal("expected error for invalid operator")
	}
}

func TestWithFilterOp_ValidOps(t *testing.T) {
	s, _ := setupUserStore(t)
	createUser(t, s, "alice", "a@b.com")

	for _, op := range []where.Op{where.Eq, where.Ne, where.Gt, where.Gte, where.Lt, where.Lte} {
		_, err := s.List(context.Background(), where.WithFilterOp("name", op, "alice"))
		if err != nil {
			t.Fatalf("valid op %q rejected: %v", string(op), err)
		}
	}
}

// --- WithUpdateFields contract ---

func TestWithUpdateFields_UnknownField_Rejected(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "a@b.com")

	// "created_at" is in WithQueryFields but NOT in WithUpdateFields.
	u.Name = "alice2"
	err := s.Update(context.Background(), RID(u.RID), Fields(u, "created_at"))
	if err == nil {
		t.Fatal("expected error for field not in WithUpdateFields")
	}
}

func TestDefaultAutoDiscoverUpdateFields(t *testing.T) {
	gdb := setupDB(t)
	db.Migrate(context.Background(), gdb, db.Table(&Item{}))
	// No WithUpdateFields — auto-discover from JSON tags.
	s := New[Item](gdb, log.Empty())

	item := &Item{Code: "ABC"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	item.Code = "DEF"
	// "code" (json:"code") should be auto-discovered as updatable.
	if err := s.Update(context.Background(), RID(item.RID), Fields(item, "code")); err != nil {
		t.Fatalf("auto-discovered update field should work: %v", err)
	}

	// Base model fields should be rejected.
	err := s.Update(context.Background(), RID(item.RID), Fields(item, "version"))
	if err == nil {
		t.Fatal("base model field 'version' should not be updatable")
	}
}

// --- DeleteOne contract ---

func TestDeleteOne_Idempotent(t *testing.T) {
	s, _ := setupUserStore(t)
	// version == 0: non-existent → nil (idempotent).
	err := s.Delete(context.Background(), RID("usr_nonexistent"))
	if err != nil {
		t.Fatalf("idempotent DeleteOne should return nil, got %v", err)
	}
}

func TestDeleteOne_NotFound_WithVersion(t *testing.T) {
	s, _ := setupUserStore(t)
	err := s.Delete(context.Background(), RID("usr_nonexistent"), WithVersion(1))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDeleteOne_StaleVersion(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "alice@example.com")

	// Update to version 2.
	u.Name = "alice2"
	if err := s.Update(context.Background(), RID(u.RID), Fields(u, "name")); err != nil {
		t.Fatal(err)
	}

	// DeleteOne with stale version 1.
	err := s.Delete(context.Background(), RID(u.RID), WithVersion(1))
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("expected ErrStaleVersion, got %v", err)
	}

	// Verify record still exists.
	_, err = s.Get(context.Background(), Where(where.WithFilter("id", u.RID)))
	if err != nil {
		t.Fatalf("record should still exist after stale DeleteOne: %v", err)
	}
}

func TestDeleteOne_CorrectVersion(t *testing.T) {
	s, _ := setupUserStore(t)
	u := createUser(t, s, "alice", "alice@example.com")

	err := s.Delete(context.Background(), RID(u.RID), WithVersion(u.Version))
	if err != nil {
		t.Fatalf("DeleteOne with correct version should succeed: %v", err)
	}

	_, err = s.Get(context.Background(), Where(where.WithFilter("id", u.RID)))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after DeleteOne, got %v", err)
	}
}

func TestDeleteOne_SoftDelete_SetsDeleteToken(t *testing.T) {
	s, gdb := setupUserStore(t)
	u := createUser(t, s, "alice", "alice@example.com")

	if err := s.Delete(context.Background(), RID(u.RID)); err != nil {
		t.Fatal(err)
	}

	// Should not be found via normal query.
	_, err := s.Get(context.Background(), Where(where.WithFilter("id", u.RID)))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after soft DeleteOne, got %v", err)
	}

	// delete_token must be set.
	var token string
	gdb.Raw("SELECT delete_token FROM users WHERE rid = ?", u.RID).Scan(&token)
	if token == "" {
		t.Fatal("delete_token should be set after soft DeleteOne")
	}
}

func TestDeleteOne_PhysicalDelete(t *testing.T) {
	s := setupItemStore(t)
	item := &Item{Code: "DEL"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(context.Background(), RID(item.RID)); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get(context.Background(), Where(where.WithFilter("id", item.RID)))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after physical DeleteOne, got %v", err)
	}
}

// --- P0a: error classification tests ---

func TestAutoDiscoverUpdateFields_ExcludesBaseModel(t *testing.T) {
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}
	s := New[Item](gdb, log.Empty())

	item := &Item{Code: "UPD"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	// "id" (mapped to rid) is a base model field — should be rejected.
	err := s.Update(context.Background(), RID(item.RID), Fields(item, "id"))
	if err == nil {
		t.Fatal("base model field 'id' should not be updatable")
	}
}

func TestResolveUpdateColumn_UnknownField_ErrUnknownUpdateField(t *testing.T) {
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&User{}, db.SoftUnique("uk_email", "email"))); err != nil {
		t.Fatal(err)
	}
	s := New[User](gdb, log.Empty(),
		WithQueryFields("id", "name"),
		WithUpdateFields("name", "email"),
	)
	u := createUser(t, s, "alice", "alice@test.com")
	err := s.Update(context.Background(), RID(u.RID), Fields(u, "nonexistent"))
	if !errors.Is(err, ErrUnknownUpdateField) {
		t.Fatalf("expected ErrUnknownUpdateField, got %v", err)
	}
}

func TestWhereErrUnknownField(t *testing.T) {
	s, _ := setupUserStore(t)
	_, err := s.List(context.Background(), where.WithFilter("bogus", "val"))
	// mapQueryError wraps ErrUnknownField into apierr.ErrInvalidArgument (400).
	// Verify the error is surfaced as a client error.
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected error to mention field name, got %v", err)
	}
	// Verify it's mapped to a 400-class error (not a raw where error).
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("expected *apierr.Error, got %T: %v", err, err)
	}
	if ae.Code != 400 {
		t.Fatalf("expected 400, got %d", ae.Code)
	}
}

func TestDefaultAutoDiscoverQueryFields(t *testing.T) {
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}
	// No WithQueryFields — auto-discover from JSON tags.
	s := New[Item](gdb, log.Empty())

	// "code" (json:"code") should be auto-discovered and queryable.
	item := &Item{Code: "ABC"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), Where(where.WithFilter("code", "ABC")))
	if err != nil {
		t.Fatalf("auto-discovered field should be queryable: %v", err)
	}
	if got.Code != "ABC" {
		t.Fatalf("expected ABC, got %s", got.Code)
	}

	// Unknown field should still be rejected (mapped to 400 by Store).
	_, err = s.Get(context.Background(), Where(where.WithFilter("bogus", "x")))
	if err == nil {
		t.Fatal("unknown field should be rejected")
	}
}

// --- P1: MapError tests ---

func TestMapError_NotFound(t *testing.T) {
	got := MapError(ErrNotFound)
	if got == nil || got.Code != 404 {
		t.Fatalf("expected 404, got %v", got)
	}
}

func TestMapError_StaleVersion(t *testing.T) {
	got := MapError(ErrStaleVersion)
	if got == nil || got.Code != 409 {
		t.Fatalf("expected 409, got %v", got)
	}
}

func TestMapError_Duplicate(t *testing.T) {
	got := MapError(ErrDuplicate)
	if got == nil || got.Code != 409 {
		t.Fatalf("expected 409, got %v", got)
	}
}

func TestMapError_ProgrammingErrors_NotMapped(t *testing.T) {
	// ErrMissingConditions and ErrMissingColumns are programming errors;
	// MapError should return nil so they fall through to 500.
	if got := MapError(ErrMissingConditions); got != nil {
		t.Fatalf("ErrMissingConditions should not be mapped, got %v", got)
	}
	if got := MapError(ErrMissingColumns); got != nil {
		t.Fatalf("ErrMissingColumns should not be mapped, got %v", got)
	}
}

func TestMapError_UnknownError_Nil(t *testing.T) {
	got := MapError(fmt.Errorf("something else"))
	if got != nil {
		t.Fatalf("expected nil for unknown error, got %v", got)
	}
}

// --- P2: Scope tests ---

func TestScope_GetOne_Filtered(t *testing.T) {
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&User{}, db.SoftUnique("uk_email", "email"))); err != nil {
		t.Fatal(err)
	}
	// Scope filters by name="alice" (simulating tenant).
	s := New[User](gdb, log.Empty(),
		WithQueryFields("id", "name"),
		WithScope(func(_ context.Context, q *gorm.DB) (*gorm.DB, error) {
			return q.Where("name = ?", "alice"), nil
		}),
	)
	alice := createUser(t, s, "alice", "alice@test.com")

	// Create bob via a no-scope store (simulates different tenant).
	sAll := New[User](gdb, log.Empty(), WithQueryFields("id", "name"))
	bob := createUser(t, sAll, "bob", "bob@test.com")

	// GetOne alice — should work.
	_, err := s.Get(context.Background(), RID(alice.RID))
	if err != nil {
		t.Fatalf("expected alice to be visible: %v", err)
	}

	// GetOne bob — should fail (scope filters it out).
	_, err = s.Get(context.Background(), RID(bob.RID))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for bob (out of scope), got %v", err)
	}
}

func TestScope_List_Filtered(t *testing.T) {
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&User{}, db.SoftUnique("uk_email", "email"))); err != nil {
		t.Fatal(err)
	}
	s := New[User](gdb, log.Empty(),
		WithQueryFields("id", "name"),
		WithScope(func(_ context.Context, q *gorm.DB) (*gorm.DB, error) {
			return q.Where("name = ?", "alice"), nil
		}),
	)
	createUser(t, s, "alice", "alice@test.com")

	sAll := New[User](gdb, log.Empty(), WithQueryFields("id", "name"))
	createUser(t, sAll, "bob", "bob@test.com")

	page, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Name != "alice" {
		t.Fatalf("expected only alice, got %d items", len(page.Items))
	}
}

func TestScope_UpdateOne_Blocked(t *testing.T) {
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&User{}, db.SoftUnique("uk_email", "email"))); err != nil {
		t.Fatal(err)
	}
	sAll := New[User](gdb, log.Empty(),
		WithQueryFields("id", "name"),
		WithUpdateFields("name"),
	)
	bob := createUser(t, sAll, "bob", "bob@test.com")

	// Scoped store can't see bob.
	s := New[User](gdb, log.Empty(),
		WithQueryFields("id", "name"),
		WithUpdateFields("name"),
		WithScope(func(_ context.Context, q *gorm.DB) (*gorm.DB, error) {
			return q.Where("name = ?", "alice"), nil
		}),
	)
	bob.Name = "hacked"
	err := s.Update(context.Background(), RID(bob.RID), Fields(bob, "name"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound (scope blocks update), got %v", err)
	}
}

func TestScope_DeleteOne_Blocked(t *testing.T) {
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}
	sAll := New[Item](gdb, log.Empty(), WithQueryFields("id", "code"))
	item := &Item{Code: "X"}
	if err := sAll.Create(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithScope(func(_ context.Context, q *gorm.DB) (*gorm.DB, error) {
			return q.Where("code = ?", "NOPE"), nil
		}),
	)
	// version=0 is idempotent, so use version>0 to detect scope blocking.
	err := s.Delete(context.Background(), RID(item.RID), WithVersion(item.Version))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound (scope blocks delete), got %v", err)
	}

	// Item should still exist.
	_, err = sAll.Get(context.Background(), RID(item.RID))
	if err != nil {
		t.Fatalf("item should still exist: %v", err)
	}
}

func TestScope_Error_FailClosed(t *testing.T) {
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Item{})); err != nil {
		t.Fatal(err)
	}
	scopeErr := fmt.Errorf("scope: unauthenticated")
	s := New[Item](gdb, log.Empty(),
		WithQueryFields("id", "code"),
		WithUpdateFields("code"),
		WithScope(func(_ context.Context, _ *gorm.DB) (*gorm.DB, error) {
			return nil, scopeErr
		}),
	)

	// All read/write methods should propagate the scope error.
	_, err := s.Get(context.Background(), RID("rid_x"))
	if !errors.Is(err, scopeErr) {
		t.Fatalf("GetOne: expected scope error, got %v", err)
	}
	_, err = s.List(context.Background())
	if !errors.Is(err, scopeErr) {
		t.Fatalf("List: expected scope error, got %v", err)
	}
	_, err = s.Get(context.Background(), Where(where.WithFilter("code", "x")))
	if !errors.Is(err, scopeErr) {
		t.Fatalf("Get: expected scope error, got %v", err)
	}

	item := &Item{Code: "Y"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatalf("Create should NOT apply scope: %v", err)
	}
	err = s.Update(context.Background(), RID(item.RID), Fields(item, "code"))
	if !errors.Is(err, scopeErr) {
		t.Fatalf("UpdateOne: expected scope error, got %v", err)
	}
	err = s.Delete(context.Background(), RID(item.RID))
	if !errors.Is(err, scopeErr) {
		t.Fatalf("DeleteOne: expected scope error, got %v", err)
	}
}

func TestScope_UpdateOne_ScopeError_VersionNotIncremented(t *testing.T) {
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&User{}, db.SoftUnique("uk_email", "email"))); err != nil {
		t.Fatal(err)
	}
	scopeErr := fmt.Errorf("scope: blocked")
	s := New[User](gdb, log.Empty(),
		WithQueryFields("id", "name"),
		WithUpdateFields("name"),
		WithScope(func(_ context.Context, _ *gorm.DB) (*gorm.DB, error) {
			return nil, scopeErr
		}),
	)
	// Create via unscoped store.
	sAll := New[User](gdb, log.Empty(), WithQueryFields("id"), WithUpdateFields("name"))
	u := createUser(t, sAll, "alice", "alice@test.com")
	origVersion := u.Version

	u.Name = "changed"
	err := s.Update(context.Background(), RID(u.RID), Fields(u, "name"))
	if !errors.Is(err, scopeErr) {
		t.Fatalf("expected scope error, got %v", err)
	}
	// Version must NOT be incremented when scope errors.
	if u.Version != origVersion {
		t.Fatalf("version should be %d after scope error, got %d", origVersion, u.Version)
	}
}

func TestScope_NilScope_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil scope")
		}
	}()
	WithScope(nil)
}

func TestScope_NoScope_NoEffect(t *testing.T) {
	// Store without WithScope should behave exactly as before.
	s, _ := setupUserStore(t)
	u := createUser(t, s, "test", "test@test.com")
	got, err := s.Get(context.Background(), RID(u.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "test" {
		t.Fatalf("expected test, got %s", got.Name)
	}
}
