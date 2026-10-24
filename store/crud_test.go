package store_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/store/where"
)

func TestCreate_FillsTheBase(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	p := &post{Title: "hello"}
	if err := s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.RID, "pst_") || p.ID == 0 || p.Version != 1 || p.CreatedAt.IsZero() || p.Status != "draft" {
		t.Fatalf("created %+v", p)
	}
	if err := s.Create(ctx, nil); err == nil {
		t.Fatal("a nil object is an error")
	}
}

func TestCreate_DuplicateNamesTheField(t *testing.T) {
	d := openDB(t)
	ctx := t.Context()
	s, notes := store.New[post](d), store.New[note](d)
	mustCreate(t, ctx, s, &post{Title: "same"})
	err := s.Create(ctx, &post{Title: "same"})
	var dup *store.DuplicateError
	if !errors.As(err, &dup) || dup.Field != "title" || !errors.Is(err, chok.ErrConflict) {
		t.Fatalf("SoftUnique duplicate: %#v", err)
	}
	if e := dup.APIError(); e.Code != 409 || e.Metadata["field"] != "title" {
		t.Fatalf("envelope %+v", e)
	}
	// A plain unique index is resolved by its columns.
	mustCreate(t, ctx, notes, &note{Code: "x"})
	err = notes.Create(ctx, &note{Code: "x"})
	if !errors.As(err, &dup) || dup.Field != "code" {
		t.Fatalf("uniqueIndex duplicate: %#v", err)
	}
	// Constraints names the field of a constraint; SQLite reports the
	// columns instead of the name.
	named := store.New[note](d, store.Constraints(map[string]string{"uk_note_code": "the_code", "code": "the_code"}))
	if err := named.Create(ctx, &note{Code: "x"}); !errors.As(err, &dup) || dup.Field != "the_code" {
		t.Fatalf("declared constraint: %#v", err)
	}
}

func TestBatchCreate_IsAtomic(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	if err := s.BatchCreate(ctx, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	batch := []*post{{Title: "a"}, {Title: "b"}, {Title: "c"}}
	if err := s.BatchCreate(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if batch[0].RID == "" || batch[0].RID == batch[2].RID || batch[2].ID == 0 {
		t.Fatalf("every row is filled: %+v", batch)
	}
	err := s.BatchCreate(ctx, []*post{{Title: "d"}, {Title: "a"}})
	var dup *store.DuplicateError
	if !errors.As(err, &dup) || dup.Field != "title" {
		t.Fatalf("duplicate of a stored row: %v", err)
	}
	if err := s.BatchCreate(ctx, []*post{{Title: "x"}, {Title: "x"}}); !errors.As(err, &dup) {
		t.Fatalf("duplicate within the batch: %v", err)
	}
	if n, _ := s.Count(ctx); n != 3 {
		t.Fatalf("a failed batch writes nothing: %d rows", n)
	}
	if err := s.BatchCreate(ctx, []*post{{Title: "e"}, nil}); err == nil {
		t.Fatal("a nil item is an error")
	}
}

func TestGet_ByLocator(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	p := &post{Title: "find me", Status: "published"}
	mustCreate(t, ctx, s, p, &post{Title: "other"})
	for _, loc := range []store.Locator{store.RID(p.RID), store.ID(p.ID), store.Where(where.Eq("title", "find me"))} {
		got, err := s.Get(ctx, loc)
		if err != nil || got.RID != p.RID || got.Status != "published" {
			t.Fatalf("Get(%s) = %+v, %v", loc, got, err)
		}
	}
	_, err := s.Get(ctx, store.RID("pst_missing"))
	var nf *store.NotFoundError
	if !errors.As(err, &nf) || !errors.Is(err, chok.ErrNotFound) || !strings.Contains(err.Error(), "pst_missing") {
		t.Fatalf("missing: %v", err)
	}
	for _, loc := range []store.Locator{store.ID(99_999), store.Where(where.Eq("title", "nobody"))} {
		if _, err := s.Get(ctx, loc); !errors.As(err, &nf) {
			t.Fatalf("missing %s: %v", loc, err)
		}
	}
	if _, err := s.Get(ctx, store.Where()); !errors.As(err, new(*store.MissingConditionsError)) {
		t.Fatalf("Where without filters: %v", err)
	}
	if _, err := s.Get(ctx, store.Where(where.Order("title"))); !errors.As(err, new(*store.MissingConditionsError)) {
		t.Fatalf("Where with only an order: %v", err)
	}
	// Extra filters narrow a Get.
	if _, err := s.Get(ctx, store.RID(p.RID), where.Eq("status", "draft")); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("filtered out: %v", err)
	}
}

func TestExists(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	p := &post{Title: "x"}
	mustCreate(t, ctx, s, p)
	if ok, err := s.Exists(ctx, store.RID(p.RID)); !ok || err != nil {
		t.Fatalf("exists: %v %v", ok, err)
	}
	if ok, err := s.Exists(ctx, store.Where(where.Eq("title", "y"))); ok || err != nil {
		t.Fatalf("absent: %v %v", ok, err)
	}
	if err := s.Delete(ctx, store.RID(p.RID)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Exists(ctx, store.RID(p.RID)); ok {
		t.Fatal("a soft-deleted row does not exist")
	}
}

func TestCount_FiltersAndSoftDelete(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	mustCreate(t, ctx, s, &post{Title: "a", Status: "published"}, &post{Title: "b"}, &post{Title: "c"})
	if n, err := s.Count(ctx, where.Eq("status", "draft"), where.Page(1, 1), where.Order("title")); err != nil || n != 2 {
		t.Fatalf("count ignores pagination: %d %v", n, err)
	}
	if err := s.Delete(ctx, store.Where(where.Eq("title", "b"))); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx); n != 2 {
		t.Fatalf("after delete: %d", n)
	}
	if n, _ := s.Count(ctx, where.WithTrashed()); n != 3 {
		t.Fatalf("with trashed: %d", n)
	}
	if n, _ := s.Count(ctx, where.OnlyTrashed()); n != 1 {
		t.Fatalf("only trashed: %d", n)
	}
	if err := s.Restore(ctx, store.Where(where.Eq("title", "b"))); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx); n != 3 {
		t.Fatalf("after restore: %d", n)
	}
	if _, err := s.Count(ctx, where.Eq("nope", 1)); !errors.Is(err, where.ErrUnknownField) || !errors.Is(err, chok.ErrBadRequest) {
		t.Fatalf("unknown field: %v", err)
	}
}

