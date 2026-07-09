package store

import (
	"context"
	"errors"
	"testing"

	"github.com/zynthara/chok/v2/store/where"
)

func TestWithRowsAffected_DeleteBulkSoft(t *testing.T) {
	s, _ := setupUserStore(t)
	ctx := context.Background()
	for _, u := range []User{
		{Name: "tmp", Email: "t1@example.com"},
		{Name: "tmp", Email: "t2@example.com"},
		{Name: "keep", Email: "k@example.com"},
	} {
		if err := s.Create(ctx, &u); err != nil {
			t.Fatal(err)
		}
	}

	var n int64
	err := s.Delete(ctx, Where(where.WithFilter("name", "tmp")), WithRowsAffected(&n))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("affected = %d, want 2", n)
	}
	if left, _ := s.Count(ctx); left != 1 {
		t.Fatalf("remaining = %d, want 1", left)
	}
}

func TestWithRowsAffected_DeleteBulkHard(t *testing.T) {
	s := setupItemStore(t)
	ctx := context.Background()
	for _, code := range []string{"a", "b", "c"} {
		if err := s.Create(ctx, &Item{Code: code}); err != nil {
			t.Fatal(err)
		}
	}

	var n int64
	err := s.Delete(ctx, Where(where.WithFilterIn("code", []string{"a", "b"})), WithRowsAffected(&n))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("affected = %d, want 2 (hard-delete path)", n)
	}
}

func TestWithRowsAffected_DeleteZeroMatch(t *testing.T) {
	s, _ := setupUserStore(t)
	n := int64(99) // pre-seeded to prove the option overwrites
	err := s.Delete(context.Background(),
		Where(where.WithFilter("name", "nobody")), WithRowsAffected(&n))
	if err != nil {
		t.Fatalf("idempotent zero-match delete must return nil, got %v", err)
	}
	if n != 0 {
		t.Fatalf("affected = %d, want 0", n)
	}
}

func TestWithRowsAffected_UpdateBulkSet(t *testing.T) {
	s, _ := setupUserStore(t)
	ctx := context.Background()
	for _, u := range []User{
		{Name: "tmp", Email: "t1@example.com"},
		{Name: "tmp", Email: "t2@example.com"},
		{Name: "keep", Email: "k@example.com"},
	} {
		if err := s.Create(ctx, &u); err != nil {
			t.Fatal(err)
		}
	}

	var n int64
	err := s.Update(ctx, Where(where.WithFilter("name", "tmp")),
		Set(map[string]any{"name": "archived"}), WithRowsAffected(&n))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("affected = %d, want 2", n)
	}
}

func TestWithRowsAffected_UpdateNotFound(t *testing.T) {
	s, _ := setupUserStore(t)
	n := int64(99)
	err := s.Update(context.Background(), Where(where.WithFilter("name", "nobody")),
		Set(map[string]any{"name": "x"}), WithRowsAffected(&n))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("zero-match update must keep returning ErrNotFound, got %v", err)
	}
	if n != 0 {
		t.Fatalf("affected = %d, want 0 alongside ErrNotFound", n)
	}
}

func TestWithRowsAffected_UpdateFieldsStaleVersion(t *testing.T) {
	s, _ := setupUserStore(t)
	ctx := context.Background()
	u := &User{Name: "orig", Email: "o@example.com"}
	if err := s.Create(ctx, u); err != nil {
		t.Fatal(err)
	}

	u.Name = "changed"
	u.Version = 42 // stale on purpose — implicit lock rides Fields
	var n int64
	err := s.Update(ctx, RID(u.RID), Fields(u, "name"), WithRowsAffected(&n))
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("want ErrStaleVersion, got %v", err)
	}
	if n != 0 {
		t.Fatalf("affected = %d, want 0 on version conflict", n)
	}
}

func TestWithRowsAffected_NilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("WithRowsAffected(nil) must panic — configuration error")
		}
	}()
	WithRowsAffected(nil)
}
