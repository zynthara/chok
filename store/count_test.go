package store

import (
	"context"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

func TestCount_ScopeAndFilter(t *testing.T) {
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&restoreDoc{})); err != nil {
		t.Fatal(err)
	}
	s := New[restoreDoc](gdb, log.Empty())

	alice, bob := userCtx("usr_alice"), userCtx("usr_bob")
	for _, d := range []struct {
		ctx   context.Context
		title string
	}{
		{alice, "draft"}, {alice, "final"}, {bob, "draft"},
	} {
		if err := s.Create(d.ctx, &restoreDoc{Title: d.title}); err != nil {
			t.Fatal(err)
		}
	}

	// No options: everything the caller's scope can see.
	if n, err := s.Count(alice); err != nil || n != 2 {
		t.Fatalf("alice total = %d, %v; want 2", n, err)
	}
	if n, err := s.Count(bob); err != nil || n != 1 {
		t.Fatalf("bob total = %d, %v; want 1", n, err)
	}
	// Filters resolve against the query allowlist.
	if n, err := s.Count(alice, where.WithFilter("title", "draft")); err != nil || n != 1 {
		t.Fatalf("alice drafts = %d, %v; want 1", n, err)
	}
	// Pagination options are stripped, not smuggled into COUNT.
	if n, err := s.Count(alice, where.WithPage(1, 1)); err != nil || n != 2 {
		t.Fatalf("count with pagination = %d, %v; want 2 (page size must not cap it)", n, err)
	}
}

func TestCount_TracksSoftDeleteAndRestore(t *testing.T) {
	s, _ := setupUserStore(t)
	ctx := context.Background()

	u := &User{Name: "carol", Email: "carol@example.com"}
	if err := s.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx); n != 1 {
		t.Fatalf("count after create = %d, want 1", n)
	}
	if err := s.Delete(ctx, RID(u.RID)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx); n != 0 {
		t.Fatalf("count must exclude soft-deleted rows, got %d", n)
	}
	if err := s.Restore(ctx, RID(u.RID)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx); n != 1 {
		t.Fatalf("count after restore = %d, want 1", n)
	}
}

func TestCount_UnknownFieldRejected(t *testing.T) {
	s, _ := setupUserStore(t)
	if _, err := s.Count(context.Background(), where.WithFilter("nope", 1)); err == nil {
		t.Fatal("undeclared filter field must be rejected")
	}
}
