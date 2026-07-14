package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

type batchItem struct {
	db.Model
	Code  string `json:"code"  gorm:"uniqueIndex;size:50;not null"`
	Alt   string `json:"alt"   gorm:"uniqueIndex;size:50;not null"`
	Value string `json:"value" gorm:"size:100"`
}

func (batchItem) RIDPrefix() string { return "bti" }

type keyOnlyBatchModel struct {
	db.Model
}

func (keyOnlyBatchModel) RIDPrefix() string { return "kob" }

type recursiveConflictValuer string

func (v recursiveConflictValuer) Value() (driver.Value, error) { return v, nil }

func setupBatchItemStore(t *testing.T, opts ...StoreOption) (*Store[batchItem], *db.DB) {
	t.Helper()
	h := dbtest.Open(t)
	if err := h.Migrate(context.Background(), db.Table(&batchItem{})); err != nil {
		t.Fatal(err)
	}
	base := []StoreOption{
		WithQueryFields("id", "code", "alt", "value"),
		WithUpdateFields("code", "alt", "value"),
	}
	return New[batchItem](h, log.Empty(), append(base, opts...)...), h
}

func TestBatchUpdate_HappyPathFieldsAndVersions(t *testing.T) {
	s, _ := setupUserStore(t)
	a := createUser(t, s, "alice", "alice@example.com")
	b := createUser(t, s, "bob", "bob@example.com")
	a.Name = "Alice"
	b.Name = "Bob"
	b.Email = "unchanged@example.com"

	if err := s.BatchUpdate(context.Background(), []*User{a, b}, "name"); err != nil {
		t.Fatal(err)
	}
	if a.Version != 2 || b.Version != 2 {
		t.Fatalf("versions = (%d, %d), want (2, 2)", a.Version, b.Version)
	}
	gotA, err := s.Get(context.Background(), RID(a.RID))
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := s.Get(context.Background(), RID(b.RID))
	if err != nil {
		t.Fatal(err)
	}
	if gotA.Name != "Alice" || gotB.Name != "Bob" {
		t.Fatalf("names not persisted: a=%q b=%q", gotA.Name, gotB.Name)
	}
	if gotB.Email != "bob@example.com" {
		t.Fatalf("field subset changed email: %q", gotB.Email)
	}
}

func TestBatchUpdate_StaleVersionRollsBackAndRestoresVersions(t *testing.T) {
	s, _ := setupUserStore(t)
	a := createUser(t, s, "alice", "alice@example.com")
	b := createUser(t, s, "bob", "bob@example.com")

	freshB, err := s.Get(context.Background(), RID(b.RID))
	if err != nil {
		t.Fatal(err)
	}
	freshB.Name = "bob-concurrent"
	if err := s.Update(context.Background(), RID(freshB.RID), Fields(freshB, "name")); err != nil {
		t.Fatal(err)
	}

	a.Name = "alice-batch"
	b.Name = "bob-stale"
	err = s.BatchUpdate(context.Background(), []*User{a, b}, "name")
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("want ErrStaleVersion, got %v", err)
	}
	if a.Version != 1 || b.Version != 1 {
		t.Fatalf("rollback must restore input versions, got (%d, %d)", a.Version, b.Version)
	}
	gotA, err := s.Get(context.Background(), RID(a.RID))
	if err != nil {
		t.Fatal(err)
	}
	if gotA.Name != "alice" || gotA.Version != 1 {
		t.Fatalf("first update escaped rollback: %+v", gotA)
	}
}