func TestList_PagesOrdersAndCounts(t *testing.T) {
	s := posts(t, store.Policy(storePolicy(3, 5)))
	ctx := t.Context()
	for _, title := range []string{"e", "b", "d", "a", "c", "f", "g"} {
		mustCreate(t, ctx, s, &post{Title: title})
	}
	page, err := s.List(ctx)
	if err != nil || page.Page != 1 || page.Size != 3 || len(page.Items) != 3 || !page.HasMore || page.Total != 0 {
		t.Fatalf("default page: %+v %v", page, err)
	}
	if got := titles(page.Items); !slices.Equal(got, []string{"e", "b", "d"}) {
		t.Fatalf("no order is insertion order: %v", got)
	}
	page, err = s.List(ctx, where.Order("title"), where.Page(2, 2), where.Count())
	if err != nil || page.Page != 2 || page.Size != 2 || page.Total != 7 || !page.HasMore {
		t.Fatalf("page 2: %+v %v", page, err)
	}
	if got := titles(page.Items); !slices.Equal(got, []string{"c", "d"}) {
		t.Fatalf("page 2 = %v", got)
	}
	page, _ = s.List(ctx, where.Order("title", true), where.Page(4, 2))
	if got := titles(page.Items); !slices.Equal(got, []string{"a"}) || page.HasMore {
		t.Fatalf("last page = %v, has more %v", got, page.HasMore)
	}
	page, _ = s.List(ctx, where.Page(1, 50))
	if page.Size != 5 || len(page.Items) != 5 {
		t.Fatalf("a size over the max is clamped: %+v", page)
	}
	page, _ = s.List(ctx, where.Order("title"), where.Offset(5), where.Limit(10))
	if got := titles(page.Items); !slices.Equal(got, []string{"f", "g"}) || page.Page != 0 || page.Size != 5 {
		t.Fatalf("offset and a limit over the max: %v %+v", got, page)
	}
	page, err = s.List(ctx, where.Eq("title", "zzz"))
	if err != nil || page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("an empty page has a non-nil slice: %+v %v", page, err)
	}
	if _, err := s.List(ctx, where.Page(0, 10)); !errors.Is(err, chok.ErrBadRequest) {
		t.Fatalf("page 0: %v", err)
	}
}

