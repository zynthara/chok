package store

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

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
}

func (AggSale) RIDPrefix() string { return "ags" }

func setupAggStore(t *testing.T) *Store[AggSale] {
	t.Helper()
	return setupAggStoreOn(t, setupDB(t))
}

func setupAggStoreOn(t *testing.T, gdb *db.DB) *Store[AggSale] {
	t.Helper()
	if err := gdb.Migrate(context.Background(), db.Table(&AggSale{})); err != nil {
		t.Fatal(err)
	}
	return New[AggSale](gdb, log.Empty(),
		WithQueryFields("id", "status", "qty", "price", "rating", "flag", "at", "created_at"))
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
}
