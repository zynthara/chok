package store

import (
	"context"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
)

// Arch-review fix regression tests: admin roles are resolved once at
// construction (WithAdminRoles > db.store.admin_roles > deprecated global
// default) and drive BOTH the auto-detected OwnerScope bypass and the
// write-side owner fill. Before the fix the two sides could disagree —
// OwnerScope captured the global list at construction while fillOwner
// re-read it per call — and the documented per-store override
// (WithScope(OwnerScope(...))) actually intersected bypass sets.

func setupAdminRolesStore(t *testing.T, opts ...StoreOption) *Store[Product] {
	t.Helper()
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&Product{})); err != nil {
		t.Fatal(err)
	}
	base := []StoreOption{WithQueryFields("id", "name"), WithUpdateFields("name")}
	return New[Product](gdb, log.Empty(), append(base, opts...)...)
}

func TestFix_AdminRoles_OptionDrivesScopeAndOwnerFill(t *testing.T) {
	s := setupAdminRolesStore(t, WithAdminRoles("superadmin"))

	alice := userCtx("alice")
	if err := s.Create(alice, &Product{Name: "alice-widget"}); err != nil {
		t.Fatal(err)
	}

	// Read side: the per-store role bypasses the auto OwnerScope...
	page, err := s.List(userCtx("root", "superadmin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("superadmin must bypass OwnerScope, got %d items", len(page.Items))
	}
	// ...and the global-default "admin" role no longer does — the option
	// replaces the inherited list instead of stacking onto it.
	page, err = s.List(userCtx("boss", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("default admin role must not bypass an overridden list, got %d items", len(page.Items))
	}

	// Write side honours the same list: superadmin may preset OwnerID...
	imported := &Product{Name: "import"}
	imported.SetOwnerID("someone-else")
	if err := s.Create(userCtx("root", "superadmin"), imported); err != nil {
		t.Fatal(err)
	}
	if imported.OwnerID != "someone-else" {
		t.Fatalf("per-store admin preset OwnerID must be preserved, got %q", imported.OwnerID)
	}
	// ...while the default-role admin is overwritten like any non-admin.
	spoofed := &Product{Name: "spoof"}
	spoofed.SetOwnerID("victim")
	if err := s.Create(userCtx("boss", "admin"), spoofed); err != nil {
		t.Fatal(err)
	}
	if spoofed.OwnerID != "boss" {
		t.Fatalf("non-listed role preset OwnerID must be overwritten, got %q", spoofed.OwnerID)
	}
}

func TestFix_AdminRoles_PolicyInheritAndOverride(t *testing.T) {
	h := openPolicyDB(t, db.StorePolicy{AdminRoles: []string{"ops"}})
	if err := h.Migrate(context.Background(), db.Table(&Product{})); err != nil {
		t.Fatal(err)
	}
	s := New[Product](h, log.Empty(),
		WithQueryFields("id", "name"), WithUpdateFields("name"))

	if err := s.Create(userCtx("alice"), &Product{Name: "widget"}); err != nil {
		t.Fatal(err)
	}

	// Policy list reaches the store: "ops" bypasses, default "admin" doesn't.
	page, err := s.List(userCtx("op-1", "ops"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("policy admin role must bypass OwnerScope, got %d items", len(page.Items))
	}
	page, err = s.List(userCtx("boss", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("default admin role must not bypass under a policy list, got %d items", len(page.Items))
	}

	// Construction option overrides the policy.
	o := New[Product](h, log.Empty(),
		WithQueryFields("id", "name"), WithUpdateFields("name"),
		WithAdminRoles("admin"))
	page, err = o.List(userCtx("boss", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("WithAdminRoles must override the policy list, got %d items", len(page.Items))
	}
	page, err = o.List(userCtx("op-1", "ops"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("policy role must not survive a WithAdminRoles override, got %d items", len(page.Items))
	}
}

func TestFix_AdminRoles_CapturedAtConstruction(t *testing.T) {
	// Regression for the read/write drift: fillOwner used to re-read the
	// global list on every call, so SetDefaultAdminRoles after construction
	// changed write-side admin semantics while the OwnerScope kept the old
	// list. Both sides now freeze at construction.
	s := setupAdminRolesStore(t) // resolves the global default ("admin")

	SetDefaultAdminRoles("late-admin")
	t.Cleanup(func() { SetDefaultAdminRoles("admin") })

	// Write side keeps the construction-time list: "admin" is still admin...
	preset := &Product{Name: "import"}
	preset.SetOwnerID("someone-else")
	if err := s.Create(userCtx("root", "admin"), preset); err != nil {
		t.Fatal(err)
	}
	if preset.OwnerID != "someone-else" {
		t.Fatalf("construction-time admin must keep preset OwnerID, got %q", preset.OwnerID)
	}
	// ...and the late role is not admin on this store.
	late := &Product{Name: "late"}
	late.SetOwnerID("victim")
	if err := s.Create(userCtx("newcomer", "late-admin"), late); err != nil {
		t.Fatal(err)
	}
	if late.OwnerID != "newcomer" {
		t.Fatalf("post-construction role must not gain admin on an existing store, got %q", late.OwnerID)
	}

	// Read side agrees: "admin" bypasses, "late-admin" sees only its own rows.
	page, err := s.List(userCtx("root", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("construction-time admin must see all rows, got %d", len(page.Items))
	}
	page, err = s.List(userCtx("other", "late-admin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("post-construction role must stay scoped, got %d items", len(page.Items))
	}
}

func TestFix_AdminRoles_EmptyListFailsClosed(t *testing.T) {
	// WithAdminRoles() with no arguments removes every bypass on this store:
	// nobody escapes the owner filter, nobody may preset OwnerID.
	s := setupAdminRolesStore(t, WithAdminRoles())

	if err := s.Create(userCtx("alice"), &Product{Name: "widget"}); err != nil {
		t.Fatal(err)
	}

	page, err := s.List(userCtx("boss", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("empty admin list must disable scope bypass, got %d items", len(page.Items))
	}

	preset := &Product{Name: "spoof"}
	preset.SetOwnerID("victim")
	if err := s.Create(userCtx("boss", "admin"), preset); err != nil {
		t.Fatal(err)
	}
	if preset.OwnerID != "boss" {
		t.Fatalf("empty admin list must overwrite preset OwnerID, got %q", preset.OwnerID)
	}
}
