package store_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/store/where"
)

// seedStats writes four posts: views 10, 20, 30, 40; scores 0.5 to 2;
// statuses a, b, a, b; the last one deleted.
func seedStats(t *testing.T) *store.Store[post] {
	t.Helper()
	s := posts(t)
	seed(t, s)
	return s
}

func seed(t *testing.T, s *store.Store[post]) {
	t.Helper()
	ctx := t.Context()
	for i := 1; i <= 4; i++ {
		mustCreate(t, ctx, s, &post{Title: fmt.Sprintf("p%d", i), Views: int64(10 * i), Score: float64(i) / 2, Status: []string{"a", "b"}[(i+1)%2], Hidden: i == 2})
	}
	if err := s.Delete(ctx, store.Where(where.Eq("title", "p4"))); err != nil {
		t.Fatal(err)
	}
}

func TestAggregate_SumAvgMinMax(t *testing.T) {
	s := seedStats(t)
	ctx := t.Context()
	if v, ok, err := s.Sum[int64](ctx, "views"); err != nil || !ok || v != 60 {
		t.Fatalf("Sum[int64] = %d %v %v (the deleted row is out)", v, ok, err)
	}
	if v, ok, err := s.Sum[float64](ctx, "score"); err != nil || !ok || v != 3 {
		t.Fatalf("Sum[float64] = %v %v %v", v, ok, err)
	}
	if v, ok, err := s.Sum[float64](ctx, "views", where.Eq("status", "a")); err != nil || !ok || v != 40 {
		t.Fatalf("filtered Sum = %v %v %v", v, ok, err)
	}
	if v, ok, err := s.Avg(ctx, "views"); err != nil || !ok || v != 20 {
		t.Fatalf("Avg = %v %v %v", v, ok, err)
	}
	if v, ok, err := s.Min[int64](ctx, "views"); err != nil || !ok || v != 10 {
		t.Fatalf("Min = %v %v %v", v, ok, err)
	}
	if v, ok, err := s.Max[float64](ctx, "score", where.WithTrashed()); err != nil || !ok || v != 2 {
		t.Fatalf("Max with trashed = %v %v %v", v, ok, err)
	}
	if v, ok, err := s.Sum[int64](ctx, "views", where.Eq("status", "none")); err != nil || ok || v != 0 {
		t.Fatalf("nothing to sum = %v %v %v", v, ok, err)
	}
	if v, ok, err := s.Avg(ctx, "views", where.Page(1, 1), where.Order("views")); err != nil || !ok || v != 20 {
		t.Fatalf("pagination does not narrow an aggregate: %v %v %v", v, ok, err)
	}
}

func TestAggregate_Times(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	base := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC) // a DST change in New York
	for i := range 3 {
		at := base.Add(time.Duration(i) * time.Hour)
		mustCreate(t, ctx, s, &post{Title: fmt.Sprintf("t%d", i), PublishedAt: &at})
	}
	mustCreate(t, ctx, s, &post{Title: "unpublished"})
	lo, ok, err := s.Min[time.Time](ctx, "published_at")
	if err != nil || !ok || !lo.Equal(base) {
		t.Fatalf("Min = %v %v %v", lo, ok, err)
	}
	hi, ok, err := s.Max[time.Time](ctx, "published_at")
	if err != nil || !ok || !hi.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("Max = %v %v %v", hi, ok, err)
	}
	if _, ok, err := s.Min[time.Time](ctx, "published_at", where.Eq("title", "unpublished")); err != nil || ok {
		t.Fatalf("only NULLs: %v %v", ok, err)
	}
	if latest, ok, err := s.Max[time.Time](ctx, "created_at"); err != nil || !ok || time.Since(latest) > time.Hour {
		t.Fatalf("Max created_at = %v %v %v", latest, ok, err)
	}
}

