package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

// batchItem has two unique keys and a CHECK constraint every dialect
// enforces, so a statement can fail in a later INSERT chunk.
type batchItem struct {
	db.Model
	Code string `json:"code" store:"query,update" gorm:"size:20;not null;uniqueIndex:uk_batch_code"`
	Alt  string `json:"alt" store:"query,update" gorm:"size:20;not null;uniqueIndex:uk_batch_alt"`
	N    int    `json:"n" store:"query,update" gorm:"not null;default:0;check:chk_batch_n,n >= 0"`
}

func batchItems(t *testing.T, opts ...store.Option) *store.Store[batchItem] {
	t.Helper()
	return store.New[batchItem](dbtest.Open(t, db.Tables(db.Table(&batchItem{}))), opts...)
}

func manyItems(n int) []*batchItem {
	items := make([]*batchItem, n)
	for i := range items {
		items[i] = &batchItem{Code: fmt.Sprintf("c%03d", i), Alt: fmt.Sprintf("a%03d", i), N: i}
	}
	return items
}

// TestUpsert_ValidationPrecedesHooks: a conflict target that is not a
// distinct column fails before any hook runs or any row is written; an
// unknown field is the code's where.ErrUnknownField.
func TestUpsert_ValidationPrecedesHooks(t *testing.T) {
	hooks := 0
	s := batchItems(t, store.Hooks(store.HookSet[batchItem]{
		BeforeCreate: func(context.Context, *batchItem) error { hooks++; return nil },
	}))
	ctx := t.Context()
	for name, call := range map[string]func() error{
		"unknown field":     func() error { return s.Upsert(ctx, &batchItem{Code: "x", Alt: "x"}, "nope") },
		"repeated field":    func() error { return s.Upsert(ctx, &batchItem{Code: "x", Alt: "x"}, "code", "code") },
		"batch, bad target": func() error { return s.BatchUpsert(ctx, manyItems(3), "nope") },
		"batch, repeated":   func() error { return s.BatchUpsert(ctx, manyItems(3), "alt", "alt") },
	} {
		err := call()
		unknown := strings.Contains(name, "unknown") || strings.Contains(name, "bad target")
		if err == nil || unknown != errors.Is(err, where.ErrUnknownField) || (!unknown && !strings.Contains(err.Error(), "repeats column")) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if hooks != 0 {
		t.Fatalf("hooks ran %d times for refused upserts", hooks)
	}
	if n, _ := s.Count(ctx); n != 0 {
		t.Fatalf("refused upserts wrote %d rows", n)
	}
}

// TestBatchUpsert_DuplicatesAcrossChunks: two objects with one conflict
// value are refused before any SQL, also when they would land in
// different INSERT chunks, and also when a hook made them equal.
func TestBatchUpsert_DuplicatesAcrossChunks(t *testing.T) {
	s := batchItems(t)
	ctx := t.Context()
	items := manyItems(150)
	items[149].Code = items[0].Code
	if err := s.BatchUpsert(ctx, items, "code"); err == nil || !strings.Contains(err.Error(), "items 0 and 149") {
		t.Fatalf("a duplicate across the chunk boundary: %v", err)
	}
	normalized := batchItems(t, store.Hooks(store.HookSet[batchItem]{
		BeforeCreate: func(_ context.Context, it *batchItem) error { it.Code = strings.ToLower(it.Code); return nil },
	}))
	err := normalized.BatchUpsert(ctx, []*batchItem{{Code: "ABC", Alt: "1"}, {Code: "abc", Alt: "2"}}, "code")
	if err == nil || !strings.Contains(err.Error(), "same conflict values") {
		t.Fatalf("a hook that makes two keys equal: %v", err)
	}
	for _, st := range []*store.Store[batchItem]{s, normalized} {
		if n, _ := st.Count(ctx); n != 0 {
			t.Fatalf("refused batches wrote %d rows", n)
		}
	}
}

// TestBatchUpsert_IsAllOrNothing: a statement failing in the second
// INSERT chunk rolls back the first one.
func TestBatchUpsert_IsAllOrNothing(t *testing.T) {
	s := batchItems(t)
	ctx := t.Context()
	mustCreate(t, ctx, s, &batchItem{Code: "c000", Alt: "seed", N: 1})
	items := manyItems(150)
	items[120].N = -1 // violates chk_batch_n
	if err := s.BatchUpsert(ctx, items, "code"); err == nil {
		t.Fatal("the CHECK violation is an error")
	}
	if n, _ := s.Count(ctx); n != 1 {
		t.Fatalf("the failed batch left %d rows, want the seed only", n)
	}
	if seed, _ := s.Get(ctx, store.Where(where.Eq("code", "c000"))); seed.Alt != "seed" || seed.Version != 1 {
		t.Fatalf("the seed was updated by a rolled-back chunk: %+v", seed)
	}
	items[120].N = 120
	if err := s.BatchUpsert(ctx, items, "code"); err != nil {
		t.Fatal(err)
	}
	seed, _ := s.Get(ctx, store.Where(where.Eq("code", "c000")))
	if n, _ := s.Count(ctx); n != 150 || seed.Alt != "a000" || seed.Version != 2 {
		t.Fatalf("rows %d, seed %+v", n, seed)
	}
}

// TestBatchUpsert_ConflictTargetIsStrict: on SQLite and PostgreSQL the
// conflict target is the only key that updates; a collision on another
// unique key is a DuplicateError naming its field. MySQL is the other
// way round (TestBatchUpsert_MySQLIgnoresTheConflictTarget).
func TestBatchUpsert_ConflictTargetIsStrict(t *testing.T) {
	if dbtest.Driver() == "mysql" {
		t.Skip("MySQL updates on any unique key")
	}
	s := batchItems(t)
	ctx := t.Context()
	mustCreate(t, ctx, s, &batchItem{Code: "old", Alt: "shared"})
	err := s.Upsert(ctx, &batchItem{Code: "new", Alt: "shared", N: 5}, "code")
	var dup *store.DuplicateError
	if !errors.As(err, &dup) || dup.Field != "alt" {
		t.Fatalf("collision outside the target: %v", err)
	}
	if got, _ := s.Get(ctx, store.Where(where.Eq("alt", "shared"))); got.Code != "old" || got.N != 0 {
		t.Fatalf("the row changed: %+v", got)
	}
}

// TestUpsert_UniqueOnSoftDeleteModel: conflict fields that are a Unique
// index's are the conflict target as they are, so a live row holding the
// values is updated. A deleted one would be updated too and stay
// deleted: Upsert refuses that with the DuplicateError Create gets from
// the same index, and writes nothing. A SoftUnique on the same model
// still counts live rows only.
func TestUpsert_UniqueOnSoftDeleteModel(t *testing.T) {
	s := skus(t)
	ctx := t.Context()
	for i, title := range []string{"t1", "t2"} {
		if err := s.Upsert(ctx, &sku{Code: "c1", Title: title, Stock: i + 1}, "code"); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	got, err := s.Get(ctx, store.Where(where.Eq("code", "c1")))
	if err != nil || got.Title != "t2" || got.Stock != 2 || got.Version != 2 {
		t.Fatalf("the second upsert updates the row: %+v %v", got, err)
	}
	if err := s.Delete(ctx, store.RID(got.RID)); err != nil {
		t.Fatal(err)
	}
	err = s.Upsert(ctx, &sku{Code: "c1", Title: "t3", Stock: 3}, "code")
	var dup *store.DuplicateError
	if !errors.As(err, &dup) || dup.Field != "code" || dup.Constraint != "uk_sku_code" || !errors.Is(err, chok.ErrConflict) {
		t.Fatalf("an upsert onto a deleted row's code: %#v", err)
	}
	if e := dup.APIError(); e.Code != 409 || e.Metadata["field"] != "code" {
		t.Fatalf("envelope %+v", e)
	}
	var created *store.DuplicateError
	if err := s.Create(ctx, &sku{Code: "c1", Title: "t3"}); !errors.As(err, &created) || created.Field != dup.Field ||
		(dbtest.Driver() != "sqlite" && created.Constraint != dup.Constraint) {
		t.Fatalf("Create on the same code: %#v, the upsert's %#v", err, dup)
	}
	gone, err := s.Get(ctx, store.RID(got.RID), where.WithTrashed())
	if err != nil || !gone.DeletedAt.Valid || gone.Title != "t2" || gone.Stock != 2 || gone.Version != 3 {
		t.Fatalf("the deleted row holding c1 is untouched: %+v %v", gone, err)
	}
	if n, _ := s.Count(ctx, where.WithTrashed()); n != 1 {
		t.Fatalf("%d rows, want the deleted one", n)
	}
	// The deleted row's title t2 does not stop a live row.
	if err := s.Upsert(ctx, &sku{Code: "c2", Title: "t2", Stock: 4}, "title"); err != nil {
		t.Fatal(err)
	}
	if live, err := s.Get(ctx, store.Where(where.Eq("title", "t2"))); err != nil || live.Code != "c2" || live.Stock != 4 {
		t.Fatalf("a new live row: %+v %v", live, err)
	}
	// Beside the deleted c1, a live row's code still updates it.
	if err := s.Upsert(ctx, &sku{Code: "c2", Title: "t4", Stock: 5}, "code"); err != nil {
		t.Fatal(err)
	}
	if live, err := s.Get(ctx, store.Where(where.Eq("code", "c2"))); err != nil || live.Title != "t4" || live.Stock != 5 || live.Version != 2 {
		t.Fatalf("the live c2 is updated: %+v %v", live, err)
	}
	if n, _ := s.Count(ctx, where.WithTrashed()); n != 2 {
		t.Fatalf("%d rows, want the deleted c1 and the live c2", n)
	}
}

// TestUpsert_DeletedRIDIsDuplicate: the default conflict target, the
// RID, counts deleted rows on a soft-delete model. A live row's RID
// updates that row; a deleted row's is the DuplicateError Create gets,
// whose field is id, and nothing is written.
func TestUpsert_DeletedRIDIsDuplicate(t *testing.T) {
	s := skus(t)
	ctx := t.Context()
	live := &sku{Code: "c1", Title: "t1", Stock: 1}
	gone := &sku{Code: "c2", Title: "t2", Stock: 1}
	mustCreate(t, ctx, s, live, gone)
	if err := s.Delete(ctx, store.RID(gone.RID)); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(ctx, &sku{RID: live.RID, Code: "c1", Title: "t1b", Stock: 5}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(ctx, store.RID(live.RID)); err != nil || got.Title != "t1b" || got.Stock != 5 || got.Version != 2 {
		t.Fatalf("a live row's RID updates it: %+v %v", got, err)
	}
	err := s.Upsert(ctx, &sku{RID: gone.RID, Code: "c3", Title: "t3", Stock: 7})
	var dup *store.DuplicateError
	if !errors.As(err, &dup) || dup.Field != "id" || !errors.Is(err, chok.ErrConflict) {
		t.Fatalf("an upsert by a deleted row's RID: %#v", err)
	}
	var created *store.DuplicateError
	if err := s.Create(ctx, &sku{RID: gone.RID, Code: "c3", Title: "t3", Stock: 7}); !errors.As(err, &created) || created.Field != dup.Field ||
		(dbtest.Driver() != "sqlite" && created.Constraint != dup.Constraint) {
		t.Fatalf("Create with the same RID: %#v, the upsert's %#v", err, dup)
	}
	if got, err := s.Get(ctx, store.RID(gone.RID), where.WithTrashed()); err != nil || !got.DeletedAt.Valid ||
		got.Code != "c2" || got.Title != "t2" || got.Stock != 1 || got.Version != 2 {
		t.Fatalf("the deleted row is untouched: %+v %v", got, err)
	}
	if n, _ := s.Count(ctx, where.WithTrashed()); n != 2 {
		t.Fatalf("%d rows, want the two created", n)
	}
}

// TestBatchUpsert_DeletedHolderFailsTheBatch: one object whose Unique
// value a deleted row holds, here in the second INSERT chunk, fails the
// whole batch: no row is inserted and the live row the batch would
// update keeps its values. Without it the same batch goes through.
func TestBatchUpsert_DeletedHolderFailsTheBatch(t *testing.T) {
	s := skus(t)
	ctx := t.Context()
	seed := &sku{Code: "c000", Title: "seed", Stock: 1}
	gone := &sku{Code: "gone", Title: "gone", Stock: 1}
	mustCreate(t, ctx, s, seed, gone)
	if err := s.Delete(ctx, store.RID(gone.RID)); err != nil {
		t.Fatal(err)
	}
	items := make([]*sku, 150)
	for i := range items {
		items[i] = &sku{Code: fmt.Sprintf("c%03d", i), Title: fmt.Sprintf("t%03d", i), Stock: i}
	}
	items[120].Code = "gone"
	err := s.BatchUpsert(ctx, items, "code")
	var dup *store.DuplicateError
	if !errors.As(err, &dup) || dup.Field != "code" || !errors.Is(err, chok.ErrConflict) {
		t.Fatalf("item 120 has a deleted row's code: %#v", err)
	}
	if n, _ := s.Count(ctx, where.WithTrashed()); n != 2 {
		t.Fatalf("the refused batch left %d rows, want the two seeds", n)
	}
	if got, err := s.Get(ctx, store.RID(seed.RID)); err != nil || got.Title != "seed" || got.Version != 1 {
		t.Fatalf("the refused batch updated the live row: %+v %v", got, err)
	}
	if got, err := s.Get(ctx, store.RID(gone.RID), where.WithTrashed()); err != nil || !got.DeletedAt.Valid || got.Title != "gone" || got.Version != 2 {
		t.Fatalf("the refused batch changed the deleted row: %+v %v", got, err)
	}
	items[120].Code = "c120"
	if err := s.BatchUpsert(ctx, items, "code"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx); n != 150 {
		t.Fatalf("%d live rows, want the 150 items", n)
	}
	if got, err := s.Get(ctx, store.RID(seed.RID)); err != nil || got.Title != "t000" || got.Version != 2 {
		t.Fatalf("the live c000 is updated: %+v %v", got, err)
	}
}
