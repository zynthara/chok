package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/store/where"
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