// TestList_TiesNeverRepeatAcrossPages pins the ID tie-break: rows equal
// in the order field still page deterministically.
func TestList_TiesNeverRepeatAcrossPages(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	for i := range 9 {
		mustCreate(t, ctx, s, &post{Title: string(rune('a' + i)), Status: "same"})
	}
	seen := map[string]bool{}
	for pg := 1; pg <= 3; pg++ {
		page, err := s.List(ctx, where.Order("status"), where.Page(pg, 3))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page.Items {
			if seen[p.Title] {
				t.Fatalf("%s on two pages", p.Title)
			}
			seen[p.Title] = true
		}
	}
	if len(seen) != 9 {
		t.Fatalf("saw %d of 9", len(seen))
	}
}

func TestListFromQuery(t *testing.T) {
	s := posts(t, store.Policy(storePolicy(20, 2)))
	ctx := t.Context()
	mustCreate(t, ctx, s, &post{Title: "a", Status: "draft"}, &post{Title: "b", Status: "published"}, &post{Title: "c", Status: "archived"})
	page, err := s.ListFromQuery(ctx, map[string][]string{"status": {"draft", "published"}, "order": {"title:desc"}, "size": {"1"}})
	if err != nil || page.Total != 2 || page.Size != 1 || !page.HasMore || titles(page.Items)[0] != "b" {
		t.Fatalf("filters, order and size: %+v %v", page, err)
	}
	// An oversized request reports the size it got, and page 2 counts in
	// pages of that size.
	page, err = s.ListFromQuery(ctx, map[string][]string{"size": {"10"}, "order": {"title"}})
	if err != nil || page.Total != 3 || page.Size != 2 || page.Page != 1 || !page.HasMore || !slices.Equal(titles(page.Items), []string{"a", "b"}) {
		t.Fatalf("clamped page 1: %+v %v", page, err)
	}
	page, err = s.ListFromQuery(ctx, map[string][]string{"page": {"2"}, "size": {"10"}, "order": {"title"}})
	if err != nil || page.Page != 2 || page.Size != 2 || page.HasMore || !slices.Equal(titles(page.Items), []string{"c"}) {
		t.Fatalf("clamped page 2: %+v %v", page, err)
	}
	// An empty filter value is no filter.
	if page, err = s.ListFromQuery(ctx, map[string][]string{"status": {""}}); err != nil || page.Total != 3 {
		t.Fatalf("empty value: %+v %v", page, err)
	}
	for _, bad := range []map[string][]string{
		{"nope": {"1"}},
		{"order": {"body"}},
		{"order": {"title:sideways"}},
		{"page": {"x"}},
		{"page": {"1"}, "cursor": {""}},
	} {
		if _, err := s.ListFromQuery(ctx, bad); !errors.Is(err, chok.ErrBadRequest) {
			t.Errorf("%v: %v", bad, err)
		}
	}
}

