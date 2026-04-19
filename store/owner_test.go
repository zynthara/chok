package store

import (
	"context"
	"errors"
	"testing"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/log"
)

// --- test model with Owned mixin ---

type Product struct {
	db.OwnedModel
	Name string `json:"name" gorm:"size:100;not null"`
}

func (Product) RIDPrefix() string { return "prd" }

// --- helpers ---

func setupProductStore(t *testing.T, scopes ...ScopeFunc) *Store[Product] {
	t.Helper()
	gdb := setupDB(t)
	if err := db.Migrate(context.Background(), gdb, db.Table(&Product{})); err != nil {
		t.Fatal(err)
	}
	opts := []StoreOption{
		WithQueryFields("id", "name"),
		WithUpdateFields("name"),
	}
	for _, s := range scopes {
		opts = append(opts, WithScope(s))
	}
	return New[Product](gdb, log.Empty(), opts...)
}

func userCtx(subject string, roles ...string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		Subject: subject,
		Roles:   roles,
	})
}

// --- OwnerScope tests ---

func TestOwnerScope_Unauthenticated_FailClosed(t *testing.T) {
	s := setupProductStore(t, OwnerScope("admin"))

	// Create a product via admin context.
	adminCtx := userCtx("admin-1", "admin")
	p := &Product{Name: "widget"}
	if err := s.Create(adminCtx, p); err != nil {
		t.Fatal(err)
	}

	// Unauthenticated context should fail.
	_, err := s.Get(context.Background(), RID(p.RID))
	if !errors.As(err, new(*apierr.Error)) {
		t.Fatalf("expected apierr, got %v", err)
	}
}

func TestOwnerScope_UserSeesOwnRecords(t *testing.T) {
	s := setupProductStore(t, OwnerScope("admin"))

	aliceCtx := userCtx("alice")
	bobCtx := userCtx("bob")

	// Alice creates a product.
	pa := &Product{Name: "alice-widget"}
	if err := s.Create(aliceCtx, pa); err != nil {
		t.Fatal(err)
	}

	// Bob creates a product.
	pb := &Product{Name: "bob-widget"}
	if err := s.Create(bobCtx, pb); err != nil {
		t.Fatal(err)
	}

	// Alice can see her own product.
	got, err := s.Get(aliceCtx, RID(pa.RID))
	if err != nil {
		t.Fatalf("alice should see own product: %v", err)
	}
	if got.Name != "alice-widget" {
		t.Fatalf("expected alice-widget, got %s", got.Name)
	}

	// Alice cannot see Bob's product.
	_, err = s.Get(aliceCtx, RID(pb.RID))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("alice should not see bob's product, got %v", err)
	}

	// Alice's list only returns her product.
	page, err := s.List(aliceCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Name != "alice-widget" {
		t.Fatalf("expected 1 alice product, got %d", len(page.Items))
	}
}

func TestOwnerScope_AdminSeesAll(t *testing.T) {
	s := setupProductStore(t, OwnerScope("admin"))

	aliceCtx := userCtx("alice")
	adminCtx := userCtx("admin-1", "admin")

	pa := &Product{Name: "alice-widget"}
	if err := s.Create(aliceCtx, pa); err != nil {
		t.Fatal(err)
	}
	pb := &Product{Name: "bob-widget"}
	if err := s.Create(userCtx("bob"), pb); err != nil {
		t.Fatal(err)
	}

	// Admin can see all products.
	page, err := s.List(adminCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("admin should see 2 products, got %d", len(page.Items))
	}

	// Admin can get any product.
	got, err := s.Get(adminCtx, RID(pa.RID))
	if err != nil {
		t.Fatalf("admin should see alice's product: %v", err)
	}
	if got.Name != "alice-widget" {
		t.Fatalf("expected alice-widget, got %s", got.Name)
	}
}

func TestOwnerScope_UpdateBlocked(t *testing.T) {
	s := setupProductStore(t, OwnerScope("admin"))

	aliceCtx := userCtx("alice")
	bobCtx := userCtx("bob")

	p := &Product{Name: "alice-widget"}
	if err := s.Create(aliceCtx, p); err != nil {
		t.Fatal(err)
	}

	// Bob cannot update Alice's product.
	p.Name = "hacked"
	err := s.Update(bobCtx, RID(p.RID), Fields(p, "name"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob should not update alice's product, got %v", err)
	}
}

func TestOwnerScope_DeleteBlocked(t *testing.T) {
	s := setupProductStore(t, OwnerScope("admin"))

	aliceCtx := userCtx("alice")
	bobCtx := userCtx("bob")

	p := &Product{Name: "alice-widget"}
	if err := s.Create(aliceCtx, p); err != nil {
		t.Fatal(err)
	}

	// Bob cannot delete Alice's product.
	err := s.Delete(bobCtx, RID(p.RID), WithVersion(p.Version))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob should not delete alice's product, got %v", err)
	}

	// Product should still exist for Alice.
	_, err = s.Get(aliceCtx, RID(p.RID))
	if err != nil {
		t.Fatalf("product should still exist: %v", err)
	}
}

// --- Auto-fill OwnerID tests ---

func TestCreate_AutoFillsOwnerID(t *testing.T) {
	s := setupProductStore(t)

	ctx := userCtx("alice")
	p := &Product{Name: "widget"}
	if err := s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	if p.OwnerID != "alice" {
		t.Fatalf("expected OwnerID=alice, got %q", p.OwnerID)
	}
}