func TestAggregate_KindGates(t *testing.T) {
	s := seedStats(t)
	ctx := t.Context()
	for name, err := range map[string]error{
		"int64 from a float column": second(s.Sum[int64](ctx, "score")),
		"sum of text":               second(s.Sum[float64](ctx, "title")),
		"avg of time":               second(s.Avg(ctx, "created_at")),
		"min of text":               second(s.Min[int64](ctx, "status")),
		"time from a number":        second(s.Max[time.Time](ctx, "views")),
		"number from a time":        second(s.Min[int64](ctx, "created_at")),
		"min of bool":               second(s.Min[int64](ctx, "hidden")),
	} {
		var coder chok.Coder
		if err == nil || errors.As(err, &coder) {
			t.Errorf("%s: want a plain error, got %v", name, err)
		}
	}
	var coder chok.Coder
	if _, _, err := s.Sum[int64](ctx, "nope"); !errors.Is(err, where.ErrUnknownField) || errors.As(err, &coder) {
		t.Fatalf("an unknown field is the code's bug (500): %v", err)
	}
	if n, ok, err := s.Max[int64](ctx, "version"); err != nil || !ok || n < 1 {
		t.Fatalf("a column without a store tag aggregates: %d %v %v", n, ok, err)
	}
}

func second[A, B any](_ A, _ B, err error) error { return err }

func errOf[A any](_ A, err error) error { return err }

func TestAggregate_Int64OverflowFails(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	mustCreate(t, ctx, s, &post{Title: "a", Views: math.MaxInt64}, &post{Title: "b", Views: math.MaxInt64})
	if v, ok, err := s.Sum[int64](ctx, "views"); err == nil {
		t.Fatalf("an overflowing sum returned %d %v", v, ok)
	}
	v, ok, err := s.Sum[float64](ctx, "views")
	if dbtest.Driver() == "sqlite" {
		// SQLite sums integers as integers and overflows itself.
		if err == nil {
			t.Fatalf("SQLite overflow: %v %v", v, ok)
		}
	} else if err != nil || !ok || v < 1.8e19 {
		t.Fatalf("Sum[float64] past int64: %v %v %v", v, ok, err)
	}
}

func TestCountDistinct(t *testing.T) {
	s := seedStats(t)
	ctx := t.Context()
	if n, err := s.CountDistinct(ctx, "status"); err != nil || n != 2 {
		t.Fatalf("statuses = %d %v", n, err)
	}
	if n, err := s.CountDistinct(ctx, "published_at"); err != nil || n != 0 {
		t.Fatalf("only NULLs = %d %v", n, err)
	}
	if n, err := s.CountDistinct(ctx, "status", where.Eq("title", "zzz")); err != nil || n != 0 {
		t.Fatalf("no rows = %d %v", n, err)
	}
}