func TestUpdate_LocksOnVersion(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	p := &post{Title: "v"}
	mustCreate(t, ctx, s, p)
	before := p.UpdatedAt
	time.Sleep(5 * time.Millisecond)

	got, _ := s.Get(ctx, store.RID(p.RID))
	title := "v2"
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&struct {
		Title *string `json:"title"`
	}{&title}).Onto(got)); err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.Title != "v2" {
		t.Fatalf("Onto's object advances: %+v", got)
	}
	again, _ := s.Get(ctx, store.RID(p.RID))
	if again.Version != 2 || again.Title != "v2" || !again.UpdatedAt.After(before) {
		t.Fatalf("stored %+v (updated_at %v → %v)", again, before, again.UpdatedAt)
	}
	// p still holds version 1: a stale lock conflicts and writes nothing.
	err := s.Update(ctx, store.RID(p.RID), store.Patch(&struct {
		Title *string `json:"title"`
	}{&title}).Onto(p))
	var vc *store.VersionConflictError
	if !errors.As(err, &vc) || vc.Version != 1 || !errors.Is(err, chok.ErrConflict) {
		t.Fatalf("stale: %v", err)
	}
	if e := vc.APIError(); e.Code != 409 || e.Metadata["version"] != 1 {
		t.Fatalf("envelope %+v", e)
	}
	if p.Version != 1 {
		t.Fatal("a failed update does not advance the object")
	}
	// Set has no lock unless where.Version adds one.
	if err := s.Update(ctx, store.RID(p.RID), store.Set(map[string]any{"status": "x"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Set(map[string]any{"status": "y"}), where.Version(2)); !errors.As(err, &vc) {
		t.Fatalf("where.Version stale: %v", err)
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Set(map[string]any{"status": "y"}), where.Version(3)); err != nil {
		t.Fatalf("where.Version current: %v", err)
	}
	// A missing row is not found, with or without a lock.
	for _, opts := range [][]where.Option{nil, {where.Version(1)}} {
		if err := s.Update(ctx, store.RID("pst_nope"), store.Set(map[string]any{"status": "z"}), opts...); !errors.Is(err, chok.ErrNotFound) {
			t.Fatalf("missing row %v: %v", opts, err)
		}
	}
}

func TestUpdate_RefusesEmptyAndUnknownChanges(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	p := &post{Title: "x"}
	mustCreate(t, ctx, s, p)
	for _, ch := range []*store.Changes{nil, store.Set(nil), store.Set(map[string]any{}), store.Patch(&struct {
		Title *string `json:"title"`
	}{}).Onto(p)} {
		err := s.Update(ctx, store.RID(p.RID), ch)
		if !errors.As(err, new(*store.EmptyChangesError)) || !errors.Is(err, chok.ErrBadRequest) {
			t.Fatalf("empty changes: %v", err)
		}
	}
	for _, field := range []string{"version", "nope", "id"} {
		err := s.Update(ctx, store.RID(p.RID), store.Set(map[string]any{field: 9}))
		var coder chok.Coder
		if err == nil || !strings.Contains(err.Error(), "not an update field") || errors.As(err, &coder) {
			t.Fatalf("Set %s is a programming error (500): %v", field, err)
		}
	}
	if got, _ := s.Get(ctx, store.RID(p.RID)); got.Version != 1 {
		t.Fatalf("refused changes write nothing: %+v", got)
	}
	if err := s.Update(ctx, store.Where(), store.Set(map[string]any{"status": "x"})); !errors.As(err, new(*store.MissingConditionsError)) {
		t.Fatalf("update everything: %v", err)
	}
}

func TestUpdate_ByEveryLocator(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	p := &post{Title: "p"}
	mustCreate(t, ctx, s, p)
	before := p.UpdatedAt
	time.Sleep(5 * time.Millisecond)
	for i, loc := range []store.Locator{store.ID(p.ID), store.RID(p.RID), store.Where(where.Eq("title", "p"))} {
		if err := s.Update(ctx, loc, store.Set(map[string]any{"views": i + 1})); err != nil {
			t.Fatalf("%s: %v", loc, err)
		}
	}
	got, _ := s.Get(ctx, store.ID(p.ID))
	if got.Views != 3 || got.Version != 4 || !got.UpdatedAt.After(before) {
		t.Fatalf("three unlocked writes: %+v (updated_at %v → %v)", got, before, got.UpdatedAt)
	}
	for _, loc := range []store.Locator{store.ID(99_999), store.RID("pst_nope"), store.Where(where.Eq("title", "nope"))} {
		if err := s.Update(ctx, loc, store.Set(map[string]any{"views": 9})); !errors.As(err, new(*store.NotFoundError)) {
			t.Fatalf("missing %s: %v", loc, err)
		}
	}
	if err := s.Update(ctx, nil, store.Set(map[string]any{"views": 9})); err == nil {
		t.Fatal("a nil locator is an error")
	}
}

func TestUpdate_WhereWritesEveryMatch(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	mustCreate(t, ctx, s, &post{Title: "a"}, &post{Title: "b"}, &post{Title: "c", Status: "published"})
	if err := s.Update(ctx, store.Where(where.Eq("status", "draft")), store.Set(map[string]any{"status": "archived", "views": 0})); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx, where.Eq("status", "archived")); n != 2 {
		t.Fatalf("archived %d", n)
	}
	for _, p := range must(s.List(ctx, where.Eq("status", "archived"))).Items {
		if p.Version != 2 {
			t.Fatalf("every written row advances: %+v", p)
		}
	}
}

