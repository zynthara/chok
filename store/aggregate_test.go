package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlite "github.com/glebarez/sqlite"
	gomysql "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// Arch-backlog #7 regression tests: aggregates are the front door for
// the dashboard reads that previously required Unsafe — allowlist, scope
// and soft-delete semantics identical to Count, results converged to
// declared Go types across the three dialects.

type AggSale struct {
	db.SoftDeleteModel
	Status string    `json:"status" gorm:"size:16;not null"`
	Qty    int64     `json:"qty" gorm:"not null"`
	Price  float64   `json:"price" gorm:"not null"`
	Rating *int64    `json:"rating"`
	Flag   bool      `json:"flag" gorm:"not null"`
	At     time.Time `json:"at" gorm:"not null"`
	Meta   []byte    `json:"meta"`
}

func (AggSale) RIDPrefix() string { return "ags" }

func setupAggStore(t *testing.T, opts ...StoreOption) *Store[AggSale] {
	t.Helper()
	return setupAggStoreOn(t, setupDB(t), opts...)
}

func setupAggStoreOn(t *testing.T, gdb *db.DB, opts ...StoreOption) *Store[AggSale] {
	t.Helper()
	if err := gdb.Migrate(context.Background(), db.Table(&AggSale{})); err != nil {
		t.Fatal(err)
	}
	return New[AggSale](gdb, log.Empty(),
		append([]StoreOption{WithQueryFields("id", "status", "qty", "price", "rating", "flag", "at", "meta", "created_at")}, opts...)...)
}

var aggT0 = time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)

