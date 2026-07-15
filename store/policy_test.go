package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// db-layer review #2 regression tests: the handle's db.store policy is
// the app-level default for strict / require-principal / page sizes,
// and construction options — the Without* opt-outs included — override
// it per store. The rest of the suite runs on zero-policy handles, so
// "zero value changes nothing" is guarded by every other test file.

// openPolicyDB opens a real in-memory handle carrying a db.store
// policy block. Policy tests build their own handle instead of riding
// dbtest.Open because the subject is Open-time plumbing (Options →
// handle → store construction), which is driver-orthogonal.
func openPolicyDB(t *testing.T, pol db.StorePolicy) *db.DB {
	t.Helper()
	h, err := db.Open(db.Options{Driver: "sqlite",
		SQLite: db.SQLiteOptions{Path: ":memory:"}, Store: pol})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestStorePolicy_StrictDefault_RejectsAutoDiscovery(t *testing.T) {
	h := openPolicyDB(t, db.StorePolicy{Strict: true})
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("policy strict must reject an auto-discovered field surface at construction")
		}
		if msg := r.(string); !strings.Contains(msg, "db.store policy") {
			t.Fatalf("panic should point operators at the policy origin, got: %v", msg)
		}
	}()
	_ = New[Product](h, log.Empty()) // Product declares no field surface
}

func TestStorePolicy_StrictDefault_DeclaredSurfacesPass(t *testing.T) {
	h := openPolicyDB(t, db.StorePolicy{Strict: true})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("declared field surfaces must satisfy policy strict, got panic: %v", r)
		}
	}()
	// Explicit options — and the tag path the blog quickstart rides.
	s := New[Product](h, log.Empty(),
		WithQueryFields("id", "name"), WithUpdateFields("name"))
	if !s.strict {
		t.Fatal("policy strict must reach the constructed store")
	}
	tagged := New[TaggedPost](h, log.Empty())
	if !tagged.strict {
		t.Fatal("policy strict must reach a tag-declared store")
	}
}

func TestStorePolicy_WithoutStrict_OptsOut(t *testing.T) {
	h := openPolicyDB(t, db.StorePolicy{Strict: true})
	// Auto-discovery would panic under the inherited strict; the
	// explicit opt-out must construct (and warn, as in dev).
	s := New[Product](h, log.Empty(), WithoutStrict())
	if s.strict {
		t.Fatal("WithoutStrict must override the policy default")
	}
}

func TestStorePolicy_RequirePrincipal_FailsClosed(t *testing.T) {
	h := openPolicyDB(t, db.StorePolicy{RequirePrincipal: true})
	if err := h.Migrate(context.Background(), db.Table(&Product{})); err != nil {
		t.Fatal(err)
	}
	s := New[Product](h, log.Empty(),
		WithQueryFields("id", "name"), WithUpdateFields("name"))

	err := s.Create(context.Background(), &Product{Name: "widget"})
	if !errors.Is(err, apierr.ErrUnauthenticated) {
		t.Fatalf("ownerless create under policy require_principal: want ErrUnauthenticated, got %v", err)
	}
	if err := s.Create(userCtx("alice"), &Product{Name: "widget"}); err != nil {
		t.Fatalf("authenticated create must still work: %v", err)
	}
}

func TestStorePolicy_WithoutRequirePrincipal_OptsOut(t *testing.T) {
	h := openPolicyDB(t, db.StorePolicy{RequirePrincipal: true})
	if err := h.Migrate(context.Background(), db.Table(&Product{})); err != nil {
		t.Fatal(err)
	}
	s := New[Product](h, log.Empty(),
		WithQueryFields("id", "name"), WithUpdateFields("name"),
		WithoutRequirePrincipal())
	if err := s.Create(context.Background(), &Product{Name: "widget"}); err != nil {
		t.Fatalf("opt-out must restore the legacy ownerless create: %v", err)
	}
}

func TestStorePolicy_PageSizes_InheritAndOverride(t *testing.T) {
	h := openPolicyDB(t, db.StorePolicy{MaxPageSize: 2, DefaultPageSize: 1})
	if err := h.Migrate(context.Background(), db.Table(&Product{})); err != nil {
		t.Fatal(err)
	}
	s := New[Product](h, log.Empty(),
		WithQueryFields("id", "name"), WithUpdateFields("name"))
	if s.maxPageSize != 2 || s.defaultPageSize != 1 {
		t.Fatalf("policy page sizes must reach the store: max=%d default=%d", s.maxPageSize, s.defaultPageSize)
	}

	// And through to the SQL LIMIT: an oversized request comes back
	// clamped to the policy cap (the envelope a handler echoes is its
	// own concern — the row count is the store's).
	alice := userCtx("alice")
	for _, name := range []string{"a", "b", "c"} {
		if err := s.Create(alice, &Product{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.List(alice, where.WithPage(1, 10))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("policy max_page_size=2 must clamp the query: got %d items", len(page.Items))
	}
	page, err = s.List(alice, where.WithMaxPageSize(50_000))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("call-site max page size must not raise Store policy: got %d items", len(page.Items))
	}
	// An explicit zero is an override (unlimited / package default),
	// not "unset": the call site must be able to escape the policy.
	o := New[Product](h, log.Empty(),
		WithQueryFields("id", "name"), WithUpdateFields("name"),
		WithMaxPageSize(0), WithDefaultPageSize(0))
	if o.maxPageSize != 0 || o.defaultPageSize != 0 {
		t.Fatalf("explicit zero must override the policy: max=%d default=%d", o.maxPageSize, o.defaultPageSize)
	}
}