func TestDelete_SoftAndHard(t *testing.T) {
	d := openDB(t)
	ctx := t.Context()
	s, notes := store.New[post](d), store.New[note](d)
	trashed := func(loc store.Locator) *post {
		t.Helper()
		if _, err := s.Get(ctx, loc); !errors.Is(err, chok.ErrNotFound) {
			t.Fatalf("deleted row %s: %v", loc, err)
		}
		gone, err := s.Get(ctx, loc, where.WithTrashed())
		if err != nil || !gone.DeletedAt.Valid || len(gone.DeleteToken) == 0 || gone.Version != 2 {
			t.Fatalf("trashed row %+v %v", gone, err)
		}
		return gone
	}
	p, q := &post{Title: "soft"}, &post{Title: "by id"}
	mustCreate(t, ctx, s, p, q, &post{Title: "w1", Status: "old"}, &post{Title: "w2", Status: "old"})
	if err := s.Delete(ctx, store.RID(p.RID)); err != nil {
		t.Fatal(err)
	}
	trashed(store.RID(p.RID))
	if err := s.Delete(ctx, store.ID(q.ID)); err != nil {
		t.Fatal(err)
	}
	trashed(store.ID(q.ID))
	if err := s.Delete(ctx, store.Where(where.Eq("status", "old"))); err != nil {
		t.Fatal(err)
	}
	// One statement, one token: rows live together never share their
	// SoftUnique values, so (values, token) stays unique.
	trashed(store.Where(where.Eq("title", "w1")))
	trashed(store.Where(where.Eq("title", "w2")))
	// Deleting what is not there is not an error.
	for _, loc := range []store.Locator{store.RID(p.RID), store.RID("pst_none"), store.ID(99_999), store.Where(where.Eq("title", "none"))} {
		if err := s.Delete(ctx, loc); err != nil {
			t.Fatalf("delete %s again: %v", loc, err)
		}
	}
	// The SoftUnique title is free again.
	mustCreate(t, ctx, s, &post{Title: "soft"})

	a, b := &note{Code: "hard"}, &note{Code: "by rid"}
	mustCreate(t, ctx, notes, a, b, &note{Code: "w", N: 7}, &note{Code: "v", N: 7}, &note{Code: "keep"})
	for _, loc := range []store.Locator{store.ID(a.ID), store.RID(b.RID), store.Where(where.Eq("n", 7))} {
		if err := notes.Delete(ctx, loc); err != nil {
			t.Fatal(err)
		}
	}
	if left, _ := notes.Pluck[string](ctx, "code", where.WithTrashed()); !slices.Equal(left, []string{"keep"}) {
		t.Fatalf("a hard delete removes the rows: %v left", left)
	}
	for _, loc := range []store.Locator{store.Where(), store.Where(where.Order("code")), store.Where(where.Page(1, 1))} {
		if err := notes.Delete(ctx, loc); !errors.As(err, new(*store.MissingConditionsError)) {
			t.Fatalf("delete everything with %s: %v", loc, err)
		}
	}
}