func TestCreate_PresetOwnerID_NonAdminOverwritten(t *testing.T) {
	// Security: a non-admin authenticated caller must not be able to spoof
	// OwnerID by pre-setting it on the struct. fillOwner overwrites the
	// attacker-provided value with the principal's Subject.
	s := setupProductStore(t)

	ctx := userCtx("alice")
	p := &Product{Name: "widget"}
	p.SetOwnerID("victim")
	if err := s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	if p.OwnerID != "alice" {
		t.Fatalf("non-admin preset OwnerID must be overwritten with principal subject, got %q", p.OwnerID)
	}
}

func TestCreate_PresetOwnerID_AdminPreserved(t *testing.T) {
	// Admin escape hatch: principals in the global admin-role set may set
	// OwnerID to any value (used for imports, backfills, cross-user ops).
	s := setupProductStore(t)

	SetDefaultAdminRoles("admin")
	t.Cleanup(func() { SetDefaultAdminRoles("admin") }) // restore default

	ctx := userCtx("root", "admin")
	p := &Product{Name: "widget"}
	p.SetOwnerID("explicit-owner")
	if err := s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	if p.OwnerID != "explicit-owner" {
		t.Fatalf("admin preset OwnerID must be preserved, got %q", p.OwnerID)
	}
}

func TestCreate_PresetOwnerID_NoPrincipalPreserved(t *testing.T) {
	// No principal in ctx (e.g. background job, tests): fillOwner is a
	// no-op — preset OwnerID is preserved. Ownership enforcement for
	// HTTP paths must come from Authn middleware upstream.
	s := setupProductStore(t)

	p := &Product{Name: "widget"}
	p.SetOwnerID("explicit-owner")
	if err := s.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	if p.OwnerID != "explicit-owner" {
		t.Fatalf("no-principal preset OwnerID must be preserved, got %q", p.OwnerID)
	}
}

func TestBatchCreate_AutoFillsOwnerID(t *testing.T) {
	s := setupProductStore(t)

	ctx := userCtx("bob")
	products := []*Product{
		{Name: "widget-1"},
		{Name: "widget-2"},
	}
	if err := s.BatchCreate(ctx, products); err != nil {
		t.Fatal(err)
	}

	for i, p := range products {
		if p.OwnerID != "bob" {
			t.Fatalf("products[%d]: expected OwnerID=bob, got %q", i, p.OwnerID)
		}
	}
}

func TestCreate_NoAuth_OwnerIDEmpty(t *testing.T) {
	// Without OwnerScope, Create on unauthenticated context leaves OwnerID empty.
	// This will fail on DB NOT NULL — that's correct (fail-closed at DB level).
	gdb := setupDB(t)
	// Use a table without NOT NULL to test the fill logic in isolation.
	gdb.Exec("CREATE TABLE products (id INTEGER PRIMARY KEY, rid TEXT UNIQUE, version INTEGER DEFAULT 1, created_at DATETIME, updated_at DATETIME, owner_id TEXT, name TEXT)")

	s := New[Product](gdb, log.Empty(), WithQueryFields("id", "name"))

	p := &Product{Name: "orphan"}
	if err := s.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	if p.OwnerID != "" {
		t.Fatalf("expected empty OwnerID without auth, got %q", p.OwnerID)
	}
}

// --- Non-owned model is unaffected ---

func TestCreate_NonOwnedModel_Unaffected(t *testing.T) {
	// Item does not embed db.Owned. Create should work without auth context.
	s := setupItemStore(t)
	item := &Item{Code: "ABC"}
	if err := s.Create(context.Background(), item); err != nil {
		t.Fatalf("non-owned model should not require auth: %v", err)
	}
}

// --- ScopedDB ---

func TestScopedDB_AppliesOwnerScope(t *testing.T) {
	s := setupProductStore(t)

	aliceCtx := userCtx("alice")
	bobCtx := userCtx("bob")

	if err := s.Create(aliceCtx, &Product{Name: "alice-a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(bobCtx, &Product{Name: "bob-b"}); err != nil {
		t.Fatal(err)
	}

	// Alice's ScopedDB should only see her record.
	q, err := s.ScopedDB(aliceCtx)
	if err != nil {
		t.Fatal(err)
	}
	var items []Product
	if err := q.Find(&items).Error; err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "alice-a" {
		t.Fatalf("alice scoped db should see only her record, got %+v", items)
	}
}

func TestScopedDB_UnauthenticatedFailsClosed(t *testing.T) {
	s := setupProductStore(t)

	_, err := s.ScopedDB(context.Background())
	if err == nil {
		t.Fatal("expected ScopedDB to fail without principal in ctx")
	}
	// Should be apierr.ErrUnauthenticated (from auto OwnerScope).
	if !errors.As(err, new(*apierr.Error)) {
		t.Fatalf("expected apierr, got %v", err)
	}
}

func TestScopedDB_NonOwnedModel_NoError(t *testing.T) {
	// Item has no OwnerScope — ScopedDB works without auth.
	s := setupItemStore(t)
	q, err := s.ScopedDB(context.Background())
	if err != nil {
		t.Fatalf("non-owned model ScopedDB should not require auth: %v", err)
	}
	if q == nil {
		t.Fatal("ScopedDB returned nil DB without error")
	}
}