func TestBatchUpdate_HookFailureRunsBeforeAnySQL(t *testing.T) {
	var hooks int
	h := dbtest.Open(t)
	if err := h.Migrate(context.Background(), db.Table(&batchItem{})); err != nil {
		t.Fatal(err)
	}
	hookErr := errors.New("hook rejected")
	s := New[batchItem](h, log.Empty(),
		WithQueryFields("id", "code", "alt", "value"),
		WithUpdateFields("code", "alt", "value"),
		WithBeforeUpdate(func(context.Context, Locator, Changes) error {
			hooks++
			if hooks == 2 {
				return hookErr
			}
			return nil
		}),
	)
	a := &batchItem{Code: "A", Alt: "A", Value: "old-a"}
	b := &batchItem{Code: "B", Alt: "B", Value: "old-b"}
	if err := s.BatchCreate(context.Background(), []*batchItem{a, b}); err != nil {
		t.Fatal(err)
	}
	a.Value = "new-a"
	b.Value = "new-b"
	err := s.BatchUpdate(context.Background(), []*batchItem{a, b}, "value")
	if !errors.Is(err, hookErr) {
		t.Fatalf("want hook error, got %v", err)
	}
	got, err := s.Get(context.Background(), RID(a.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "old-a" {
		t.Fatalf("first row was written before all hooks passed: %+v", got)
	}
}

func TestBatchUpdate_StaticValidationPrecedesHooks(t *testing.T) {
	var hooks int
	s, _ := setupBatchItemStore(t, WithBeforeUpdate(func(context.Context, Locator, Changes) error {
		hooks++
		return nil
	}))
	obj := &batchItem{Code: "A", Alt: "A"}

	err := s.BatchUpdate(context.Background(), []*batchItem{obj}, "missing")
	if !errors.Is(err, ErrUnknownUpdateField) {
		t.Fatalf("want ErrUnknownUpdateField, got %v", err)
	}
	if hooks != 0 {
		t.Fatalf("invalid static input ran %d hooks", hooks)
	}

	err = s.BatchUpdate(context.Background(), []*batchItem{nil}, "value")
	if err == nil || hooks != 0 {
		t.Fatalf("nil item: err=%v hooks=%d", err, hooks)
	}
}

func TestBatchUpdate_EmptyAndMissingLocator(t *testing.T) {
	s, _ := setupBatchItemStore(t)
	if err := s.BatchUpdate(context.Background(), nil, "value"); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	err := s.BatchUpdate(context.Background(), []*batchItem{{Code: "A", Alt: "A"}}, "value")
	if err == nil {
		t.Fatal("missing locator should fail")
	}
}

func TestBatchUpdate_OwnerScopeFailureRollsBack(t *testing.T) {
	s := setupProductStore(t)
	aliceCtx := userCtx("alice")
	bobCtx := userCtx("bob")
	a := &Product{Name: "alice-old"}
	b := &Product{Name: "bob-old"}
	if err := s.Create(aliceCtx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(bobCtx, b); err != nil {
		t.Fatal(err)
	}
	a.Name = "alice-new"
	b.Name = "bob-attacked"

	err := s.BatchUpdate(aliceCtx, []*Product{a, b}, "name")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want scoped ErrNotFound, got %v", err)
	}
	got, err := s.Get(aliceCtx, RID(a.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "alice-old" || a.Version != 1 {
		t.Fatalf("scope failure did not roll back first row: db=%+v inputVersion=%d", got, a.Version)
	}
}

func TestBatchUpsert_InsertAndConflictUpdate(t *testing.T) {
	s, _ := setupBatchItemStore(t)
	ctx := context.Background()
	seed := &batchItem{Code: "A", Alt: "seed", Value: "old"}
	if err := s.Create(ctx, seed); err != nil {
		t.Fatal(err)
	}

	objs := []*batchItem{
		{Code: "A", Alt: "incoming", Value: "updated"},
		{Code: "B", Alt: "new", Value: "inserted"},
	}
	if err := s.BatchUpsert(ctx, objs, []string{"code"}, "value"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Count(ctx); err != nil || n != 2 {
		t.Fatalf("count=%d err=%v, want 2", n, err)
	}
	got, err := s.Get(ctx, Where(where.WithFilter("code", "A")))
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "updated" || got.Alt != "seed" || got.RID != seed.RID {
		t.Fatalf("conflict update touched wrong state: %+v", got)
	}
}

func TestBatchUpsert_DuplicateConflictRejectedAcrossChunkBoundary(t *testing.T) {
	var hooks int
	s, _ := setupBatchItemStore(t, WithBeforeCreate(func(context.Context, *batchItem) error {
		hooks++
		return nil
	}))
	objs := make([]*batchItem, createBatchSize+1)
	for i := range objs {
		objs[i] = &batchItem{Code: fmt.Sprintf("C%03d", i), Alt: fmt.Sprintf("A%03d", i)}
	}
	objs[createBatchSize].Code = objs[createBatchSize-1].Code

	err := s.BatchUpsert(context.Background(), objs, []string{"code"}, "value")
	if !errors.Is(err, ErrDuplicateBatchConflict) {
		t.Fatalf("want ErrDuplicateBatchConflict, got %v", err)
	}
	if hooks != 0 {
		t.Fatalf("duplicate preflight ran %d hooks", hooks)
	}
	if n, countErr := s.Count(context.Background()); countErr != nil || n != 0 {
		t.Fatalf("duplicate preflight wrote rows: count=%d err=%v", n, countErr)
	}
}

func TestBatchUpsert_LaterChunkFailureRollsBackEarlierChunks(t *testing.T) {
	s, _ := setupBatchItemStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, &batchItem{Code: "seed", Alt: "taken"}); err != nil {
		t.Fatal(err)
	}
	objs := make([]*batchItem, createBatchSize+1)
	for i := range objs {
		objs[i] = &batchItem{
			Code: fmt.Sprintf("C%03d", i),
			Alt:  fmt.Sprintf("A%03d", i),
		}
	}
	// The second statement violates a different unique key, which is not
	// handled by the declared code conflict target on SQLite/PostgreSQL.
	objs[createBatchSize].Alt = "taken"
	err := s.BatchUpsert(ctx, objs, []string{"code"}, "value")
	if err == nil {
		t.Fatal("expected later-chunk unique violation")
	}
	if n, countErr := s.Count(ctx); countErr != nil || n != 1 {
		t.Fatalf("earlier chunk escaped rollback: count=%d err=%v", n, countErr)
	}
}

func TestBatchUpsert_StaticValidationPrecedesHooks(t *testing.T) {
	var hooks int
	s, _ := setupBatchItemStore(t, WithBeforeCreate(func(context.Context, *batchItem) error {
		hooks++
		return nil
	}))
	obj := &batchItem{Code: "A", Alt: "A"}
	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"missing conflict columns", func() error { return s.BatchUpsert(context.Background(), []*batchItem{obj}, nil, "value") }, ErrMissingConflictColumns},
		{"unknown conflict column", func() error {
			return s.BatchUpsert(context.Background(), []*batchItem{obj}, []string{"missing"}, "value")
		}, nil},
		{"unknown update column", func() error {
			return s.BatchUpsert(context.Background(), []*batchItem{obj}, []string{"code"}, "missing")
		}, ErrUnknownUpdateField},
		{"nil item", func() error { return s.BatchUpsert(context.Background(), []*batchItem{nil}, []string{"code"}, "value") }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hooks = 0
			err := tt.call()
			if err == nil {
				t.Fatal("expected error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
			if hooks != 0 {
				t.Fatalf("invalid static input ran %d hooks", hooks)
			}
		})
	}
}

func TestUpsert_StaticValidationPrecedesHooks(t *testing.T) {
	var hooks int
	s, h := setupBatchItemStore(t, WithBeforeCreate(func(context.Context, *batchItem) error {
		hooks++
		return nil
	}))
	obj := &batchItem{Code: "A", Alt: "A"}
	if err := s.Upsert(context.Background(), obj, []string{"missing"}, "value"); err == nil {
		t.Fatal("unknown conflict column should fail")
	}
	if hooks != 0 {
		t.Fatalf("invalid Upsert ran %d hooks", hooks)
	}

	keyOnly := New[keyOnlyBatchModel](h, log.Empty(), WithQueryFields("id"))
	err := keyOnly.Upsert(context.Background(), &keyOnlyBatchModel{}, []string{"id"})
	if !errors.Is(err, ErrMissingColumns) {
		t.Fatalf("empty update whitelist: want ErrMissingColumns, got %v", err)
	}
}

func TestResolveUpsertColumns_DuplicateColumnPolicy(t *testing.T) {
	s, _ := setupBatchItemStore(t)
	if _, _, err := s.resolveUpsertColumns([]string{"code", "code"}, []string{"value"}); err == nil {
		t.Fatal("duplicate conflict columns should be rejected")
	}
	conflict, updates, err := s.resolveUpsertColumns([]string{"code"}, []string{"value", "value"})
	if err != nil {
		t.Fatal(err)
	}
	if len(conflict) != 1 || len(updates) != 1 || updates[0] != "value" {
		t.Fatalf("duplicate update columns were not collapsed: conflict=%+v updates=%+v", conflict, updates)
	}
}

func TestNormalizeConflictValue_RecursiveValuerIsBounded(t *testing.T) {
	_, err := normalizeConflictValue(recursiveConflictValuer("loop"))
	if err == nil || !strings.Contains(err.Error(), "maximum normalization depth") {
		t.Fatalf("recursive driver.Valuer should fail with bounded-depth error, got %v", err)
	}
}

func TestBatchUpsert_HookNormalizationCannotCreateDuplicateKey(t *testing.T) {
	s, _ := setupBatchItemStore(t, WithBeforeCreate(func(_ context.Context, obj *batchItem) error {
		obj.Code = "normalized"
		return nil
	}))
	err := s.BatchUpsert(context.Background(), []*batchItem{
		{Code: "A", Alt: "A"},
		{Code: "B", Alt: "B"},
	}, []string{"code"}, "value")
	if !errors.Is(err, ErrDuplicateBatchConflict) {
		t.Fatalf("want post-hook ErrDuplicateBatchConflict, got %v", err)
	}
	if n, countErr := s.Count(context.Background()); countErr != nil || n != 0 {
		t.Fatalf("post-hook duplicate wrote rows: count=%d err=%v", n, countErr)
	}
}

func TestBatchUpsert_MySQLIgnoresDeclaredConflictTarget(t *testing.T) {
	h := dbtest.OpenMySQL(t)
	ctx := context.Background()
	if err := h.Migrate(ctx, db.Table(&batchItem{})); err != nil {
		t.Fatal(err)
	}
	s := New[batchItem](h, log.Empty(),
		WithQueryFields("id", "code", "alt", "value"),
		WithUpdateFields("code", "alt", "value"),
	)
	seed := &batchItem{Code: "old-code", Alt: "shared-alt", Value: "old"}
	if err := s.Create(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if err := s.BatchUpsert(ctx, []*batchItem{{
		Code: "new-code", Alt: "shared-alt", Value: "updated",
	}}, []string{"code"}, "value"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, Where(where.WithFilter("alt", "shared-alt")))
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != "old-code" || got.Value != "updated" {
		t.Fatalf("mysql conflict semantics changed: %+v", got)
	}
}