func TestDelete_WithVersion(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	p := &post{Title: "v"}
	mustCreate(t, ctx, s, p)
	if err := s.Delete(ctx, store.RID(p.RID), where.Version(5)); !errors.As(err, new(*store.VersionConflictError)) {
		t.Fatalf("stale: %v", err)
	}
	if err := s.Delete(ctx, store.RID("pst_none"), where.Version(1)); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.Delete(ctx, store.RID(p.RID), where.Version(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, store.RID(p.RID), where.Version(1)); !errors.As(err, new(*store.VersionConflictError)) {
		t.Fatalf("a deleted row moved on: %v", err)
	}
}

func TestRestore(t *testing.T) {
	d := openDB(t)
	ctx := t.Context()
	s := store.New[post](d)
	p := &post{Title: "back"}
	mustCreate(t, ctx, s, p)
	if err := s.Restore(ctx, store.RID(p.RID)); err != nil {
		t.Fatalf("restoring a live row does nothing: %v", err)
	}
	_ = s.Delete(ctx, store.RID(p.RID))
	if err := s.Restore(ctx, store.RID(p.RID)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, store.RID(p.RID))
	if err != nil || got.DeleteToken != "" || got.DeletedAt.Valid || got.Version != 3 {
		t.Fatalf("restored %+v %v", got, err)
	}
	if err := s.Restore(ctx, store.RID("pst_none")); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// A live row took the title: the restore conflicts and changes nothing.
	_ = s.Delete(ctx, store.RID(p.RID))
	mustCreate(t, ctx, s, &post{Title: "back"})
	var dup *store.DuplicateError
	if err := s.Restore(ctx, store.RID(p.RID)); !errors.As(err, &dup) || dup.Field != "title" {
		t.Fatalf("slot taken: %v", err)
	}
	if err := store.New[note](d).Restore(ctx, store.ID(1)); err == nil || !strings.Contains(err.Error(), "no soft delete") {
		t.Fatalf("hard-delete model: %v", err)
	}
}

func TestUpsert(t *testing.T) {
	d := openDB(t)
	ctx := t.Context()
	notes := store.New[note](d)
	if err := notes.Upsert(ctx, &note{Code: "a", N: 1}, "code"); err != nil {
		t.Fatal(err)
	}
	if err := notes.Upsert(ctx, &note{Code: "a", N: 2}, "code"); err != nil {
		t.Fatal(err)
	}
	got, err := notes.Get(ctx, store.Where(where.Eq("code", "a")))
	if err != nil || got.N != 2 || got.Version != 2 {
		t.Fatalf("the conflict updates and advances the version: %+v %v", got, err)
	}
	// By default the conflict is the RID.
	if err := notes.Upsert(ctx, &note{Code: "b", N: 1}); err != nil {
		t.Fatal(err)
	}
	if err := notes.Upsert(ctx, &note{Model: got.Model, Code: "a", N: 3}); err != nil {
		t.Fatal(err)
	}
	if got, _ = notes.Get(ctx, store.RID(got.RID)); got.N != 3 {
		t.Fatalf("upsert by RID: %+v", got)
	}
	if err := notes.Upsert(ctx, &note{Code: "c"}, "body"); err == nil {
		t.Fatal("a conflict field must be a query field")
	}
	if err := notes.Upsert(ctx, nil); err == nil {
		t.Fatal("nil object")
	}
	// A SoftUnique conflict counts live rows: a deleted "t" does not stop
	// a new one, a live one is updated.
	s := store.New[post](d)
	mustCreate(t, ctx, s, &post{Title: "t", Views: 1})
	_ = s.Delete(ctx, store.Where(where.Eq("title", "t")))
	if err := s.Upsert(ctx, &post{Title: "t", Views: 2}, "title"); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(ctx, &post{Title: "t", Views: 3}, "title"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx, where.WithTrashed()); n != 2 {
		t.Fatalf("rows %d", n)
	}
	if live, _ := s.Get(ctx, store.Where(where.Eq("title", "t"))); live.Views != 3 {
		t.Fatalf("live row %+v", live)
	}
	// Owned stores refuse: the update path cannot be fenced.
	if err := store.New[doc](d).Upsert(as(ctx, "u"), &doc{Title: "x"}); err == nil || !strings.Contains(err.Error(), "cannot be fenced") {
		t.Fatalf("owned upsert: %v", err)
	}
}

func TestBatchUpsert(t *testing.T) {
	notes := store.New[note](openDB(t))
	ctx := t.Context()
	mustCreate(t, ctx, notes, &note{Code: "a", N: 1})
	if err := notes.BatchUpsert(ctx, []*note{{Code: "a", N: 10}, {Code: "b", N: 20}}, "code"); err != nil {
		t.Fatal(err)
	}
	a, _ := notes.Get(ctx, store.Where(where.Eq("code", "a")))
	b, _ := notes.Get(ctx, store.Where(where.Eq("code", "b")))
	if a.N != 10 || b.N != 20 {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	err := notes.BatchUpsert(ctx, []*note{{Code: "c"}, {Code: "c"}}, "code")
	if err == nil || !strings.Contains(err.Error(), "same conflict values") {
		t.Fatalf("duplicate tuple: %v", err)
	}
	if err := notes.BatchUpsert(ctx, []*note{{Code: "d"}, nil}, "code"); err == nil {
		t.Fatal("nil item")
	}
	if n, _ := notes.Count(ctx); n != 2 {
		t.Fatalf("refused batches write nothing: %d", n)
	}
}

func must[V any](v V, err error) V {
	if err != nil {
		panic(err)
	}
	return v
}
