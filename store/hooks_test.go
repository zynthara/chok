package store_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

func TestHooks_RunBeforeWritesAndCanAbort(t *testing.T) {
	d := openDB(t)
	ctx := t.Context()
	veto := errors.New("veto")
	var calls []string
	s := store.New[post](d, store.Hooks(store.HookSet[post]{
		BeforeCreate: func(_ context.Context, p *post) error {
			calls = append(calls, "create "+p.Title)
			if p.Title == "forbidden" {
				return veto
			}
			p.Status = "from hook"
			return nil
		},
		BeforeUpdate: func(_ context.Context, loc store.Locator, changes map[string]any) error {
			calls = append(calls, "update "+loc.String())
			if changes["views"] == 13 {
				return veto
			}
			return nil
		},
		BeforeDelete: func(_ context.Context, loc store.Locator) error {
			calls = append(calls, "delete "+loc.String())
			return veto
		},
	}))
	p := &post{Title: "ok"}
	mustCreate(t, ctx, s, p)
	if p.Status != "from hook" {
		t.Fatalf("a create hook may change the row: %+v", p)
	}
	if err := s.Create(ctx, &post{Title: "forbidden"}); !errors.Is(err, veto) {
		t.Fatalf("vetoed create: %v", err)
	}
	if err := s.BatchCreate(ctx, []*post{{Title: "fine"}, {Title: "forbidden"}}); !errors.Is(err, veto) {
		t.Fatalf("vetoed batch: %v", err)
	}
	if n, _ := s.Count(ctx); n != 1 {
		t.Fatalf("vetoed rows are not written: %d", n)
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Set(map[string]any{"views": 2})); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Set(map[string]any{"views": 13})); !errors.Is(err, veto) {
		t.Fatalf("vetoed update: %v", err)
	}
	if got, _ := s.Get(ctx, store.RID(p.RID)); got.Views != 2 || got.Version != 2 {
		t.Fatalf("a vetoed update writes nothing: %+v", got)
	}
	if err := s.Delete(ctx, store.RID(p.RID)); !errors.Is(err, veto) {
		t.Fatalf("vetoed delete: %v", err)
	}
	if ok, _ := s.Exists(ctx, store.RID(p.RID)); !ok {
		t.Fatal("a vetoed delete deletes nothing")
	}
	want := []string{"create ok", "create forbidden", "create fine", "create forbidden",
		"update rid " + p.RID, "update rid " + p.RID, "delete rid " + p.RID}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls %q, want %q", calls, want)
	}
}

// TestHooks_UpdateSeesTheChanges: BeforeUpdate gets the resolved changes,
// field name to the value about to be written, from Set and from Patch
// alike — and a copy, so the hook cannot rewrite the update behind the
// validation.
func TestHooks_UpdateSeesTheChanges(t *testing.T) {
	var seen []map[string]any
	s := posts(t, store.Hooks(store.HookSet[post]{
		BeforeUpdate: func(_ context.Context, _ store.Locator, changes map[string]any) error {
			seen = append(seen, maps.Clone(changes))
			changes["status"] = "tampered"
			return nil
		},
	}))
	ctx := t.Context()
	p := &post{Title: "a"}
	mustCreate(t, ctx, s, p)
	if err := s.Update(ctx, store.RID(p.RID), store.Set(map[string]any{"status": "published", "views": 3})); err != nil {
		t.Fatal(err)
	}
	title, empty := "b", ""
	req := &struct {
		ID    string  `path:"id"`
		Title *string `json:"title"`
		Body  *string `json:"body"`
		Score *int    `json:"-"`
	}{ID: p.RID, Title: &title, Body: &empty}
	got, _ := s.Get(ctx, store.RID(p.RID))
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(req).Onto(got)); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"status": "published", "views": 3}, {"title": "b", "body": ""}}
	if len(seen) != 2 || !maps.Equal(seen[0], want[0]) || !maps.Equal(seen[1], want[1]) {
		t.Fatalf("snapshots %v, want %v", seen, want)
	}
	if got, _ = s.Get(ctx, store.RID(p.RID)); got.Status != "published" || got.Title != "b" {
		t.Fatalf("the hook's edits are not written: %+v", got)
	}
}

// TestHooks_ValidationFirst: a malformed change fails before the hook,
// so hooks only ever see writable changes.
func TestHooks_ValidationFirst(t *testing.T) {
	ran := false
	s := posts(t, store.Hooks(store.HookSet[post]{
		BeforeUpdate: func(context.Context, store.Locator, map[string]any) error { ran = true; return nil },
	}))
	ctx := t.Context()
	p := &post{Title: "x"}
	mustCreate(t, ctx, s, p)
	if err := s.Update(ctx, store.RID(p.RID), store.Set(map[string]any{"nope": 1})); err == nil {
		t.Fatal("unknown field")
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Set(nil)); err == nil {
		t.Fatal("empty set")
	}
	bad := &struct {
		Internal *string `json:"internal"`
	}{new("x")}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(bad).Onto(p)); err == nil {
		t.Fatal("a DTO naming a field the store does not update")
	}
	if ran {
		t.Fatal("the hook ran for an invalid change")
	}
	if err := s.Update(ctx, store.Where(where.Eq("title", "x")), store.Set(map[string]any{"views": 1})); err != nil || !ran {
		t.Fatalf("valid change: %v, hook ran %v", err, ran)
	}
}