func TestGroupBy(t *testing.T) {
	s := seedStats(t)
	ctx := t.Context()
	groups, err := s.GroupBy[string](ctx, "status", []store.Aggregate{
		{Func: "count"}, {Func: "sum", Field: "views"}, {Func: "avg", Field: "score"},
		{Func: "max", Field: "created_at"}, {Func: "count_distinct", Field: "title"}, {Func: "min", Field: "published_at"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].Key != "a" || groups[1].Key != "b" {
		t.Fatalf("groups %+v", groups)
	}
	a := groups[0].Values
	if n, ok := a[0].Int64(); !ok || n != 2 {
		t.Fatalf("count %v", a[0])
	}
	if n, ok := a[1].Int64(); !ok || n != 40 {
		t.Fatalf("sum %v", a[1])
	}
	if f, ok := a[2].Float64(); !ok || f != 1 {
		t.Fatalf("avg %v", a[2])
	}
	if _, ok := a[2].Int64(); ok {
		t.Fatal("an avg is not an int64")
	}
	if tm, ok := a[3].Time(); !ok || tm.IsZero() {
		t.Fatalf("max time %v", a[3])
	}
	if n, ok := a[4].Int64(); !ok || n != 2 {
		t.Fatalf("count distinct %v", a[4])
	}
	if !a[5].IsNull() {
		t.Fatalf("min of NULLs %v", a[5])
	}
	if f, ok := groups[1].Values[1].Float64(); !ok || f != 20 {
		t.Fatalf("an int64 reads as float64: %v", groups[1].Values[1])
	}
	b, err := json.Marshal(groups[1])
	if err != nil || !strings.Contains(string(b), `"key":"b"`) || !strings.Contains(string(b), `"values":[1,20,1,"`) || !strings.HasSuffix(string(b), `null]}`) {
		t.Fatalf("JSON %s %v", b, err)
	}
	bools, err := s.GroupBy[bool](ctx, "hidden", []store.Aggregate{{Func: "count"}})
	if err != nil || len(bools) != 2 || bools[0].Key || !bools[1].Key {
		t.Fatalf("bool keys %+v %v", bools, err)
	}
	ints, err := s.GroupBy[int64](ctx, "views", []store.Aggregate{{Func: "count"}}, where.Gt("views", 15))
	if err != nil || len(ints) != 2 || ints[0].Key != 20 {
		t.Fatalf("int keys %+v %v", ints, err)
	}
	empty, err := s.GroupBy[string](ctx, "status", []store.Aggregate{{Func: "count"}}, where.Eq("title", "zzz"))
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("no rows %#v %v", empty, err)
	}
}

func TestGroupBy_Refusals(t *testing.T) {
	s := seedStats(t)
	ctx := t.Context()
	count := []store.Aggregate{{Func: "count"}}
	for name, err := range map[string]error{
		"no aggregates":    errOf(s.GroupBy[string](ctx, "status", nil)),
		"key type":         errOf(s.GroupBy[int64](ctx, "status", count)),
		"unsupported key":  errOf(s.GroupBy[int32](ctx, "views", count)),
		"unknown func":     errOf(s.GroupBy[string](ctx, "status", []store.Aggregate{{Func: "median", Field: "views"}})),
		"count of a field": errOf(s.GroupBy[string](ctx, "status", []store.Aggregate{{Func: "count", Field: "views"}})),
		"sum of text":      errOf(s.GroupBy[string](ctx, "status", []store.Aggregate{{Func: "sum", Field: "title"}})),
		"NULL key":         errOf(s.GroupBy[time.Time](ctx, "published_at", count)),
	} {
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	for _, opt := range []where.Option{where.Order("status"), where.Page(1, 1), where.Count(), where.Limit(1)} {
		if _, err := s.GroupBy[string](ctx, "status", count, opt); !errors.Is(err, chok.ErrBadRequest) {
			t.Errorf("non-filter option: %v", err)
		}
	}
	if _, err := s.GroupBy[time.Time](ctx, "published_at", count, where.NotNull("published_at")); err != nil {
		t.Fatalf("NotNull removes the NULL key: %v", err)
	}
}

func TestAggregate_OwnerScope(t *testing.T) {
	docs := ownedDocs(t)
	ctx := t.Context()
	if n, err := docs.CountDistinct(as(ctx, "alice"), "title"); err != nil || n != 2 {
		t.Fatalf("alice's titles = %d %v", n, err)
	}
	if n, err := docs.CountDistinct(ctx, "title"); err != nil || n != 0 {
		t.Fatalf("anonymous = %d %v", n, err)
	}
	ids, err := docs.Pluck[string](as(ctx, "bob"), "title")
	if err != nil || !slices.Equal(ids, []string{"b1"}) {
		t.Fatalf("bob's titles %v %v", ids, err)
	}
}

func ownedDocs(t *testing.T) *store.Store[doc] {
	t.Helper()
	docs := store.New[doc](openDB(t))
	ctx := t.Context()
	mustCreate(t, as(ctx, "alice"), docs, &doc{Title: "a1"}, &doc{Title: "a2"})
	mustCreate(t, as(ctx, "bob"), docs, &doc{Title: "b1"})
	return docs
}

func TestPluck(t *testing.T) {
	d := openDB(t)
	s := store.New[post](d)
	seed(t, s)
	ctx := t.Context()
	got, err := s.Pluck[string](ctx, "title", where.Order("views", true))
	if err != nil || !slices.Equal(got, []string{"p3", "p2", "p1"}) {
		t.Fatalf("titles %v %v", got, err)
	}
	views, err := s.Pluck[int64](ctx, "views", where.Order("views"), where.Limit(2))
	if err != nil || !slices.Equal(views, []int64{10, 20}) {
		t.Fatalf("views %v %v", views, err)
	}
	rids, err := s.Pluck[string](ctx, "id", where.Eq("title", "p1"))
	if err != nil || len(rids) != 1 || !strings.HasPrefix(rids[0], "pst_") {
		t.Fatalf("id plucks the RID: %v %v", rids, err)
	}
	ids, err := s.PluckIDs(ctx, where.Order("title"))
	if err != nil || len(ids) != 3 || ids[0] == 0 {
		t.Fatalf("ids %v %v", ids, err)
	}
	none, err := s.Pluck[string](ctx, "title", where.Eq("title", "zzz"))
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("empty %#v %v", none, err)
	}
	var coder chok.Coder
	if _, err := s.Pluck[string](ctx, "nope"); !errors.Is(err, where.ErrUnknownField) || errors.As(err, &coder) {
		t.Fatalf("unknown field: %v", err)
	}
	if bodies, err := s.Pluck[string](ctx, "body", where.Eq("title", "zzz")); err != nil || len(bodies) != 0 {
		t.Fatalf("an update-only field plucks: %v %v", bodies, err)
	}
	// Pluck takes every row, not a page: a projection for server code is
	// not clamped by the policy's page sizes, which guard client lists.
	for i := range 30 {
		mustCreate(t, ctx, s, &post{Title: fmt.Sprintf("x%02d", i)})
	}
	capped := store.New[post](d, store.Policy(storePolicy(2, 5)))
	if all, _ := capped.Pluck[string](ctx, "title"); len(all) != 33 {
		t.Fatalf("pluck %d rows", len(all))
	}
	if page, _ := capped.Pluck[string](ctx, "title", where.Order("title"), where.Page(2, 0)); !slices.Equal(page, []string{"p3", "x00"}) {
		t.Fatalf("an explicit page uses the default size: %v", page)
	}
	// Inside a transaction it sees the transaction's rows.
	_ = s.Tx(ctx, func(tx context.Context) error {
		mustCreate(t, tx, s, &post{Title: "in tx"})
		if got, _ := s.Pluck[string](tx, "title", where.Eq("title", "in tx")); len(got) != 1 {
			t.Errorf("in tx %v", got)
		}
		return nil
	})
}

// TestPluck_TwoStepIn: the keys plucked from one store filter another,
// both through their own field lists, with no join through Unsafe.
func TestPluck_TwoStepIn(t *testing.T) {
	d := openDB(t)
	ctx := t.Context()
	s, notes := store.New[post](d), store.New[note](d)
	mustCreate(t, ctx, s, &post{Title: "a", Status: "pick"}, &post{Title: "b"}, &post{Title: "c", Status: "pick"})
	mustCreate(t, ctx, notes, &note{Code: "a"}, &note{Code: "b"}, &note{Code: "c"})
	keys, err := s.Pluck[string](ctx, "title", where.Eq("status", "pick"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := notes.Pluck[string](ctx, "code", where.In("code", keys), where.Order("code"))
	if err != nil || !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("two-step IN: %v %v", got, err)
	}
}

func TestListIn(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	var batch []*post
	for i := range 1200 {
		batch = append(batch, &post{Title: fmt.Sprintf("t%04d", i), Views: int64(i % 7)})
	}
	if err := s.BatchCreate(ctx, batch); err != nil {
		t.Fatal(err)
	}
	var want []string
	for i := 0; i < 1200; i += 2 {
		want = append(want, fmt.Sprintf("t%04d", i))
	}
	values := append(slices.Clone(want), want[:10]...) // duplicates count once
	got, err := s.ListIn(ctx, "title", values)
	if err != nil || len(got) != 600 {
		t.Fatalf("1,210 values in 500s: %d rows %v", len(got), err)
	}
	ints, err := s.ListIn(ctx, "views", []int64{0, 1}, where.Lt("title", "t0100"))
	if err != nil || len(ints) != 30 {
		t.Fatalf("filtered %d %v", len(ints), err)
	}
	none, err := s.ListIn(ctx, "title", []string{})
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("no values %#v %v", none, err)
	}
	// ListIn reads like List: soft-deleted rows are out, and the public
	// id fetches by RID, skipping the missing ones.
	if err := s.Delete(ctx, store.Where(where.Eq("title", "t0002"))); err != nil {
		t.Fatal(err)
	}
	byID, err := s.ListIn(ctx, "id", []string{got[0].RID, got[1].RID, "pst_missing"})
	if err != nil || len(byID) != 1 || byID[0].Title != "t0000" {
		t.Fatalf("by id, one deleted, one missing: %+v %v", byID, err)
	}
	for name, err := range map[string]error{
		"order":        errOf(s.ListIn(ctx, "title", []string{"t0001"}, where.Order("title"))),
		"page":         errOf(s.ListIn(ctx, "title", []string{"t0001"}, where.Page(1, 1))),
		"count":        errOf(s.ListIn(ctx, "title", []string{"t0001"}, where.Count())),
		"empty, order": errOf(s.ListIn(ctx, "title", []string{}, where.Order("title"))),
	} {
		if !errors.Is(err, chok.ErrBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A field the code names wrong is its bug: a 500 that still matches
	// where.ErrUnknownField.
	for name, err := range map[string]error{
		"unknown field":       errOf(s.ListIn(ctx, "nope", []string{"x"})),
		"empty, unknown":      errOf(s.ListIn(ctx, "nope", []string{})),
		"unknown extra field": errOf(s.ListIn(ctx, "title", []string{"t0001"}, where.Eq("nope", 1))),
	} {
		var coder chok.Coder
		if !errors.Is(err, where.ErrUnknownField) || errors.As(err, &coder) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAggregate_SeesRowsInsideTx(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	err := s.Tx(ctx, func(tx context.Context) error {
		mustCreate(t, tx, s, &post{Title: "staged", Views: 7})
		if sum, ok, err := s.Sum[int64](tx, "views"); err != nil || !ok || sum != 7 {
			t.Errorf("Sum inside the transaction = %d %v %v", sum, ok, err)
		}
		if n, err := s.CountDistinct(tx, "title"); err != nil || n != 1 {
			t.Errorf("CountDistinct inside the transaction = %d %v", n, err)
		}
		return errors.New("roll back")
	})
	if err == nil {
		t.Fatal("rolled back")
	}
	if _, ok, _ := s.Sum[int64](ctx, "views"); ok {
		t.Fatal("the rolled-back row is gone")
	}
}

// TestAggregate_MixedOffsetsCompareByInstant: times written in different
// zones aggregate by instant on every dialect — SQLite, which stores the
// text with its offset, through its UTC expression.
func TestAggregate_MixedOffsetsCompareByInstant(t *testing.T) {
	s := posts(t)
	ctx := t.Context()
	// As text the later instant sorts first.
	early := time.Date(2026, 7, 1, 14, 0, 0, 0, time.FixedZone("p14", 14*3600))  // 00:00Z
	late := time.Date(2026, 6, 30, 13, 0, 0, 0, time.FixedZone("m12", -12*3600)) // 01:00Z
	mustCreate(t, ctx, s, &post{Title: "early", PublishedAt: &early}, &post{Title: "late", PublishedAt: &late})
	if lo, ok, err := s.Min[time.Time](ctx, "published_at"); err != nil || !ok || !lo.Equal(early) {
		t.Fatalf("Min = %v %v %v, want %v", lo, ok, err, early.UTC())
	}
	if hi, ok, err := s.Max[time.Time](ctx, "published_at"); err != nil || !ok || !hi.Equal(late) {
		t.Fatalf("Max = %v %v %v, want %v", hi, ok, err, late.UTC())
	}
	// One instant written in two zones is one value.
	inst := time.Date(2026, 7, 2, 8, 30, 0, 123_000_000, time.UTC)
	zoned := inst.In(time.FixedZone("p8", 8*3600))
	mustCreate(t, ctx, s, &post{Title: "utc", Status: "same", PublishedAt: &inst}, &post{Title: "p8", Status: "same", PublishedAt: &zoned})
	groups, err := s.GroupBy[time.Time](ctx, "published_at", []store.Aggregate{{Func: "count"}}, where.Eq("status", "same"))
	if err != nil || len(groups) != 1 || !groups[0].Key.Equal(inst) {
		t.Fatalf("one instant, two zones: %v %v", groups, err)
	}
	if n, err := s.CountDistinct(ctx, "published_at", where.Eq("status", "same")); err != nil || n != 1 {
		t.Fatalf("CountDistinct = %d %v", n, err)
	}
}

// TestAggregate_SQLiteNumericTimes: SQLite may hold a time as a number of
// Unix seconds next to text; both read on one UTC timeline, the number
// never as a Julian day.
func TestAggregate_SQLiteNumericTimes(t *testing.T) {
	if dbtest.Driver() != "sqlite" {
		t.Skip("dynamically typed time storage is SQLite's")
	}
	s := posts(t)
	ctx := t.Context()
	at := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	mustCreate(t, ctx, s, &post{Title: "text", PublishedAt: &at}, &post{Title: "number", PublishedAt: &at})
	if err := s.Unsafe(ctx).Where("title = ?", "number").UpdateColumn("published_at", gorm.Expr("2440588")).Error; err != nil {
		t.Fatal(err)
	}
	want := time.Date(1970, 1, 29, 5, 56, 28, 0, time.UTC) // 2440588 s, not Julian day 2440588
	if lo, ok, err := s.Min[time.Time](ctx, "published_at"); err != nil || !ok || !lo.Equal(want) {
		t.Fatalf("Min = %v %v %v, want %v", lo, ok, err, want)
	}
	if hi, ok, err := s.Max[time.Time](ctx, "published_at"); err != nil || !ok || !hi.Equal(at) {
		t.Fatalf("Max = %v %v %v", hi, ok, err)
	}
	if n, err := s.CountDistinct(ctx, "published_at"); err != nil || n != 2 {
		t.Fatalf("CountDistinct = %d %v", n, err)
	}
}

func TestGroupBy_NoncanonicalBoolErrors(t *testing.T) {
	if dbtest.Driver() == "postgres" {
		t.Skip("a PostgreSQL boolean holds only true and false")
	}
	s := seedStats(t)
	ctx := t.Context()
	if err := s.Unsafe(ctx).Where("title = ?", "p1").UpdateColumn("hidden", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.GroupBy[bool](ctx, "hidden", []store.Aggregate{{Func: "count"}}); err == nil || !strings.Contains(err.Error(), "non-canonical") {
		t.Fatalf("2 is no bool: %v", err)
	}
}

// jsonText declares a JSON column through gorm's GormDBDataType.
type jsonText string

func (jsonText) GormDBDataType(*gorm.DB, *schema.Field) string { return "json" }

// declared declares column types against the Go types.
type declared struct {
	db.Model
	Label   string   `json:"label" store:"query" gorm:"size:20;not null;default:''"`
	Code    string   `json:"code" store:"query" gorm:"type:char(4);not null;default:''"`
	Qty     *int64   `json:"qty" store:"query" gorm:"type:text"` // PostgreSQL cannot even store one
	Raw     []byte   `json:"raw" store:"query"`
	Payload string   `json:"payload" store:"query" gorm:"type:json"`
	Doc     jsonText `json:"doc" store:"query"`
}

// TestAggregate_DeclaredTypes: the Go type picks the result type, and a
// declared column type that would make the database compute something
// else is refused before any query — text under a number, JSON, bytes.
func TestAggregate_DeclaredTypes(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&declared{})))
	s := store.New[declared](d)
	ctx := t.Context()
	for i, label := range []string{"a", "b", "a"} {
		obj := &declared{Label: label, Code: label + []string{"bc", "xy", "bc"}[i], Payload: `{}`, Doc: `{}`}
		mustCreate(t, ctx, s, obj)
	}
	for name, err := range map[string]error{
		"Min of text-declared int":        second(s.Min[int64](ctx, "qty")),
		"Sum of text-declared int":        second(s.Sum[float64](ctx, "qty")),
		"group sum of text-declared int":  errOf(s.GroupBy[string](ctx, "label", []store.Aggregate{{Func: "max", Field: "qty"}})),
		"CountDistinct of bytes":          errOf(s.CountDistinct(ctx, "raw")),
		"CountDistinct of a json type":    errOf(s.CountDistinct(ctx, "payload")),
		"CountDistinct of GormDBDataType": errOf(s.CountDistinct(ctx, "doc")),
		"GroupBy a json key":              errOf(s.GroupBy[string](ctx, "payload", []store.Aggregate{{Func: "count"}})),
	} {
		var coder chok.Coder
		if err == nil || errors.As(err, &coder) {
			t.Errorf("%s: want a plain (500) error, got %v", name, err)
		}
	}
	// A CHAR column is text and groups like one.
	groups, err := s.GroupBy[string](ctx, "code", []store.Aggregate{{Func: "count"}})
	if err != nil || len(groups) != 2 || strings.TrimSpace(groups[0].Key) != "abc" {
		t.Fatalf("char groups %v %v", groups, err)
	}
	if n, err := s.CountDistinct(ctx, "label"); err != nil || n != 2 {
		t.Fatalf("CountDistinct label = %d %v", n, err)
	}
}

func TestAggregate_QualifiedAlias(t *testing.T) {
	d := openDB(t)
	ctx := t.Context()
	own := store.New[post](d, store.Alias("views", "posts.views"))
	mustCreate(t, ctx, own, &post{Title: "a", Views: 4}, &post{Title: "b", Views: 6})
	if sum, ok, err := own.Sum[int64](ctx, "views"); err != nil || !ok || sum != 10 {
		t.Fatalf("own-table qualifier: %d %v %v", sum, ok, err)
	}
	if groups, err := own.GroupBy[string](ctx, "title", []store.Aggregate{{Func: "sum", Field: "views"}}); err != nil || len(groups) != 2 {
		t.Fatalf("grouped: %v %v", groups, err)
	}
	foreign := store.New[post](d, store.Alias("views", "stats.views"))
	if _, _, err := foreign.Sum[int64](ctx, "views"); err == nil || !strings.Contains(err.Error(), "another table") {
		t.Fatalf("a foreign qualifier: %v", err)
	}
}

// TestAggregate_ConcurrentFirstUse: a fresh store's first aggregates race
// for nothing (under -race on every lane).
func TestAggregate_ConcurrentFirstUse(t *testing.T) {
	seeded := seedStats(t)
	ctx := t.Context()
	errs := make(chan error, 16)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			var err error
			switch i % 4 {
			case 0:
				_, _, err = seeded.Max[time.Time](ctx, "created_at")
			case 1:
				_, _, err = seeded.Sum[int64](ctx, "views")
			case 2:
				_, err = seeded.GroupBy[string](ctx, "status", []store.Aggregate{{Func: "min", Field: "created_at"}})
			case 3:
				_, err = seeded.CountDistinct(ctx, "title")
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// ledger is an owned model with a number and a time to aggregate.
type ledger struct {
	db.OwnedModel
	Kind   string    `json:"kind" store:"query" gorm:"size:20;not null"`
	Amount int64     `json:"amount" store:"query" gorm:"not null"`
	At     time.Time `json:"at" store:"query" gorm:"not null"`
}

// TestAggregate_EveryStatementIsScoped: every statement an aggregate or a
// projection sends to an owned table carries the owner predicate — no
// metadata probe reads another owner's row on the way.
func TestAggregate_EveryStatementIsScoped(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&ledger{})))
	s := store.New[ledger](d)
	alice := as(t.Context(), "alice")
	for i := range 3 {
		mustCreate(t, alice, s, &ledger{Kind: "k", Amount: int64(i), At: time.Now()})
	}
	mustCreate(t, as(t.Context(), "bob"), s, &ledger{Kind: "k", Amount: 100, At: time.Now()})
	var mu sync.Mutex
	var sqls []string
	capture := func(tx *gorm.DB) {
		mu.Lock()
		sqls = append(sqls, tx.Statement.SQL.String())
		mu.Unlock()
	}
	gdb := d.Unsafe(t.Context())
	if err := gdb.Callback().Query().After("gorm:query").Register("test:capture", capture); err != nil {
		t.Fatal(err)
	}
	if err := gdb.Callback().Row().After("gorm:row").Register("test:capture", capture); err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{alice, t.Context()} {
		sqls = nil
		sum, _, _ := s.Sum[int64](ctx, "amount")
		_, _, _ = s.Avg(ctx, "amount")
		_, _, _ = s.Min[time.Time](ctx, "at")
		_, _, _ = s.Max[int64](ctx, "amount")
		_, _ = s.CountDistinct(ctx, "kind")
		_, _ = s.GroupBy[string](ctx, "kind", []store.Aggregate{{Func: "count"}, {Func: "sum", Field: "amount"}})
		_, _ = s.Pluck[int64](ctx, "amount")
		_, _ = s.PluckIDs(ctx)
		_, _ = s.ListIn(ctx, "kind", []string{"k"})
		if ctx == alice && sum != 3 {
			t.Fatalf("alice's sum %d", sum)
		}
		if len(sqls) < 9 {
			t.Fatalf("captured %d statements", len(sqls))
		}
		for _, q := range sqls {
			if !strings.Contains(q, "owner_id") && !strings.Contains(q, "1 = 0") {
				t.Errorf("an unfenced statement: %s", q)
			}
		}
	}
}