// seedAggSales inserts a fixed, hand-checkable data set:
//
//	status  qty  price  rating  flag  at
//	paid     10   1.50    5     true  t0
//	paid     20   2.25    NULL  true  t0+1h
//	draft     3   0.25    1     false t0+2h
func seedAggSales(t *testing.T, s *Store[AggSale]) {
	t.Helper()
	five, one := int64(5), int64(1)
	for _, row := range []*AggSale{
		{Status: "paid", Qty: 10, Price: 1.50, Rating: &five, Flag: true, At: aggT0},
		{Status: "paid", Qty: 20, Price: 2.25, Rating: nil, Flag: true, At: aggT0.Add(time.Hour)},
		{Status: "draft", Qty: 3, Price: 0.25, Rating: &one, Flag: false, At: aggT0.Add(2 * time.Hour)},
	} {
		if err := s.Create(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAggregate_SumAvgMinMax(t *testing.T) {
	s := setupAggStore(t)
	seedAggSales(t, s)
	ctx := context.Background()

	sum, ok, err := Sum[int64](ctx, s, "qty")
	if err != nil || !ok || sum != 33 {
		t.Fatalf("Sum[int64] qty = %d, %v, %v; want 33, true, nil", sum, ok, err)
	}
	fsum, ok, err := Sum[float64](ctx, s, "price")
	if err != nil || !ok || fsum != 4.0 {
		t.Fatalf("Sum[float64] price = %v, %v, %v; want 4.0", fsum, ok, err)
	}
	// float64 may widen an integer column (documented 2^53 trade).
	wide, ok, err := Sum[float64](ctx, s, "qty")
	if err != nil || !ok || wide != 33.0 {
		t.Fatalf("Sum[float64] qty = %v, %v, %v; want 33.0", wide, ok, err)
	}
	avg, ok, err := Avg(ctx, s, "qty")
	if err != nil || !ok || math.Abs(avg-11.0) > 1e-9 {
		t.Fatalf("Avg qty = %v, %v, %v; want 11.0", avg, ok, err)
	}
	lo, ok, err := Min[int64](ctx, s, "qty")
	if err != nil || !ok || lo != 3 {
		t.Fatalf("Min qty = %d, %v, %v; want 3", lo, ok, err)
	}
	hi, ok, err := Max[float64](ctx, s, "price")
	if err != nil || !ok || hi != 2.25 {
		t.Fatalf("Max price = %v, %v, %v; want 2.25", hi, ok, err)
	}

	// Filters narrow the aggregated set like any read.
	paid, ok, err := Sum[int64](ctx, s, "qty", where.WithFilter("status", "paid"))
	if err != nil || !ok || paid != 30 {
		t.Fatalf("filtered Sum = %d, %v, %v; want 30", paid, ok, err)
	}
}

func TestAggregate_TimeMinMax(t *testing.T) {
	s := setupAggStore(t)
	seedAggSales(t, s)
	ctx := context.Background()

	first, ok, err := Min[time.Time](ctx, s, "at")
	if err != nil || !ok {
		t.Fatalf("Min at = %v, %v", ok, err)
	}
	if !first.Equal(aggT0) {
		t.Fatalf("Min at = %v, want %v (compare instants)", first, aggT0)
	}
	last, ok, err := Max[time.Time](ctx, s, "at")
	if err != nil || !ok || !last.Equal(aggT0.Add(2*time.Hour)) {
		t.Fatalf("Max at = %v, %v, %v; want t0+2h", last, ok, err)
	}
}

func TestAggregate_NullSemantics(t *testing.T) {
	s := setupAggStore(t)
	ctx := context.Background()

	// Zero rows: SQL NULL surfaces as ok=false with the zero value —
	// never conflated with a real zero sum.
	if sum, ok, err := Sum[int64](ctx, s, "qty"); err != nil || ok || sum != 0 {
		t.Fatalf("Sum over zero rows = %d, %v, %v; want 0, false, nil", sum, ok, err)
	}
	if _, ok, err := Avg(ctx, s, "qty"); err != nil || ok {
		t.Fatalf("Avg over zero rows: ok=%v err=%v; want false, nil", ok, err)
	}
	if _, ok, err := Min[time.Time](ctx, s, "at"); err != nil || ok {
		t.Fatalf("Min over zero rows: ok=%v err=%v; want false, nil", ok, err)
	}
	// COUNT never returns NULL: zero rows count as 0 with no ok bool.
	if n, err := CountDistinct(ctx, s, "status"); err != nil || n != 0 {
		t.Fatalf("CountDistinct over zero rows = %d, %v; want 0", n, err)
	}

	seedAggSales(t, s)
	// NULLs do not contribute: two non-NULL ratings (5 + 1).
	if sum, ok, err := Sum[int64](ctx, s, "rating"); err != nil || !ok || sum != 6 {
		t.Fatalf("Sum rating = %d, %v, %v; want 6 (NULLs skipped)", sum, ok, err)
	}
	if avg, ok, err := Avg(ctx, s, "rating"); err != nil || !ok || math.Abs(avg-3.0) > 1e-9 {
		t.Fatalf("Avg rating = %v, %v, %v; want 3.0 (NULLs skipped)", avg, ok, err)
	}
	// Matching rows whose every value is NULL: still ok=false.
	if _, ok, err := Sum[int64](ctx, s, "rating", where.WithFilter("qty", 20)); err != nil || ok {
		t.Fatalf("all-NULL Sum: ok=%v err=%v; want false, nil", ok, err)
	}
}

func TestAggregate_KindGates(t *testing.T) {
	s := setupAggStore(t)
	ctx := context.Background()

	// String columns are not aggregatable in v1 (collation-defined order).
	if _, _, err := Sum[int64](ctx, s, "status"); err == nil || !strings.Contains(err.Error(), "numeric") {
		t.Fatalf("Sum over a string column must be rejected, got %v", err)
	}
	if _, _, err := Min[int64](ctx, s, "status"); err == nil {
		t.Fatal("Min over a string column must be rejected")
	}
	// Sum/Avg never accept time columns.
	if _, _, err := Sum[float64](ctx, s, "at"); err == nil {
		t.Fatal("Sum over a time column must be rejected")
	}
	if _, _, err := Avg(ctx, s, "at"); err == nil {
		t.Fatal("Avg over a time column must be rejected")
	}
	// int64 never narrows a float column — rejected up front, not at scan.
	if _, _, err := Sum[int64](ctx, s, "price"); err == nil || !strings.Contains(err.Error(), "float64") {
		t.Fatalf("Sum[int64] over a float column must be rejected, got %v", err)
	}
	if _, _, err := Min[int64](ctx, s, "price"); err == nil {
		t.Fatal("Min[int64] over a float column must be rejected")
	}
	// time.Time only reads time columns.
	if _, _, err := Max[time.Time](ctx, s, "qty"); err == nil {
		t.Fatal("Max[time.Time] over an integer column must be rejected")
	}
}

func TestAggregate_UnknownFieldStaysRaw(t *testing.T) {
	// Arch-backlog #3 provenance: aggregate field names are server code —
	// a typo keeps the raw where.ErrUnknownField (500), never a
	// client-shaped 400.
	s := setupAggStore(t)
	ctx := context.Background()

	if _, _, err := Sum[int64](ctx, s, "typo"); !errors.Is(err, where.ErrUnknownField) {
		t.Fatalf("Sum unknown field must surface raw ErrUnknownField, got %v", err)
	}
	if errIs400(func() error { _, _, err := Sum[int64](ctx, s, "typo"); return err }()) {
		t.Fatal("Sum unknown field must not map to a client 400")
	}
	// A real column outside the allowlist is equally rejected — the
	// aggregates cannot become a side door around WithQueryFields.
	if _, _, err := Max[time.Time](ctx, s, "updated_at"); !errors.Is(err, where.ErrUnknownField) {
		t.Fatalf("undeclared column must surface raw ErrUnknownField, got %v", err)
	}
	if _, err := CountDistinct(ctx, s, "typo"); !errors.Is(err, where.ErrUnknownField) {
		t.Fatalf("CountDistinct unknown field must surface raw ErrUnknownField, got %v", err)
	}
	if _, err := GroupBy[string](ctx, s, "typo", []Aggregate{CountRows()}); !errors.Is(err, where.ErrUnknownField) {
		t.Fatalf("GroupBy unknown group field must surface raw ErrUnknownField, got %v", err)
	}
	if _, err := GroupBy[string](ctx, s, "status", []Aggregate{SumOf("typo")}); !errors.Is(err, where.ErrUnknownField) {
		t.Fatalf("GroupBy unknown aggregate field must surface raw ErrUnknownField, got %v", err)
	}
}

func errIs400(err error) bool { return errors.Is(err, apierr.ErrInvalidArgument) }

func TestAggregate_PaginationStrippedLikeCount(t *testing.T) {
	// Single-value aggregates are total-shaped: pagination and ordering
	// options are stripped exactly like Count strips them.
	s := setupAggStore(t)
	seedAggSales(t, s)
	ctx := context.Background()

	sum, ok, err := Sum[int64](ctx, s, "qty",
		where.WithPage(1, 1), where.WithOrder("qty", true))
	if err != nil || !ok || sum != 33 {
		t.Fatalf("Sum with page/order options = %d, %v, %v; want the full 33", sum, ok, err)
	}
}

func TestAggregate_OwnerScopeIsolation(t *testing.T) {
	s := setupProductStore(t) // db.OwnedModel: automatic fail-closed OwnerScope
	alice, bob := userCtx("usr_alice"), userCtx("usr_bob")
	for _, d := range []struct {
		ctx  context.Context
		name string
	}{
		{alice, "a1"}, {alice, "a2"}, {bob, "b1"},
	} {
		if err := s.Create(d.ctx, &Product{Name: d.name}); err != nil {
			t.Fatal(err)
		}
	}

	// Aggregates see only the caller's rows — owner A's numbers must not
	// include owner B's.
	if n, err := CountDistinct(alice, s, "name"); err != nil || n != 2 {
		t.Fatalf("alice CountDistinct = %d, %v; want 2", n, err)
	}
	groups, err := GroupBy[string](alice, s, "name", []Aggregate{CountRows()})
	if err != nil || len(groups) != 2 {
		t.Fatalf("alice GroupBy = %v, %v; want her 2 groups", groups, err)
	}
	// Fail-closed: no principal means an error, not global numbers.
	if _, err := CountDistinct(context.Background(), s, "name"); !errors.Is(err, apierr.ErrUnauthenticated) {
		t.Fatalf("unauthenticated aggregate must fail closed, got %v", err)
	}
	if _, err := GroupBy[string](context.Background(), s, "name", []Aggregate{CountRows()}); !errors.Is(err, apierr.ErrUnauthenticated) {
		t.Fatalf("unauthenticated GroupBy must fail closed, got %v", err)
	}
}

func TestAggregate_ExcludesSoftDeleted(t *testing.T) {
	s := setupAggStore(t)
	seedAggSales(t, s)
	ctx := context.Background()

	// Soft-delete the qty=20 row; every aggregate must forget it.
	page, err := s.List(ctx, where.WithFilter("qty", 20))
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("locate row: %v, %v", page, err)
	}
	if err := s.Delete(ctx, RID(page.Items[0].RID)); err != nil {
		t.Fatal(err)
	}

	if sum, ok, err := Sum[int64](ctx, s, "qty"); err != nil || !ok || sum != 13 {
		t.Fatalf("Sum after soft delete = %d, %v, %v; want 13", sum, ok, err)
	}
	groups, err := GroupBy[string](ctx, s, "status", []Aggregate{CountRows()})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if n, _ := g.Values[0].Int64(); g.Key == "paid" && n != 1 {
			t.Fatalf("paid group counts %d rows after soft delete, want 1", n)
		}
	}
}

func TestGroupBy_MultipleAggregates(t *testing.T) {
	s := setupAggStore(t)
	seedAggSales(t, s)
	ctx := context.Background()

	groups, err := GroupBy[string](ctx, s, "status", []Aggregate{
		CountRows(), SumOf("qty"), AvgOf("qty"), MinOf("price"), MaxOf("at"), CountDistinctOf("qty"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	// Deterministic output: ordered by group key ascending.
	if groups[0].Key != "draft" || groups[1].Key != "paid" {
		t.Fatalf("group order = [%s %s], want [draft paid]", groups[0].Key, groups[1].Key)
	}

	draft, paid := groups[0], groups[1]
	if n, ok := draft.Values[0].Int64(); !ok || n != 1 {
		t.Fatalf("draft count = %d, %v; want 1", n, ok)
	}
	if n, ok := paid.Values[1].Int64(); !ok || n != 30 {
		t.Fatalf("paid sum qty = %d, %v; want 30", n, ok)
	}
	if avg, ok := paid.Values[2].Float64(); !ok || math.Abs(avg-15.0) > 1e-9 {
		t.Fatalf("paid avg qty = %v, %v; want 15.0", avg, ok)
	}
	if lo, ok := paid.Values[3].Float64(); !ok || lo != 1.50 {
		t.Fatalf("paid min price = %v, %v; want 1.5", lo, ok)
	}
	if at, ok := paid.Values[4].Time(); !ok || !at.Equal(aggT0.Add(time.Hour)) {
		t.Fatalf("paid max at = %v, %v; want t0+1h", at, ok)
	}
	if n, ok := paid.Values[5].Int64(); !ok || n != 2 {
		t.Fatalf("paid distinct qty = %d, %v; want 2", n, ok)
	}

	// Filters narrow the row set BEFORE grouping (WHERE, not HAVING).
	groups, err = GroupBy[string](ctx, s, "status", []Aggregate{SumOf("qty")},
		where.WithFilterOp("qty", where.Gte, 10))
	if err != nil || len(groups) != 1 || groups[0].Key != "paid" {
		t.Fatalf("filtered groups = %v, %v; want just paid", groups, err)
	}
}

func TestGroupBy_KeyKinds(t *testing.T) {
	s := setupAggStore(t)
	seedAggSales(t, s)
	ctx := context.Background()

	// Bool keys: SQLite hands back 0/1 integers, PostgreSQL native bools.
	flags, err := GroupBy[bool](ctx, s, "flag", []Aggregate{CountRows()})
	if err != nil || len(flags) != 2 {
		t.Fatalf("bool groups = %v, %v; want 2", flags, err)
	}
	// Integer keys.
	qtys, err := GroupBy[int64](ctx, s, "qty", []Aggregate{CountRows()})
	if err != nil || len(qtys) != 3 || qtys[0].Key != 3 {
		t.Fatalf("int64 groups = %v, %v; want [3 10 20]", qtys, err)
	}

	// K must match the column kind exactly, checked before the query.
	if _, err := GroupBy[int64](ctx, s, "status", []Aggregate{CountRows()}); err == nil {
		t.Fatal("K=int64 over a string column must be rejected")
	}
	if _, err := GroupBy[string](ctx, s, "qty", []Aggregate{CountRows()}); err == nil {
		t.Fatal("K=string over an integer column must be rejected")
	}
	// Unsupported key types are named in the error.
	if _, err := GroupBy[int32](ctx, s, "qty", []Aggregate{CountRows()}); err == nil || !strings.Contains(err.Error(), "int32") {
		t.Fatalf("unsupported K must be rejected by name, got %v", err)
	}
}

func TestGroupBy_NullKeyRejected(t *testing.T) {
	s := setupAggStore(t)
	seedAggSales(t, s)
	ctx := context.Background()

	// rating carries a NULL: grouping by it must fail loudly, never fold
	// the NULL group into a zero-value bucket.
	_, err := GroupBy[int64](ctx, s, "rating", []Aggregate{CountRows()})
	if err == nil || !strings.Contains(err.Error(), "WithFilterNotNull") {
		t.Fatalf("NULL group key must error and point at WithFilterNotNull, got %v", err)
	}
	// The documented escape works.
	groups, err := GroupBy[int64](ctx, s, "rating", []Aggregate{CountRows()},
		where.WithFilterNotNull("rating"))
	if err != nil || len(groups) != 2 {
		t.Fatalf("filtered NULL-free grouping = %v, %v; want 2 groups", groups, err)
	}
}

func TestGroupBy_GuardRejectsNonFilterOptions(t *testing.T) {
	// Non-filter options are REJECTED, not stripped: a silently dropped
	// WithOrder+WithLimit would masquerade as top-N while returning
	// key-ordered groups (the ListIn guard precedent).
	s := setupAggStore(t)
	ctx := context.Background()
	aggs := []Aggregate{CountRows()}

	for name, opt := range map[string]where.Option{
		"order":  where.WithOrder("qty"),
		"page":   where.WithPage(1, 10),
		"limit":  where.WithLimit(5),
		"offset": where.WithOffset(5),
		"cursor": where.WithCursor("qty", where.CursorAfter, 1, 5),
		"cap":    where.WithMaxPageSize(5),
		// Round-1 review #5: WithCount used to slip through unseen —
		// ApplyFiltersOnly's countOnly mode never records it into the
		// Config, so the guard now runs under where.Apply.
		"count": where.WithCount(),
	} {
		if _, err := GroupBy[string](ctx, s, "status", aggs, opt); !errIs400(err) {
			t.Fatalf("GroupBy must reject %s options, got %v", name, err)
		}
	}
}

func TestGroupBy_EmptyAndZeroRows(t *testing.T) {
	s := setupAggStore(t)
	ctx := context.Background()

	if _, err := GroupBy[string](ctx, s, "status", nil); err == nil {
		t.Fatal("GroupBy without aggregates must be rejected")
	}
	if _, err := GroupBy[string](ctx, s, "status", []Aggregate{{}}); err == nil {
		t.Fatal("a zero-value Aggregate must be rejected")
	}
	groups, err := GroupBy[string](ctx, s, "status", []Aggregate{CountRows()})
	if err != nil {
		t.Fatal(err)
	}
	if groups == nil || len(groups) != 0 {
		t.Fatalf("zero rows must return an empty non-nil slice, got %#v", groups)
	}
}

func TestGroupBy_NullAggregateValue(t *testing.T) {
	s := setupAggStore(t)
	seedAggSales(t, s)
	ctx := context.Background()

	groups, err := GroupBy[string](ctx, s, "status", []Aggregate{SumOf("rating"), CountRows()},
		where.WithFilter("qty", 20))
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups = %v, %v; want the one paid group", groups, err)
	}
	v := groups[0].Values[0]
	if !v.IsNull() {
		t.Fatal("an all-NULL group aggregate must report IsNull")
	}
	if _, ok := v.Int64(); ok {
		t.Fatal("accessors on a NULL aggregate must return ok=false")
	}
	if _, ok := v.Float64(); ok {
		t.Fatal("Float64 on a NULL aggregate must return ok=false")
	}
	if n, ok := groups[0].Values[1].Int64(); !ok || n != 1 {
		t.Fatalf("count next to a NULL aggregate = %d, %v; want 1", n, ok)
	}
}

func TestAggregate_AccessorKindDiscipline(t *testing.T) {
	s := setupAggStore(t)
	seedAggSales(t, s)
	ctx := context.Background()

	groups, err := GroupBy[string](ctx, s, "status", []Aggregate{SumOf("qty"), MinOf("price"), MaxOf("at")})
	if err != nil {
		t.Fatal(err)
	}
	paid := groups[1]
	// Float64 widens an int64-kind value (one accessor for dashboards)...
	if f, ok := paid.Values[0].Float64(); !ok || f != 30.0 {
		t.Fatalf("Float64 over int sum = %v, %v; want 30.0", f, ok)
	}
	// ...but Int64 never truncates a float-kind value...
	if _, ok := paid.Values[1].Int64(); ok {
		t.Fatal("Int64 over a float aggregate must refuse (would truncate)")
	}
	// ...and Time only reads time-kind values.
	if _, ok := paid.Values[0].Time(); ok {
		t.Fatal("Time over a numeric aggregate must refuse")
	}
	if _, ok := paid.Values[2].Int64(); ok {
		t.Fatal("Int64 over a time aggregate must refuse")
	}
}

func TestCountDistinct_Values(t *testing.T) {
	s := setupAggStore(t)
	seedAggSales(t, s)
	ctx := context.Background()

	// Three rows, two distinct statuses.
	if n, err := CountDistinct(ctx, s, "status"); err != nil || n != 2 {
		t.Fatalf("CountDistinct status = %d, %v; want 2", n, err)
	}
	// COUNT(DISTINCT col) ignores NULLs: 5 and 1.
	if n, err := CountDistinct(ctx, s, "rating"); err != nil || n != 2 {
		t.Fatalf("CountDistinct rating = %d, %v; want 2", n, err)
	}
	// Any declared field qualifies — strings included.
	if n, err := CountDistinct(ctx, s, "status", where.WithFilter("status", "paid")); err != nil || n != 1 {
		t.Fatalf("filtered CountDistinct = %d, %v; want 1", n, err)
	}
}

func TestAggregate_SeesRowsInsideTx(t *testing.T) {
	s := setupAggStore(t)
	ctx := context.Background()

	err := s.Tx(ctx, func(tx *Store[AggSale]) error {
		if err := tx.Create(ctx, &AggSale{Status: "paid", Qty: 7, Price: 1, Flag: true, At: aggT0}); err != nil {
			return err
		}
		sum, ok, err := Sum[int64](ctx, tx, "qty")
		if err != nil {
			return err
		}
		if !ok || sum != 7 {
			t.Fatalf("Sum inside tx = %d, %v; want the uncommitted 7", sum, ok)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestAggregate_Round1MixedOffsetInstantOrder is the round-1 review #1
// regression: SQLite stores timestamps as text in the writer's zone, so
// raw MIN/MAX compared lexicographically and picked the wrong INSTANT
// across mixed offsets, and one instant written under two offsets
// grouped/counted as two values. Aggregates now read SQLite time columns
// through a UTC-normalising expression; PostgreSQL and MySQL compare
// instants natively — the assertions here are the dialect-convergence
// contract and run on every lane.
func TestAggregate_Round1MixedOffsetInstantOrder(t *testing.T) {
	s := setupAggStore(t)
	ctx := context.Background()

	// Lexicographic text order INVERTS instant order for this pair:
	// "2026-06-30 13:00:00-12:00" sorts first but is the LATER instant.
	early := time.Date(2026, 7, 1, 14, 0, 0, 0, time.FixedZone("p14", 14*3600))  // 2026-07-01T00:00:00Z
	late := time.Date(2026, 6, 30, 13, 0, 0, 0, time.FixedZone("m12", -12*3600)) // 2026-07-01T01:00:00Z
	for _, at := range []time.Time{early, late} {
		if err := s.Create(ctx, &AggSale{Status: "mix", Qty: 1, Price: 1, Flag: true, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	lo, ok, err := Min[time.Time](ctx, s, "at")
	if err != nil || !ok || !lo.Equal(early) {
		t.Fatalf("Min across mixed offsets = %v, %v, %v; want the instant-min %v", lo, ok, err, early.UTC())
	}
	hi, ok, err := Max[time.Time](ctx, s, "at")
	if err != nil || !ok || !hi.Equal(late) {
		t.Fatalf("Max across mixed offsets = %v, %v, %v; want the instant-max %v", hi, ok, err, late.UTC())
	}

	// One instant written under two zones is ONE value: one group, one
	// distinct count — never two buckets that differ only by offset text.
	inst := time.Date(2026, 7, 2, 8, 30, 0, 123_000_000, time.UTC)
	for _, at := range []time.Time{inst, inst.In(time.FixedZone("p8", 8*3600))} {
		if err := s.Create(ctx, &AggSale{Status: "same", Qty: 1, Price: 1, Flag: true, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	groups, err := GroupBy[time.Time](ctx, s, "at", []Aggregate{CountRows()},
		where.WithFilter("status", "same"))
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || !groups[0].Key.Equal(inst) {
		t.Fatalf("same instant under two offsets grouped as %d buckets (%v); want 1", len(groups), groups)
	}
	if n, _ := groups[0].Values[0].Int64(); n != 2 {
		t.Fatalf("the merged instant group counts %d rows, want 2", n)
	}
	n, err := CountDistinct(ctx, s, "at", where.WithFilter("status", "same"))
	if err != nil || n != 1 {
		t.Fatalf("CountDistinct over one instant in two zones = %d, %v; want 1", n, err)
	}
}

// TestGroupBy_Round1BoolNoncanonicalErrors is the round-1 review #3
// regression: SQL groups raw 1 and 2 as two buckets, and folding both
// onto Go true handed the caller duplicate Group keys that silently
// overwrite each other in a map. Non-canonical storage now errors.
func TestGroupBy_Round1BoolNoncanonicalErrors(t *testing.T) {
	if dbtest.Driver() == "postgres" {
		t.Skip("postgres bool columns cannot store non-canonical integers")
	}
	s := setupAggStore(t)
	seedAggSales(t, s)
	ctx := context.Background()

	gdb, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec("UPDATE agg_sales SET flag = 2 WHERE status = 'draft'").Error; err != nil {
		t.Fatal(err)
	}
	_, err = GroupBy[bool](ctx, s, "flag", []Aggregate{CountRows()})
	if err == nil || !strings.Contains(err.Error(), "non-canonical") {
		t.Fatalf("non-canonical bool storage must error loudly, got %v", err)
	}
}

// TestCountDistinct_Round1NonComparableRejected is the round-1 review #4
// regression: the CountDistinct contract is comparable scalars, not "any
// declared field" — a column with no derivable scalar kind (bytes, JSON)
// is refused up front as a server-side error instead of surfacing a
// database comparison failure mid-query.
func TestCountDistinct_Round1NonComparableRejected(t *testing.T) {
	s := setupAggStore(t)
	ctx := context.Background()

	_, err := CountDistinct(ctx, s, "meta")
	if err == nil || !strings.Contains(err.Error(), "scalar") {
		t.Fatalf("CountDistinct over a bytes column must be rejected, got %v", err)
	}
	if errIs400(err) {
		t.Fatal("the rejection is a server-side error, not a client 400")
	}
	_, err = GroupBy[string](ctx, s, "status", []Aggregate{CountDistinctOf("meta")})
	if err == nil || !strings.Contains(err.Error(), "scalar") {
		t.Fatalf("CountDistinctOf over a bytes column must be rejected, got %v", err)
	}
}

// TestAggregate_Round1QualifiedAliasColumn is the round-1 review #6
// regression: the allowlist legally maps a field to a table-qualified
// column, which the schema lookup used to miss. A qualifier naming the
// model's own table now resolves; a foreign qualifier is refused with an
// explanation instead of a bare "missing from schema".
func TestAggregate_Round1QualifiedAliasColumn(t *testing.T) {
	s := setupAggStore(t, WithColumnAlias("qty", "agg_sales.qty"))
	seedAggSales(t, s)
	ctx := context.Background()

	sum, ok, err := Sum[int64](ctx, s, "qty")
	if err != nil || !ok || sum != 33 {
		t.Fatalf("Sum over an own-table qualified alias = %d, %v, %v; want 33", sum, ok, err)
	}
	groups, err := GroupBy[string](ctx, s, "status", []Aggregate{SumOf("qty")})
	if err != nil || len(groups) != 2 {
		t.Fatalf("GroupBy with qualified aggregate column = %v, %v; want 2 groups", groups, err)
	}

	foreign := setupAggStore(t, WithColumnAlias("qty", "other_table.qty"))
	_, _, err = Sum[int64](ctx, foreign, "qty")
	if err == nil || !strings.Contains(err.Error(), "own table") {
		t.Fatalf("a foreign-table qualifier must be refused by name, got %v", err)
	}
}

// aggJSONDoc exercises the round-2 review #3 gate: a Go string field
// declared as a database JSON column. The wire kind is str, but JSON
// documents are not comparable scalars on every dialect (PostgreSQL
// json has no equality operator), so aggregation must refuse at entry.
type aggJSONDoc struct {
	db.Model
	Payload string `json:"payload" gorm:"type:json"`
}

func (aggJSONDoc) RIDPrefix() string { return "ajd" }

// TestAggregate_Round2JSONColumnRejected is the round-2 review #3
// regression: the wire-kind gate alone let a string field declared
// gorm:"type:json" through, and PostgreSQL then failed COUNT(DISTINCT)
// mid-query — json (unlike jsonb) has no equality operator. The gate now
// also inspects the declared database type, uniformly on every dialect.
func TestAggregate_Round2JSONColumnRejected(t *testing.T) {
	gdb := setupDB(t)
	ctx := context.Background()
	if err := gdb.Migrate(ctx, db.Table(&aggJSONDoc{})); err != nil {
		t.Fatal(err)
	}
	s := New[aggJSONDoc](gdb, log.Empty(), WithQueryFields("id", "payload"))

	_, err := CountDistinct(ctx, s, "payload")
	if err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("CountDistinct over a json column must be refused by name, got %v", err)
	}
	if errIs400(err) {
		t.Fatal("the rejection is a server-side error, not a client 400")
	}
	if _, err := GroupBy[string](ctx, s, "payload", []Aggregate{CountRows()}); err == nil {
		t.Fatal("grouping by a json column must be refused (GROUP BY needs equality)")
	}
	if _, err := GroupBy[string](ctx, s, "id", []Aggregate{CountDistinctOf("payload")}); err == nil {
		t.Fatal("CountDistinctOf over a json column must be refused")
	}
}

// TestAggregate_Round2SQLiteNumericEpochNotJulian is the round-2 review
// #2 regression: the previous 'auto' modifier read numeric values in the
// Julian-day range as Julian days, silently shifting Unix seconds from
// the first 63 days of 1970 (2440588 = 1970-01-29T05:56:28Z came back as
// 1970-01-01T12:00:00Z). Numeric storage now always reads as Unix
// seconds via an explicit typeof branch, text as a plain timestamp.
func TestAggregate_Round2SQLiteNumericEpochNotJulian(t *testing.T) {
	if dbtest.Driver() != "sqlite" {
		t.Skip("dynamic-typed integer time storage is a SQLite-only shape")
	}
	s := setupAggStore(t)
	seedAggSales(t, s)
	ctx := context.Background()

	// A legacy integer-epoch row next to the seeded text rows: both
	// typeof branches must land in one consistent UTC timeline.
	gdb, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec("UPDATE agg_sales SET at = 2440588 WHERE status = 'draft'").Error; err != nil {
		t.Fatal(err)
	}
	want := time.Date(1970, 1, 29, 5, 56, 28, 0, time.UTC)
	lo, ok, err := Min[time.Time](ctx, s, "at")
	if err != nil || !ok || !lo.Equal(want) {
		t.Fatalf("Min over an early-epoch integer = %v, %v, %v; want %v (not a Julian-day misread)", lo, ok, err, want)
	}
	hi, ok, err := Max[time.Time](ctx, s, "at")
	if err != nil || !ok || !hi.Equal(aggT0.Add(time.Hour)) {
		t.Fatalf("Max across mixed text/integer storage = %v, %v, %v; want the text row t0+1h", hi, ok, err)
	}
	if n, err := CountDistinct(ctx, s, "at"); err != nil || n != 3 {
		t.Fatalf("CountDistinct across mixed storage = %d, %v; want 3", n, err)
	}
}

// aggJSONText is a custom Go string type whose database column is JSON
// on every dialect via GormDBDataTypeInterface — the round-3 review #2
// bypass: schema.Field.DataType stays the kind-derived "string", so a
// gate reading only DataType lets it through.
type aggJSONText string

func (aggJSONText) GormDBDataType(*gorm.DB, *schema.Field) string { return "JSON" }

type aggJSONNote struct {
	db.Model
	Body aggJSONText `json:"body"`
}

func (aggJSONNote) RIDPrefix() string { return "ajn" }

// TestAggregate_Round3GormDBDataTypeJSONRejected is the round-3 review
// #2 regression: the JSON gate read the logical schema DataType only,
// while GORM's own migrator resolves the REAL dialect column type
// through GormDBDataTypeInterface — a custom string type mapping to a
// JSON column sailed through and PostgreSQL failed mid-query. The gate
// now consults the migrator's resolved type as well.
func TestAggregate_Round3GormDBDataTypeJSONRejected(t *testing.T) {
	gdb := setupDB(t)
	ctx := context.Background()
	if err := gdb.Migrate(ctx, db.Table(&aggJSONNote{})); err != nil {
		t.Fatal(err)
	}
	s := New[aggJSONNote](gdb, log.Empty(), WithQueryFields("id", "body"))

	_, err := CountDistinct(ctx, s, "body")
	if err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("CountDistinct over a GormDBDataType JSON column must be refused, got %v", err)
	}
	if _, err := GroupBy[string](ctx, s, "body", []Aggregate{CountRows()}); err == nil {
		t.Fatal("grouping by a GormDBDataType JSON column must be refused")
	}
	if _, err := GroupBy[string](ctx, s, "id", []Aggregate{CountDistinctOf("body")}); err == nil {
		t.Fatal("CountDistinctOf over a GormDBDataType JSON column must be refused")
	}
}

// aggMistyped declares a numeric Go field over a text column — legal in
// GORM, lethal to aggregation: the database computes under the COLUMN's
// semantics, so SQLite would answer Min with the lexicographic extreme
// (10 beats 2) and PostgreSQL would refuse SUM(text) mid-query.
type aggMistyped struct {
	db.Model
	Qty int64 `json:"qty" gorm:"type:text"`
}

func (aggMistyped) RIDPrefix() string { return "amt" }

// TestAggregate_Round4TextBackedIntRejected is the round-4 review #2
// regression: the capability matrix now has two halves — the wire kind
// governs Go result convergence, and the dialect column type decides
// whether the database operation is legal. A mismatch fails closed at
// entry instead of silently computing under text semantics.
func TestAggregate_Round4TextBackedIntRejected(t *testing.T) {
	gdb := setupDB(t)
	ctx := context.Background()
	if err := gdb.Migrate(ctx, db.Table(&aggMistyped{})); err != nil {
		t.Fatal(err)
	}
	s := New[aggMistyped](gdb, log.Empty(), WithQueryFields("id", "qty"))
	// No rows are seeded: the gate fires before any query, and the
	// mismatch is unseedable on the strictest dialect anyway — pgx
	// refuses to encode an int64 into a text column at INSERT time,
	// which is the same class of wrongness the gate catches for the
	// dialects that would happily store and then mis-compare it.
	_, _, err := Min[int64](ctx, s, "qty")
	if err == nil || !strings.Contains(err.Error(), "column type") {
		t.Fatalf("Min over a text-backed int field must fail closed naming the column type, got %v", err)
	}
	if errIs400(err) {
		t.Fatal("the rejection is a server-side error, not a client 400")
	}
	if _, _, err := Sum[int64](ctx, s, "qty"); err == nil {
		t.Fatal("Sum over a text-backed int field must fail closed")
	}
	if _, err := GroupBy[int64](ctx, s, "qty", []Aggregate{CountRows()}); err == nil {
		t.Fatal("grouping by a text-backed int field must fail closed")
	}
	if _, err := CountDistinct(ctx, s, "qty"); err == nil {
		t.Fatal("CountDistinct over a text-backed int field must fail closed")
	}
}

// TestAggregate_Round5CatalogWhitelistRejectsExotics is the round-5
// review #2 regression: the type gate was substring matching, which
// swept PostgreSQL's exotic neighbours into the wrong family — "interval"
// and "int4range" contain "int", "daterange" starts with "date",
// "time"/"timetz" contain "time", "integer[]" (an array) contains "int".
// The exact per-dialect whitelist must admit none of them, under ANY
// wire kind, while still admitting the legitimate types.
func TestAggregate_Round5CatalogWhitelistRejectsExotics(t *testing.T) {
	exotic := []string{"interval", "int4range", "int8range", "daterange", "tsrange", "time", "timetz", "integer[]", "bigint[]", "json", "jsonb", "bytea", "point"}
	kinds := []string{cursorKindInt, cursorKindUint, cursorKindFloat, cursorKindTime, cursorKindString, cursorKindBool}
	for _, dialect := range []string{"sqlite", "postgres", "mysql"} {
		for _, typ := range exotic {
			for _, kind := range kinds {
				if aggCatalogAllows(dialect, kind, typ) {
					t.Errorf("aggCatalogAllows(%q, %q, %q) = true; exotic/incomparable types must fail closed", dialect, kind, typ)
				}
			}
		}
	}
	// The legitimate mappings each dialect actually reports must pass —
	// a whitelist that rejects everything is useless.
	good := []struct{ dialect, kind, typ string }{
		{"postgres", cursorKindInt, "int8"}, {"postgres", cursorKindFloat, "numeric"},
		{"postgres", cursorKindTime, "timestamptz"}, {"postgres", cursorKindString, "varchar"},
		{"postgres", cursorKindBool, "bool"},
		{"mysql", cursorKindInt, "bigint"}, {"mysql", cursorKindFloat, "double"},
		{"mysql", cursorKindTime, "datetime"}, {"mysql", cursorKindBool, "tinyint"},
		{"sqlite", cursorKindInt, "integer"}, {"sqlite", cursorKindFloat, "real"},
		{"sqlite", cursorKindTime, "datetime"}, {"sqlite", cursorKindString, "text"},
		{"sqlite", cursorKindBool, "numeric"},
		// round-6 review #2: char/nchar (SQLite) and enum (MySQL) are
		// legitimate string types the whitelist must admit.
		{"sqlite", cursorKindString, "char"}, {"sqlite", cursorKindString, "nchar"},
		{"mysql", cursorKindString, "enum"}, {"mysql", cursorKindString, "char"},
	}
	for _, g := range good {
		if !aggCatalogAllows(g.dialect, g.kind, g.typ) {
			t.Errorf("aggCatalogAllows(%q, %q, %q) = false; a real blessed mapping must pass", g.dialect, g.kind, g.typ)
		}
	}
	// Unknown dialect fails closed.
	if aggCatalogAllows("cockroach", cursorKindInt, "int8") {
		t.Error("unknown dialect must fail closed")
	}
}

// f1TextInt reproduces the round-5 review #1 shape: a Go int64 field with
// no type tag, whose actual column was created as TEXT by an out-of-band
// (versioned/off) migration. The model-rendered type says integer; only
// the real catalog says text.
type f1TextInt struct {
	db.Model
	Qty int64 `json:"qty"`
}

func (f1TextInt) RIDPrefix() string { return "f1t" }

// TestAggregate_Round5Finding1RealCatalogType is the round-5 review #1
// regression: the gate cached FullDataTypeOf, which only renders the
// type the MODEL would create — not the real column. Under
// migrate:versioned/off the column can genuinely be TEXT while the model
// says int64, and MIN then ran lexicographically (2 lost to 10),
// silently wrong. The gate now reads the real catalog type and fails
// closed on the mismatch.
func TestAggregate_Round5Finding1RealCatalogType(t *testing.T) {
	if dbtest.Driver() != "sqlite" {
		t.Skip("the raw-DDL versioned/off shape is scripted for SQLite")
	}
	gdb := setupDB(t)
	ctx := context.Background()
	raw := gdb.Unsafe(ctx)
	// The table the store's model maps to, but with qty as real TEXT.
	if err := raw.Exec(`CREATE TABLE f1_text_ints (id integer PRIMARY KEY AUTOINCREMENT, rid text, version integer, created_at datetime, updated_at datetime, qty TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	s := New[f1TextInt](gdb, log.Empty(), WithQueryFields("id", "qty"))
	for _, q := range []string{"2", "10"} {
		if err := raw.Exec(`INSERT INTO f1_text_ints (rid, version, qty) VALUES (?, 1, ?)`, "f1t_"+q, q).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Before the fix this returned 10, true, nil (lexicographic MIN over
	// text). Now the real catalog type (TEXT) contradicts the int64 wire
	// kind and the gate fails closed.
	_, _, err := Min[int64](ctx, s, "qty")
	if err == nil || !strings.Contains(err.Error(), "column type") {
		t.Fatalf("Min[int64] over a real TEXT column must fail closed, got %v", err)
	}
	if errIs400(err) {
		t.Fatal("the rejection is a server-side error, not a client 400")
	}
}

// TestAggregate_Round5PGExoticColumnsRejected is the round-5 review #2
// regression end-to-end on a real PostgreSQL catalog: a column whose
// real type is interval / time-of-day / a range is rejected, proving the
// gate reads and matches the actual catalog type (not the model's).
func TestAggregate_Round5PGExoticColumnsRejected(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("interval/range/time-of-day catalog types are exercised on the PG lane")
	}
	gdb := setupDB(t)
	ctx := context.Background()
	raw := gdb.Unsafe(ctx)
	// int8-backed model columns whose ACTUAL catalog type is exotic: the
	// model says int64/time, the catalog says interval/time/int4range.
	if err := raw.Exec(`CREATE TABLE exotic_rows (
		id bigserial PRIMARY KEY, rid varchar(24), version bigint,
		created_at timestamptz, updated_at timestamptz,
		span interval, tod time, rng int4range)`).Error; err != nil {
		t.Fatal(err)
	}
	type exoticRow struct {
		db.Model
		Span int64     `json:"span" gorm:"column:span"`
		Tod  time.Time `json:"tod" gorm:"column:tod"`
		Rng  int64     `json:"rng" gorm:"column:rng"`
	}
	s := New[exoticRow](gdb, log.Empty(), WithQueryFields("id", "span", "tod", "rng"))
	for _, field := range []string{"span", "tod", "rng"} {
		if _, _, err := Sum[int64](ctx, s, field); err == nil {
			// tod is time-kind so Sum rejects it on kind grounds anyway;
			// use Min for a kind-compatible probe below.
			if field != "tod" {
				t.Fatalf("Sum over exotic column %q must fail closed", field)
			}
		}
	}
	if _, _, err := Min[time.Time](ctx, s, "tod"); err == nil || !strings.Contains(err.Error(), "column type") {
		t.Fatalf("Min[time.Time] over a PG `time` (time-of-day) column must fail closed, got %v", err)
	}
	if _, _, err := Min[int64](ctx, s, "span"); err == nil || !strings.Contains(err.Error(), "column type") {
		t.Fatalf("Min[int64] over a PG `interval` column must fail closed, got %v", err)
	}
	if _, err := GroupBy[int64](ctx, s, "rng", []Aggregate{CountRows()}); err == nil {
		t.Fatal("grouping by a PG `int4range` column must fail closed")
	}
}

// r6UpperCol reproduces round-6 review #1: a Go int64 field mapping to
// column qty, over a table whose real column was declared QTY (uppercase)
// by an out-of-band migration. SQLite identifiers are case-insensitive,
// so the query works — but the catalog gate keyed the type map by the
// raw catalog name.
type r6UpperCol struct {
	db.Model
	Qty int64 `json:"qty"`
}

func (r6UpperCol) RIDPrefix() string { return "r6u" }

// TestAggregate_Round6CaseInsensitiveColumnName is the round-6 review #1
// regression: the catalog key was the raw column name while the lookup
// used the lowercase model DBName, so a case-insensitive dialect's
// upper/mixed-case column resolved to an empty type and was falsely
// rejected. Catalog keys and lookups now fold case on SQLite/MySQL.
func TestAggregate_Round6CaseInsensitiveColumnName(t *testing.T) {
	if dbtest.Driver() != "sqlite" {
		t.Skip("the raw uppercase-column shape is scripted for SQLite")
	}
	gdb := setupDB(t)
	ctx := context.Background()
	raw := gdb.Unsafe(ctx)
	if err := raw.Exec(`CREATE TABLE r6_upper_cols (id integer PRIMARY KEY AUTOINCREMENT, rid text, version integer, created_at datetime, updated_at datetime, QTY INTEGER)`).Error; err != nil {
		t.Fatal(err)
	}
	s := New[r6UpperCol](gdb, log.Empty(), WithQueryFields("id", "qty"))
	if err := raw.Exec(`INSERT INTO r6_upper_cols (rid, version, QTY) VALUES ('r6u_a', 1, 2), ('r6u_b', 1, 10)`).Error; err != nil {
		t.Fatal(err)
	}
	// The column really is INTEGER — the aggregate must compute correctly,
	// not fail closed on a case-mismatched catalog key.
	lo, ok, err := Min[int64](ctx, s, "qty")
	if err != nil || !ok || lo != 2 {
		t.Fatalf("Min over an uppercase-named INTEGER column = %d, %v, %v; want 2 (case-insensitive catalog match)", lo, ok, err)
	}
	sum, ok, err := Sum[int64](ctx, s, "qty")
	if err != nil || !ok || sum != 12 {
		t.Fatalf("Sum over an uppercase-named INTEGER column = %d, %v, %v; want 12", sum, ok, err)
	}
}

// r6CharModel exercises round-6 review #2: a string column whose real
// catalog type is CHAR — a TEXT-affinity type the exact whitelist must
// admit for GroupBy / CountDistinct.
type r6CharModel struct {
	db.Model
	Code string `json:"code" gorm:"type:char(8)"`
}

func (r6CharModel) RIDPrefix() string { return "r6h" }

// TestAggregate_Round6SQLiteCharColumn is the round-6 review #2
// regression: the SQLite string whitelist dropped char/nchar (it had
// character/nvarchar), so a CHAR column — plain TEXT affinity, perfectly
// groupable — was falsely rejected.
func TestAggregate_Round6SQLiteCharColumn(t *testing.T) {
	if dbtest.Driver() != "sqlite" {
		t.Skip("CHAR affinity behaviour is pinned on the SQLite lane")
	}
	gdb := setupDB(t)
	ctx := context.Background()
	if err := gdb.Migrate(ctx, db.Table(&r6CharModel{})); err != nil {
		t.Fatal(err)
	}
	s := New[r6CharModel](gdb, log.Empty(), WithQueryFields("id", "code"))
	for _, code := range []string{"a", "a", "b"} {
		if err := s.Create(ctx, &r6CharModel{Code: code}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := CountDistinct(ctx, s, "code"); err != nil || n != 2 {
		t.Fatalf("CountDistinct over a CHAR column = %d, %v; want 2", n, err)
	}
	groups, err := GroupBy[string](ctx, s, "code", []Aggregate{CountRows()})
	if err != nil || len(groups) != 2 {
		t.Fatalf("GroupBy over a CHAR column = %v, %v; want 2 groups", groups, err)
	}
}

// r7Kelvin exercises round-7 review #1: a Go int64 field on column k,
// over a table that also has a DISTINCT column named with the Kelvin
// sign (U+212A), which strings.ToLower folds to ASCII 'k'.
type r7Kelvin struct {
	db.Model
	K int64 `json:"k" gorm:"column:k"`
}

func (r7Kelvin) RIDPrefix() string { return "r7k" }

// TestAggregate_Round7UnicodeFoldCollision is the round-7 review #1
// regression: the catalog key folded with full Unicode case rules, so
// the Kelvin sign U+212A (a real, DISTINCT SQLite column) collapsed onto
// ASCII 'k' and overwrote the real k → TEXT entry with INTEGER. The gate
// then mistyped the TEXT column as integer and MIN ran lexicographically
// (2 lost to 10). ASCII-only folding keeps the two columns distinct.
func TestAggregate_Round7UnicodeFoldCollision(t *testing.T) {
	if dbtest.Driver() != "sqlite" {
		t.Skip("the distinct-Unicode-column shape is scripted for SQLite")
	}
	gdb := setupDB(t)
	ctx := context.Background()
	raw := gdb.Unsafe(ctx)
	// ASCII k is TEXT; the Kelvin-sign column (U+212A) is INTEGER and is
	// declared LAST, so a colliding fold would leave map["k"] = integer.
	ddl := "CREATE TABLE r7_kelvins (id integer PRIMARY KEY AUTOINCREMENT, rid text, version integer, created_at datetime, updated_at datetime, k TEXT, \"K\" INTEGER)"
	if err := raw.Exec(ddl).Error; err != nil {
		t.Fatal(err)
	}
	s := New[r7Kelvin](gdb, log.Empty(), WithQueryFields("id", "k"))
	if err := raw.Exec("INSERT INTO r7_kelvins (rid, version, k) VALUES ('r7k_a',1,'2'),('r7k_b',1,'10')").Error; err != nil {
		t.Fatal(err)
	}
	// The real k column is TEXT; the int64 wire kind must fail closed,
	// not compute a lexicographic MIN over text.
	_, _, err := Min[int64](ctx, s, "k")
	if err == nil || !strings.Contains(err.Error(), "column type") {
		t.Fatalf("Min[int64] over a real TEXT column (Unicode-distinct sibling) must fail closed, got %v", err)
	}
}

// TestAggregate_Round7ScopeBeforeCatalog is the round-7 review #2
// regression: resolveAggCatalog ran a real catalog read (a sqlite_master
// query plus SELECT ... LIMIT 1) BEFORE the fail-closed scope, so an
// unauthenticated caller touched the database before being rejected. The
// scope is now applied first, in memory, so a rejected caller issues no
// DB operations at all.
func TestAggregate_Round7ScopeBeforeCatalog(t *testing.T) {
	s := setupProductStore(t) // OwnedModel → fail-closed OwnerScope; fresh, no aggregate cached
	root := s.h.Unsafe(context.Background())
	var dbOps atomic.Int64
	count := func(*gorm.DB) { dbOps.Add(1) }
	// The catalog metadata read and the aggregate itself ride the Raw and
	// Row callback chains; count all read paths so nothing slips through.
	if err := root.Callback().Raw().Register("r7:raw", count); err != nil {
		t.Fatal(err)
	}
	if err := root.Callback().Row().Register("r7:row", count); err != nil {
		t.Fatal(err)
	}
	if err := root.Callback().Query().Register("r7:query", count); err != nil {
		t.Fatal(err)
	}

	_, err := CountDistinct(context.Background(), s, "name")
	if !errors.Is(err, apierr.ErrUnauthenticated) {
		t.Fatalf("unauthenticated aggregate must fail closed with ErrUnauthenticated, got %v", err)
	}
	if n := dbOps.Load(); n != 0 {
		t.Fatalf("unauthenticated aggregate issued %d DB operations before failing closed; the scope must gate before the catalog read", n)
	}

	// Sanity: an authenticated caller does reach the database (proves the
	// counter is wired to the paths the aggregate actually uses).
	if _, err := CountDistinct(userCtx("u1"), s, "name"); err != nil {
		t.Fatal(err)
	}
	if dbOps.Load() == 0 {
		t.Fatal("authenticated aggregate issued no DB operations; the counter is not observing the read path")
	}
}

// TestAggregate_Round8CatalogNeverSamplesDataRows is the round-8 review
// regression: the catalog resolver used GORM's Migrator.ColumnTypes,
// whose implementation samples the DATA table (SELECT * FROM <table>
// LIMIT 1) without the store's scopes — the first authenticated
// aggregate on an owned model read one arbitrary tenant's row to sniff
// column types. Types now come from pure catalog metadata
// (pragma_table_info / pg_catalog / information_schema), so no
// statement may touch
// the data table unless it carries the scope predicate. The assertion is
// on SQL SHAPE, not operation counts — a count can't tell a scoped read
// from an unscoped one.
func TestAggregate_Round8CatalogNeverSamplesDataRows(t *testing.T) {
	s := setupProductStore(t) // fresh OwnedModel store: catalog not yet resolved
	seedCtx := userCtx("usr_a")
	if err := s.Create(seedCtx, &Product{Name: "w"}); err != nil {
		t.Fatal(err)
	}

	root := s.h.Unsafe(context.Background())
	var mu sync.Mutex
	var stmts []string
	capture := func(tx *gorm.DB) {
		mu.Lock()
		defer mu.Unlock()
		stmts = append(stmts, tx.Statement.SQL.String())
	}
	for name, reg := range map[string]func(string, func(*gorm.DB)) error{
		"raw":   root.Callback().Raw().Register,
		"row":   root.Callback().Row().Register,
		"query": root.Callback().Query().Register,
	} {
		if err := reg("r8:"+name, capture); err != nil {
			t.Fatal(err)
		}
	}

	// First authenticated aggregate: resolves the catalog AND runs the
	// real query. Every captured statement that references the data table
	// must carry the owner-scope predicate; catalog metadata queries
	// reference pragma/information_schema instead.
	if _, err := CountDistinct(userCtx("usr_a"), s, "name"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stmts) == 0 {
		t.Fatal("no SQL captured; the assertion is not observing the read path")
	}
	sawScopedRead := false
	for _, sql := range stmts {
		if !strings.Contains(sql, "products") {
			continue // catalog metadata query — never touches the data table
		}
		if !strings.Contains(sql, "owner_id") {
			t.Fatalf("statement touches the data table without the owner scope: %s", sql)
		}
		sawScopedRead = true
	}
	if !sawScopedRead {
		t.Fatal("no scoped data-table read captured; the assertion is not observing the aggregate query")
	}
}

// TestAggregate_Round8CatalogMissTableNotPoisoned pins the retry
// semantics the metadata reader must keep: aggregating before the table
// exists errors loudly (zero catalog columns is an error, not a cached
// empty map), and the SAME store recovers once the table is created.
func TestAggregate_Round8CatalogMissTableNotPoisoned(t *testing.T) {
	if dbtest.Driver() != "sqlite" {
		t.Skip("scripted table lifecycle is exercised on the SQLite lane")
	}
	gdb := setupDB(t)
	ctx := context.Background()
	s := New[f1TextInt](gdb, log.Empty(), WithQueryFields("id", "qty")) // table not created
	if _, _, err := Sum[int64](ctx, s, "qty"); err == nil {
		t.Fatal("aggregating before the table exists must error")
	}
	if err := gdb.Unsafe(ctx).Exec(`CREATE TABLE f1_text_ints (id integer PRIMARY KEY AUTOINCREMENT, rid text, version integer, created_at datetime, updated_at datetime, qty INTEGER)`).Error; err != nil {
		t.Fatal(err)
	}
	// The earlier failure must not have cached an empty catalog.
	if _, ok, err := Sum[int64](ctx, s, "qty"); err != nil || ok {
		t.Fatalf("post-migration aggregate on the same store = ok=%v err=%v; want a clean empty-table NULL", ok, err)
	}
}

// --- round-9 review regressions: the catalog reader must resolve the
// table name the way the DATA queries resolve it — #1 a dot-qualified
// TableName splits into qualifier + bare name per dialect (GORM's
// quoter renders main.t as two identifiers; matching the whole string
// against bare catalog table names reported a migrated table as having
// no columns), #2 unqualified PostgreSQL names resolve through the
// WHOLE search_path, not just current_schema()'s head. ----------------

type round9QualifiedAgg struct {
	db.Model
	Qty int64 `json:"qty"`
}

func (round9QualifiedAgg) RIDPrefix() string { return "r9q" }
func (round9QualifiedAgg) TableName() string { return "main.round9_qualified_aggs" }

func TestAggregate_Round9QualifiedTableSQLite(t *testing.T) {
	if dbtest.Driver() != "sqlite" {
		t.Skip("the attached-database qualifier main is a SQLite shape")
	}
	gdb := setupDB(t)
	ctx := context.Background()
	if err := gdb.Migrate(ctx, db.Table(&round9QualifiedAgg{})); err != nil {
		t.Fatal(err)
	}
	s := New[round9QualifiedAgg](gdb, log.Empty(), WithQueryFields("id", "qty"))
	for _, qty := range []int64{3, 4} {
		if err := s.Create(ctx, &round9QualifiedAgg{Qty: qty}); err != nil {
			t.Fatal(err)
		}
	}
	total, ok, err := Sum[int64](ctx, s, "qty")
	if err != nil || !ok || total != 7 {
		t.Fatalf("Sum over a main-qualified store = %d ok=%v err=%v; want 7 true <nil>", total, ok, err)
	}
}

// round9PGSchema carries the per-run schema name into the model's
// TableName; written once by the test before any schema parse reads it.
var round9PGSchema string

type round9PGQualifiedAgg struct {
	db.Model
	Qty int64 `json:"qty"`
}

func (round9PGQualifiedAgg) RIDPrefix() string { return "r9p" }
func (round9PGQualifiedAgg) TableName() string {
	return round9PGSchema + ".round9_pg_qualified_aggs"
}

func TestAggregate_Round9QualifiedTablePG(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("schema-qualified catalog resolution is exercised on the PG lane")
	}
	gdb := dbtest.Open(t)
	ctx := context.Background()
	// A schema OUTSIDE the lane's pinned search_path: qualified names
	// must resolve to their own schema, independent of the path.
	round9PGSchema = fmt.Sprintf("round9q_%d_%d", os.Getpid(), time.Now().UnixNano())
	sch := round9PGSchema
	if err := gdb.Unsafe(ctx).Exec(fmt.Sprintf("CREATE SCHEMA %q", sch)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = gdb.Unsafe(context.Background()).Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE", sch)).Error
	})
	if err := gdb.Migrate(ctx, db.Table(&round9PGQualifiedAgg{})); err != nil {
		t.Fatal(err)
	}
	s := New[round9PGQualifiedAgg](gdb, log.Empty(), WithQueryFields("id", "qty"))
	for _, qty := range []int64{3, 4} {
		if err := s.Create(ctx, &round9PGQualifiedAgg{Qty: qty}); err != nil {
			t.Fatal(err)
		}
	}
	total, ok, err := Sum[int64](ctx, s, "qty")
	if err != nil || !ok || total != 7 {
		t.Fatalf("Sum over a schema-qualified store = %d ok=%v err=%v; want 7 true <nil>", total, ok, err)
	}
}

type round9SearchPathAgg struct {
	db.Model
	Qty int64 `json:"qty"`
}

func (round9SearchPathAgg) RIDPrefix() string { return "r9s" }
func (round9SearchPathAgg) TableName() string { return "round9_search_path_aggs" }

func TestAggregate_Round9PGSearchPathResolution(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("search_path resolution is exercised on the PG lane")
	}
	gdb := dbtest.Open(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	first, second := "round9_sp_first_"+suffix, "round9_sp_second_"+suffix
	for _, sch := range []string{first, second} {
		if err := gdb.Unsafe(ctx).Exec(fmt.Sprintf("CREATE SCHEMA %q", sch)).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, sch := range []string{first, second} {
			_ = gdb.Unsafe(context.Background()).Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE", sch)).Error
		}
	})
	// The table exists ONLY in the second schema, so the path's HEAD has
	// nothing to report — the shape current_schema() misread as "not
	// migrated" while unqualified data queries kept resolving fine.
	if err := gdb.Unsafe(ctx).Exec(fmt.Sprintf(
		`CREATE TABLE %q.round9_search_path_aggs (id bigserial PRIMARY KEY, rid varchar(24) NOT NULL, version bigint NOT NULL DEFAULT 1, created_at timestamptz, updated_at timestamptz, qty bigint NOT NULL)`,
		second)).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Unsafe(ctx).Exec(fmt.Sprintf(
		`INSERT INTO %q.round9_search_path_aggs (rid, qty) VALUES ('r9s_sp_row_seed_0001', 7)`,
		second)).Error; err != nil {
		t.Fatal(err)
	}
	s := New[round9SearchPathAgg](gdb, log.Empty(), WithQueryFields("id", "qty"))
	if err := db.RunInTx(ctx, gdb, func(txCtx context.Context) error {
		// SET LOCAL pins this transaction's connection to a two-schema
		// path; both the aggregate and its catalog read ride the same
		// connection via txCtx.
		if err := gdb.Unsafe(txCtx).Exec(fmt.Sprintf("SET LOCAL search_path = %q, %q", first, second)).Error; err != nil {
			return err
		}
		total, ok, err := Sum[int64](txCtx, s, "qty")
		if err != nil {
			return err
		}
		if !ok || total != 7 {
			t.Fatalf("Sum through the search_path = %d ok=%v; want 7 true", total, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// --- round-10 review regressions: the PG catalog read must bind to the
// REAL type and the REAL relation — #1 a user domain's bare typname can
// masquerade as a built-in (CREATE DOMAIN app."int8" AS text), so types
// resolve through typbasetype to a pg_catalog-namespace base; #2 an
// unqualified table resolves per-transaction under SET LOCAL
// search_path, so the catalog cache keys by resolved relation OID
// instead of holding one map per Store. -------------------------------

type round10DomainAgg struct {
	db.Model
	Qty  int64 `json:"qty"`
	Qty2 int64 `json:"qty2"`
}

func (round10DomainAgg) RIDPrefix() string { return "r10d" }
func (round10DomainAgg) TableName() string { return "round10_domain_aggs" }

func TestAggregate_Round10PGDomainShadowsBuiltinType(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("user-defined domains are a PostgreSQL shape")
	}
	gdb := dbtest.Open(t)
	ctx := context.Background()
	var sch string
	if err := gdb.Unsafe(ctx).Raw("SELECT current_schema()").Scan(&sch).Error; err != nil {
		t.Fatal(err)
	}
	// qty's domain steals the built-in bigint's typname but is REALLY
	// text; qty2's domain is honestly over bigint. Only explicit
	// qualification reaches the shadow domain (pg_catalog is implicitly
	// first for unqualified type names).
	for _, ddl := range []string{
		fmt.Sprintf(`CREATE DOMAIN %q."int8" AS text`, sch),
		fmt.Sprintf(`CREATE DOMAIN %q.round10_money AS bigint`, sch),
		fmt.Sprintf(`CREATE TABLE round10_domain_aggs (id bigserial PRIMARY KEY, rid varchar(24) NOT NULL, version bigint NOT NULL DEFAULT 1, created_at timestamptz, updated_at timestamptz, qty %q."int8" NOT NULL, qty2 %q.round10_money NOT NULL)`, sch, sch),
		`INSERT INTO round10_domain_aggs (rid, qty, qty2) VALUES ('r10d_row_seed_00001', '2', 2), ('r10d_row_seed_00002', '10', 10)`,
	} {
		if err := gdb.Unsafe(ctx).Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := New[round10DomainAgg](gdb, log.Empty(), WithQueryFields("id", "qty", "qty2"))
	// The shadow domain must gate as its base type text and fail closed —
	// by bare typname it passed as an integer and MIN came back
	// lexicographic (10 beats 2), silently.
	if _, _, err := Min[int64](ctx, s, "qty"); err == nil || !strings.Contains(err.Error(), `"text"`) {
		t.Fatalf("Min over a text-based domain named int8 = %v; want the fail-closed catalog error naming text", err)
	}
	// Round-8 semantics kept: a domain honestly over bigint aggregates
	// (udt_name resolved domains to their base before round-9 too).
	total, ok, err := Sum[int64](ctx, s, "qty2")
	if err != nil || !ok || total != 12 {
		t.Fatalf("Sum over a bigint-based domain = %d ok=%v err=%v; want 12 true <nil>", total, ok, err)
	}
}

type round10SPCacheAgg struct {
	db.Model
	Qty int64 `json:"qty"`
}

func (round10SPCacheAgg) RIDPrefix() string { return "r10c" }
func (round10SPCacheAgg) TableName() string { return "round10_sp_cache_aggs" }

func TestAggregate_Round10PGDynamicSearchPathCache(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("SET LOCAL search_path is a PostgreSQL shape")
	}
	gdb := dbtest.Open(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	numSch, textSch := "round10_num_"+suffix, "round10_text_"+suffix
	for _, sch := range []string{numSch, textSch} {
		if err := gdb.Unsafe(ctx).Exec(fmt.Sprintf("CREATE SCHEMA %q", sch)).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, sch := range []string{numSch, textSch} {
			_ = gdb.Unsafe(context.Background()).Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE", sch)).Error
		}
	})
	// Same table name in both schemas — qty bigint in one, text in the
	// other; values chosen so a lexicographic MIN differs from the
	// numeric one.
	for _, ddl := range []string{
		fmt.Sprintf(`CREATE TABLE %q.round10_sp_cache_aggs (id bigserial PRIMARY KEY, rid varchar(24) NOT NULL, version bigint NOT NULL DEFAULT 1, created_at timestamptz, updated_at timestamptz, qty bigint NOT NULL)`, numSch),
		fmt.Sprintf(`INSERT INTO %q.round10_sp_cache_aggs (rid, qty) VALUES ('r10c_num_seed_00001', 2), ('r10c_num_seed_00002', 10)`, numSch),
		fmt.Sprintf(`CREATE TABLE %q.round10_sp_cache_aggs (id bigserial PRIMARY KEY, rid varchar(24) NOT NULL, version bigint NOT NULL DEFAULT 1, created_at timestamptz, updated_at timestamptz, qty text NOT NULL)`, textSch),
		fmt.Sprintf(`INSERT INTO %q.round10_sp_cache_aggs (rid, qty) VALUES ('r10c_txt_seed_00001', '2'), ('r10c_txt_seed_00002', '10')`, textSch),
	} {
		if err := gdb.Unsafe(ctx).Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := New[round10SPCacheAgg](gdb, log.Empty(), WithQueryFields("id", "qty"))
	minOn := func(sch string) (int64, error) {
		var v int64
		err := db.RunInTx(ctx, gdb, func(txCtx context.Context) error {
			if err := gdb.Unsafe(txCtx).Exec(fmt.Sprintf("SET LOCAL search_path = %q", sch)).Error; err != nil {
				return err
			}
			got, ok, err := Min[int64](txCtx, s, "qty")
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("Min reported no rows")
			}
			v = got
			return nil
		})
		return v, err
	}
	// tx1 resolves the bigint relation and caches ITS column types.
	if v, err := minOn(numSch); err != nil || v != 2 {
		t.Fatalf("Min over the bigint relation = %d err=%v; want 2 <nil>", v, err)
	}
	// tx2 resolves the TEXT relation: the gate must re-resolve for this
	// relation and fail closed — reusing the bigint entry let the query
	// run and return the lexicographic MIN (10 beats 2), silently.
	if v, err := minOn(textSch); err == nil || !strings.Contains(err.Error(), `"text"`) {
		t.Fatalf("Min over the text relation = %d err=%v; want the fail-closed catalog error naming text", v, err)
	}
	// tx3 returns to the bigint relation: its entry is still cached and
	// still correct.
	if v, err := minOn(numSch); err != nil || v != 2 {
		t.Fatalf("Min back on the bigint relation = %d err=%v; want 2 <nil>", v, err)
	}
}

// --- round-11 review regressions: the catalog reader splits qualified
// table names the way each dialect's own quoter does (a dot inside a
// quoted identifier is data, and WHICH character quotes is
// dialect-specific), and the per-relation cache is a sync.Map so a
// schema-per-tenant fleet neither serialises nor re-copies on first
// touch. ---------------------------------------------------------------

// TestAggregate_Round11IdentifierSplitMatchesGORM pins the split
// against the REAL quoters of all three blessed dialects: a plain
// strings.Split on "." read `"a.b".t` as three parts on PostgreSQL (a
// working table became unaggregatable) while the same string really IS
// three parts on MySQL/SQLite, where the quote character is a backtick.
// The cases are the ones GORM's quoter was measured on.
func TestAggregate_Round11IdentifierSplitMatchesGORM(t *testing.T) {
	dialects := []struct {
		name  string
		quote byte
		d     gorm.Dialector
	}{
		{"postgres", '"', postgres.Dialector{Config: &postgres.Config{}}},
		{"mysql", '`', gormmysql.Dialector{Config: &gormmysql.Config{}}},
		{"sqlite", '`', sqlite.Dialector{}},
	}
	names := []string{
		"plain_table",
		"main.plain_table",
		`"round11.schema".quoted_dot_aggs`,
		`"Weird""Quote".t`,
		"`bt.schema`.t",
		"a.b.c",
		`sch."Tbl"`,
	}
	for _, d := range dialects {
		for _, name := range names {
			parts, ok := aggIdentifierParts(name, d.quote)
			if !ok {
				t.Errorf("%s: %q did not split", d.name, name)
				continue
			}
			var want strings.Builder
			d.d.QuoteTo(&want, name)
			if got := aggQuoteParts(parts, d.quote); got != want.String() {
				t.Errorf("%s: %q split into %q, which re-renders as %s; the driver renders %s",
					d.name, name, parts, got, want.String())
			}
		}
	}
	// The dialect-specific reading is the whole point: the same string is
	// two parts on PostgreSQL and three on MySQL/SQLite.
	if parts, _ := aggIdentifierParts(`"round11.schema".t`, '"'); len(parts) != 2 || parts[0] != "round11.schema" {
		t.Errorf(`postgres: "round11.schema".t split into %q; want ["round11.schema" "t"]`, parts)
	}
	if parts, _ := aggIdentifierParts(`"round11.schema".t`, '`'); len(parts) != 3 {
		t.Errorf(`mysql: "round11.schema".t split into %q; want three parts`, parts)
	}
	// An unterminated quoted run has no honest reading — fail closed.
	if _, ok := aggIdentifierParts(`"unterminated.t`, '"'); ok {
		t.Error(`postgres: "unterminated.t split without error; want the closed door`)
	}
}

// round11QuotedSchema carries the per-run schema name (which contains a
// DOT, so it only addresses correctly while quoted) into TableName.
var round11QuotedSchema string

type round11QuotedDotAgg struct {
	db.Model
	Qty int64 `json:"qty"`
}

func (round11QuotedDotAgg) RIDPrefix() string { return "r11q" }
func (round11QuotedDotAgg) TableName() string {
	return `"` + round11QuotedSchema + `".round11_quoted_dot_aggs`
}

func TestAggregate_Round11QuotedDotTableName(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("a quoted dot inside a schema name is exercised on the PG lane")
	}
	gdb := dbtest.Open(t)
	ctx := context.Background()
	// The schema name itself contains a dot: GORM quotes it, so migration
	// and Create work — the catalog reader must read it the same way
	// instead of seeing three identifier parts.
	round11QuotedSchema = fmt.Sprintf("round11.schema_%d_%d", os.Getpid(), time.Now().UnixNano())
	sch := round11QuotedSchema
	if err := gdb.Unsafe(ctx).Exec(fmt.Sprintf("CREATE SCHEMA %q", sch)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = gdb.Unsafe(context.Background()).Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE", sch)).Error
	})
	if err := gdb.Migrate(ctx, db.Table(&round11QuotedDotAgg{})); err != nil {
		t.Fatal(err)
	}
	s := New[round11QuotedDotAgg](gdb, log.Empty(), WithQueryFields("id", "qty"))
	for _, qty := range []int64{3, 4} {
		if err := s.Create(ctx, &round11QuotedDotAgg{Qty: qty}); err != nil {
			t.Fatal(err)
		}
	}
	total, ok, err := Sum[int64](ctx, s, "qty")
	if err != nil || !ok || total != 7 {
		t.Fatalf("Sum over a quoted-dot schema = %d ok=%v err=%v; want 7 true <nil>", total, ok, err)
	}
}

type round11TenantAgg struct {
	db.Model
	Qty int64 `json:"qty"`
}

func (round11TenantAgg) RIDPrefix() string { return "r11t" }
func (round11TenantAgg) TableName() string { return "round11_tenant_aggs" }

// TestAggregate_Round11ConcurrentTenantRelations exercises the
// per-relation cache the way a schema-per-tenant fleet does: many
// relations first-touched CONCURRENTLY through one Store. Under -race
// it pins that the sync.Map path is sound, and each tenant must get its
// OWN column types and its own answer.
func TestAggregate_Round11ConcurrentTenantRelations(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("per-transaction relation resolution is a PostgreSQL shape")
	}
	gdb := dbtest.Open(t)
	ctx := context.Background()
	const tenants = 8
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	schemas := make([]string, tenants)
	for i := range schemas {
		schemas[i] = fmt.Sprintf("round11_t%d_%s", i, suffix)
		if err := gdb.Unsafe(ctx).Exec(fmt.Sprintf("CREATE SCHEMA %q", schemas[i])).Error; err != nil {
			t.Fatal(err)
		}
		if err := gdb.Unsafe(ctx).Exec(fmt.Sprintf(
			`CREATE TABLE %q.round11_tenant_aggs (id bigserial PRIMARY KEY, rid varchar(24) NOT NULL, version bigint NOT NULL DEFAULT 1, created_at timestamptz, updated_at timestamptz, qty bigint NOT NULL)`,
			schemas[i])).Error; err != nil {
			t.Fatal(err)
		}
		// Tenant i sums to i+1, so a cross-tenant cache hit is visible.
		if err := gdb.Unsafe(ctx).Exec(fmt.Sprintf(
			`INSERT INTO %q.round11_tenant_aggs (rid, qty) VALUES ('r11t_seed_%08d', %d)`,
			schemas[i], i, i+1)).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, sch := range schemas {
			_ = gdb.Unsafe(context.Background()).Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE", sch)).Error
		}
	})

	s := New[round11TenantAgg](gdb, log.Empty(), WithQueryFields("id", "qty"))
	var wg sync.WaitGroup
	errs := make([]error, tenants)
	sums := make([]int64, tenants)
	for i := range schemas {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = db.RunInTx(ctx, gdb, func(txCtx context.Context) error {
				if err := gdb.Unsafe(txCtx).Exec(fmt.Sprintf("SET LOCAL search_path = %q", schemas[i])).Error; err != nil {
					return err
				}
				total, ok, err := Sum[int64](txCtx, s, "qty")
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("tenant %d: Sum reported no rows", i)
				}
				sums[i] = total
				return nil
			})
		}(i)
	}
	wg.Wait()
	for i := range schemas {
		if errs[i] != nil {
			t.Fatalf("tenant %d: %v", i, errs[i])
		}
		if want := int64(i + 1); sums[i] != want {
			t.Errorf("tenant %d summed to %d; want %d (a cross-tenant catalog cache hit?)", i, sums[i], want)
		}
	}
}

// round9MySQLDatabase carries the throwaway database name into the
// model's TableName; written once by the test before any schema parse
// reads it.
var round9MySQLDatabase string

type round9MySQLQualifiedAgg struct {
	db.Model
	Qty int64 `json:"qty"`
}

func (round9MySQLQualifiedAgg) RIDPrefix() string { return "r9m" }
func (round9MySQLQualifiedAgg) TableName() string {
	return round9MySQLDatabase + ".round9_mysql_qualified_aggs"
}

func TestAggregate_Round9MySQLQualifiedTable(t *testing.T) {
	gdb := dbtest.OpenMySQL(t)
	ctx := context.Background()
	if err := gdb.Unsafe(ctx).Raw("SELECT DATABASE()").Scan(&round9MySQLDatabase).Error; err != nil {
		t.Fatal(err)
	}
	if round9MySQLDatabase == "" {
		t.Fatal("SELECT DATABASE() came back empty")
	}
	// Real DDL rather than AutoMigrate: the catalog fix is about READING
	// an existing database-qualified table, not about migrating one.
	if err := gdb.Unsafe(ctx).Exec("CREATE TABLE `" + round9MySQLDatabase + "`.`round9_mysql_qualified_aggs` (id bigint unsigned AUTO_INCREMENT PRIMARY KEY, rid varchar(24) NOT NULL, version bigint NOT NULL DEFAULT 1, created_at datetime(3), updated_at datetime(3), qty bigint NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	s := New[round9MySQLQualifiedAgg](gdb, log.Empty(), WithQueryFields("id", "qty"))
	for _, qty := range []int64{3, 4} {
		if err := s.Create(ctx, &round9MySQLQualifiedAgg{Qty: qty}); err != nil {
			t.Fatal(err)
		}
	}
	total, ok, err := Sum[int64](ctx, s, "qty")
	if err != nil || !ok || total != 7 {
		t.Fatalf("Sum over a database-qualified MySQL store = %d ok=%v err=%v; want 7 true <nil>", total, ok, err)
	}
}

// TestAggregate_Round4MySQLConcurrentTimeAggregates is the round-4
// review #1 regression, still load-bearing after round-5 moved type
// resolution to the catalog: concurrent first-time aggregates must not
// race while the lazy catalog cache resolves. (Round-4's original bug
// was FullDataTypeOf mutating the shared *schema.Field on the request
// path; round-5 replaced that with a mutex-guarded, non-mutating
// Migrator.ColumnTypes read cached once per Store.) The store rides a
// SECOND handle that never migrated — the migrate:versioned/off
// production shape — so nothing pre-warms any cache.
func TestAggregate_Round4MySQLConcurrentTimeAggregates(t *testing.T) {
	migrated := setupAggStoreOn(t, dbtest.OpenMySQL(t))
	seedAggSales(t, migrated)

	cfg, err := gomysql.ParseDSN(os.Getenv(dbtest.MySQLDSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := db.Open(db.Options{Driver: "mysql", MySQL: db.MySQLOptions{
		Host: host, Port: port, Username: cfg.User, Password: cfg.Passwd,
		Database: mysqlCurrentDatabase(t, migrated),
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	s := New[AggSale](fresh, log.Empty(),
		WithQueryFields("id", "status", "qty", "price", "rating", "flag", "at", "meta", "created_at"))
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Go(func() {
			if _, _, err := Min[time.Time](ctx, s, "at"); err != nil {
				errs <- err
				return
			}
			if _, err := GroupBy[time.Time](ctx, s, "at", []Aggregate{CountRows()}); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestAggregate_MySQLTypeMapping pins the dialect with the most exotic
// aggregate wire types on a real server (make test-mysql lane): SUM over
// ints returns DECIMAL as []byte, AVG returns DECIMAL(x,4), MIN/MAX over
// DATETIME returns time.Time under parseTime — all of which must
// converge to the declared Go types exactly like SQLite and Postgres.
func TestAggregate_MySQLTypeMapping(t *testing.T) {
	s := setupAggStoreOn(t, dbtest.OpenMySQL(t))
	seedAggSales(t, s)
	ctx := context.Background()

	if sum, ok, err := Sum[int64](ctx, s, "qty"); err != nil || !ok || sum != 33 {
		t.Fatalf("MySQL Sum[int64] = %d, %v, %v; want 33", sum, ok, err)
	}
	if fsum, ok, err := Sum[float64](ctx, s, "price"); err != nil || !ok || math.Abs(fsum-4.0) > 1e-9 {
		t.Fatalf("MySQL Sum[float64] = %v, %v, %v; want 4.0", fsum, ok, err)
	}
	if avg, ok, err := Avg(ctx, s, "qty"); err != nil || !ok || math.Abs(avg-11.0) > 1e-9 {
		t.Fatalf("MySQL Avg = %v, %v, %v; want 11.0", avg, ok, err)
	}
	if at, ok, err := Max[time.Time](ctx, s, "at"); err != nil || !ok || !at.Equal(aggT0.Add(2*time.Hour)) {
		t.Fatalf("MySQL Max[time.Time] = %v, %v, %v; want t0+2h", at, ok, err)
	}
	if sum, ok, err := Sum[int64](ctx, s, "qty"); err != nil || !ok || sum != 33 {
		t.Fatalf("MySQL Sum = %d, %v, %v; want 33", sum, ok, err)
	}

	groups, err := GroupBy[string](ctx, s, "status", []Aggregate{
		CountRows(), SumOf("qty"), AvgOf("price"), MaxOf("at"), CountDistinctOf("qty"),
	})
	if err != nil || len(groups) != 2 || groups[0].Key != "draft" {
		t.Fatalf("MySQL groups = %v, %v; want [draft paid]", groups, err)
	}
	paid := groups[1]
	if n, ok := paid.Values[1].Int64(); !ok || n != 30 {
		t.Fatalf("MySQL paid sum = %d, %v; want 30 (DECIMAL bytes must parse)", n, ok)
	}
	if avg, ok := paid.Values[2].Float64(); !ok || math.Abs(avg-1.875) > 1e-9 {
		t.Fatalf("MySQL paid avg price = %v, %v; want 1.875", avg, ok)
	}
	if at, ok := paid.Values[3].Time(); !ok || !at.Equal(aggT0.Add(time.Hour)) {
		t.Fatalf("MySQL paid max at = %v, %v; want t0+1h", at, ok)
	}

	// Bool group keys ride MySQL TINYINT 0/1.
	flags, err := GroupBy[bool](ctx, s, "flag", []Aggregate{CountRows()})
	if err != nil || len(flags) != 2 {
		t.Fatalf("MySQL bool groups = %v, %v; want 2", flags, err)
	}

	// Round-1 #1 convergence check, scoped by round-2 #1: WITHIN ONE
	// process the driver rewrites every write into its one Loc, so
	// instants written under different Go zones still aggregate as one
	// value. Across processes in different TZs they would not — see
	// TestAggregate_Round2MySQLDatetimeStoresWallClocks.
	inst := time.Date(2026, 7, 3, 6, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{inst, inst.In(time.FixedZone("p8", 8*3600))} {
		if err := s.Create(ctx, &AggSale{Status: "zoned", Qty: 1, Price: 1, Flag: true, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := CountDistinct(ctx, s, "at", where.WithFilter("status", "zoned")); err != nil || n != 1 {
		t.Fatalf("MySQL: one instant in two zones counts %d distinct, %v; want 1", n, err)
	}
}

// r6EnumModel exercises round-6 review #2 on MySQL: an ENUM column, whose
// INFORMATION_SCHEMA base type is "enum" — a string type the whitelist
// must admit.
type r6EnumModel struct {
	db.Model
	Tier string `json:"tier" gorm:"type:enum('free','pro')"`
}

func (r6EnumModel) RIDPrefix() string { return "r6e" }

// TestAggregate_Round6MySQLEnumColumn is the round-6 review #2 regression
// on a real MySQL server (make test-mysql lane): the MySQL string
// whitelist dropped "enum", so grouping or distinct-counting an ENUM
// column — a bounded string type — was falsely rejected.
func TestAggregate_Round6MySQLEnumColumn(t *testing.T) {
	gdb := dbtest.OpenMySQL(t)
	ctx := context.Background()
	if err := gdb.Migrate(ctx, db.Table(&r6EnumModel{})); err != nil {
		t.Fatal(err)
	}
	s := New[r6EnumModel](gdb, log.Empty(), WithQueryFields("id", "tier"))
	for _, tier := range []string{"free", "free", "pro"} {
		if err := s.Create(ctx, &r6EnumModel{Tier: tier}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := CountDistinct(ctx, s, "tier"); err != nil || n != 2 {
		t.Fatalf("CountDistinct over an ENUM column = %d, %v; want 2", n, err)
	}
	groups, err := GroupBy[string](ctx, s, "tier", []Aggregate{CountRows()})
	if err != nil || len(groups) != 2 {
		t.Fatalf("GroupBy over an ENUM column = %v, %v; want 2 groups", groups, err)
	}
}

// openMySQLLocWriter opens a second, raw go-sql-driver connection to
// the SAME per-test database the store handle uses, with the driver Loc
// pinned to loc — a faithful stand-in for another chok process running
// in that zone (db.Open pins Loc to the process's time.Local). Writing
// time.Time values through it exercises the driver's real wall-clock
// conversion, so these tests pin driver and column-mapping behaviour,
// not a hand-formatted string.
// mysqlCurrentDatabase reports the per-test database a store handle is
// connected to, so further connections can target the same schema.
func mysqlCurrentDatabase(t *testing.T, s *Store[AggSale]) string {
	t.Helper()
	gdb, err := s.Unsafe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var dbName string
	if err := gdb.Raw("SELECT DATABASE()").Row().Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	return dbName
}

func openMySQLLocWriter(t *testing.T, s *Store[AggSale], loc *time.Location) *sql.DB {
	t.Helper()
	cfg, err := gomysql.ParseDSN(os.Getenv(dbtest.MySQLDSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = mysqlCurrentDatabase(t, s)
	cfg.ParseTime = true
	cfg.Loc = loc
	// NewConnector keeps the *time.Location as an object: a DSN round
	// trip would serialise a FixedZone by name and fail LoadLocation.
	connector, err := gomysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn := sql.OpenDB(connector)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestAggregate_Round2MySQLDatetimeStoresWallClocks pins the mechanical
// basis of the MySQL deployment invariant (round-2 review #1): time
// columns are DATETIME(3), which stores the wall clock of the writing
// connection's Loc and — per the MySQL manual, unlike TIMESTAMP —
// performs no UTC conversion. Two writers in different zones therefore
// store one instant as two values, and no read-side expression can
// repair it (the stored wall clock carries no zone). The second writer
// here is a real driver connection with a different Loc (round-3
// hardening: the driver's own conversion is what gets pinned) — if a
// future driver or column-mapping change makes this converge, the
// invariant documentation must be revisited along with this test.
func TestAggregate_Round2MySQLDatetimeStoresWallClocks(t *testing.T) {
	s := setupAggStoreOn(t, dbtest.OpenMySQL(t))
	ctx := context.Background()

	inst := time.Date(2026, 7, 4, 6, 0, 0, 0, time.UTC)
	for _, status := range []string{"w1", "w2"} {
		if err := s.Create(ctx, &AggSale{Status: status, Qty: 1, Price: 1, Flag: true, At: inst}); err != nil {
			t.Fatal(err)
		}
	}
	// Same process, same Loc: one instant, one value.
	if n, err := CountDistinct(ctx, s, "at"); err != nil || n != 1 {
		t.Fatalf("single-writer baseline: CountDistinct = %d, %v; want 1", n, err)
	}

	// A writer three hours east of this process rewrites the second row
	// with the SAME instant — the driver converts it to that zone's wall
	// clock before sending.
	_, offset := inst.In(time.Local).Zone()
	east := openMySQLLocWriter(t, s, time.FixedZone("east3", offset+3*3600))
	if _, err := east.Exec("UPDATE agg_sales SET at = ? WHERE status = 'w2'", inst); err != nil {
		t.Fatal(err)
	}
	if n, err := CountDistinct(ctx, s, "at"); err != nil || n != 2 {
		t.Fatalf("cross-TZ writers: CountDistinct = %d, %v; want the documented divergence to 2", n, err)
	}
}

// TestAggregate_Round3MySQLDSTFoldCollapsesInstants pins the round-3
// review #1 sharpening of the invariant: sharing one zone is NOT enough
// when that zone has DST transitions. At the America/New_York 2026
// fall-back, 05:30Z and 06:30Z are both wall clock 01:30 (EDT then
// EST), so a single writer in that zone stores two DIFFERENT instants
// as one identical DATETIME — CountDistinct folds, GROUP BY merges,
// MIN/MAX cannot separate them, and the stored value carries nothing to
// repair it with. Hence the deployment invariant demands one FIXED
// zone, with TZ=UTC as the recommendation. Both writes go through a
// real driver connection pinned to the DST zone.
func TestAggregate_Round3MySQLDSTFoldCollapsesInstants(t *testing.T) {
	s := setupAggStoreOn(t, dbtest.OpenMySQL(t))
	ctx := context.Background()

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	beforeFold := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC) // 01:30 EDT
	afterFold := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)  // 01:30 EST
	if beforeFold.Equal(afterFold) {
		t.Fatal("sanity: the two instants must differ")
	}
	for _, status := range []string{"w1", "w2"} {
		if err := s.Create(ctx, &AggSale{Status: status, Qty: 1, Price: 1, Flag: true, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	nyWriter := openMySQLLocWriter(t, s, ny)
	for status, at := range map[string]time.Time{"w1": beforeFold, "w2": afterFold} {
		if _, err := nyWriter.Exec("UPDATE agg_sales SET at = ? WHERE status = ?", at, status); err != nil {
			t.Fatal(err)
		}
	}
	// Two distinct instants, one stored wall clock: the fold is real and
	// unrepairable — the documented reason the invariant requires a
	// fixed (transition-free) zone, recommending TZ=UTC.
	if n, err := CountDistinct(ctx, s, "at"); err != nil || n != 1 {
		t.Fatalf("DST fold: CountDistinct = %d, %v; the two instants collapse into 1 stored value", n, err)
	}
	groups, err := GroupBy[time.Time](ctx, s, "at", []Aggregate{CountRows()})
	if err != nil || len(groups) != 1 {
		t.Fatalf("DST fold: groups = %v, %v; the two instants merge into 1 bucket", groups, err)
	}
}
