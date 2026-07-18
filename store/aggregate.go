package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/store/where"
)

// Aggregates are the front door for the dashboard reads that previously
// required Unsafe: single-value Sum / Avg / Min / Max / CountDistinct and
// grouped GroupBy. They are free functions for the same reason Pluck is
// (Go methods cannot introduce type parameters), and they run under
// exactly the read semantics of Count:
//
//   - the aggregated field and the group field resolve through the query
//     allowlist like WithFilter/WithOrder — an undeclared field is
//     rejected with a raw where.ErrUnknownField (server code wrote the
//     name, so a typo is a programming bug, not client input);
//   - scopes (OwnerScope and custom) apply fail-closed, soft-deleted rows
//     are excluded, and filter options narrow the aggregated set;
//   - pagination / ordering / count options are stripped exactly like
//     Count strips them — an aggregate is total-shaped, so they could not
//     change the single result row. GroupBy instead REJECTS them: on a
//     row-set result a silently dropped WithOrder+WithLimit would read as
//     a top-N query while returning something else.
//
// Column-kind discipline rides the same schema derivation the cursor
// encoder uses (a zero-value row through the full GORM value pipeline, so
// serializer / driver.Valuer fields classify by their WIRE type): Sum and
// Avg accept numeric columns only; Min and Max additionally accept time
// columns (dashboards legitimately want the newest created_at per group);
// string and bool columns are not aggregatable in v1 — MIN over text is
// collation-defined and differs across dialects.
//
// Result typing is a deliberate three-dialect convergence. SUM of an
// integer column returns bigint on PostgreSQL, DECIMAL on MySQL and a
// dynamically-typed integer on SQLite; the framework converges every
// aggregate to the caller-declared Go type and refuses lossy readings:
// Sum[int64] is exact and errors loudly past the int64 range (never a
// silent truncation), Sum[float64] documents the usual beyond-2^53
// precision trade, Avg is always float64 (every dialect returns a
// fractional type). Do not build money-grade reporting on Avg — exact
// decimal aggregation is a raw-SQL job.

// AggregateNumber is the set of Go types a Sum can converge to. int64
// demands an integer-kind column (exact math, loud overflow); float64
// accepts integer and float columns (one conversion, IEEE-754 precision
// past 2^53).
type AggregateNumber interface {
	int64 | float64
}

// AggregateScalar is the set of Go types Min and Max can converge to:
// the numeric pair plus time.Time for timestamp columns.
type AggregateScalar interface {
	int64 | float64 | time.Time
}

// Sum returns the sum of a declared numeric field over the rows matching
// the filter options, under the Store's scopes and soft-delete rules.
//
// SQL aggregate semantics apply: NULL values do not contribute, and when
// no non-NULL value was aggregated at all (zero matching rows, or every
// matched value NULL) the database returns SQL NULL — surfaced as
// ok=false with N's zero value, never conflated with a legitimate zero
// sum:
//
//	total, ok, err := store.Sum[int64](ctx, orders, "amount",
//	    where.WithFilter("status", "paid"))
//
// N=int64 requires an integer-kind column and is exact: a sum beyond the
// int64 range is a loud error (PostgreSQL and MySQL return it as an
// arbitrary-precision value; SQLite raises its own integer overflow).
// N=float64 also accepts integer columns, trading exactness above 2^53
// for range. Choosing N=int64 on a float column is rejected up front.
func Sum[N AggregateNumber, T db.Modeler](ctx context.Context, s *Store[T], field string, opts ...where.Option) (N, bool, error) {
	return singleAggregate[N](ctx, s, aggSum, field, opts)
}

// Avg returns the average of a declared numeric field over the rows
// matching the filter options. The result is always float64: every
// dialect returns a fractional type for AVG (PostgreSQL numeric, MySQL
// DECIMAL, SQLite float), so there is no exact integer reading to offer.
// Zero aggregated values surface as ok=false, exactly like Sum.
func Avg[T db.Modeler](ctx context.Context, s *Store[T], field string, opts ...where.Option) (float64, bool, error) {
	return singleAggregate[float64](ctx, s, aggAvg, field, opts)
}

// Min returns the smallest value of a declared field over the rows
// matching the filter options. Numeric columns read as int64/float64
// under the same rules as Sum; time columns read as N=time.Time —
// compare instants with Equal, not ==: SQLite stores text timestamps in
// the writer's zone and hands the offset back. Zero aggregated values
// surface as ok=false.
func Min[N AggregateScalar, T db.Modeler](ctx context.Context, s *Store[T], field string, opts ...where.Option) (N, bool, error) {
	return singleAggregate[N](ctx, s, aggMin, field, opts)
}

// Max returns the largest value of a declared field over the rows
// matching the filter options; the mirror of Min.
func Max[N AggregateScalar, T db.Modeler](ctx context.Context, s *Store[T], field string, opts ...where.Option) (N, bool, error) {
	return singleAggregate[N](ctx, s, aggMax, field, opts)
}

// CountDistinct returns the number of distinct non-NULL values of a
// declared field over the rows matching the filter options — the
// counting sibling of PluckDistinct that never materialises the values.
// Any declared field qualifies (COUNT DISTINCT is type-agnostic), and
// zero matching rows count as 0 — COUNT never returns SQL NULL, so there
// is no ok bool to consult.
func CountDistinct[T db.Modeler](ctx context.Context, s *Store[T], field string, opts ...where.Option) (int64, error) {
	col, err := s.aggColumn(field)
	if err != nil {
		return 0, err
	}
	raw, err := s.runSingleAggregate(ctx, "COUNT(DISTINCT "+col+")", opts)
	if err != nil {
		return 0, err
	}
	if raw == nil {
		return 0, nil
	}
	return aggInt64(raw)
}

// --- grouped aggregation ----------------------------------------------

// Aggregate declares one aggregate value a GroupBy computes per group.
// Construct with CountRows / CountDistinctOf / SumOf / AvgOf / MinOf /
// MaxOf; the zero value is invalid.
type Aggregate struct {
	fn    aggFn
	field string
}

// CountRows counts the rows of each group (COUNT(*)). Reads as int64.
func CountRows() Aggregate { return Aggregate{fn: aggCountRows} }

// CountDistinctOf counts the distinct non-NULL values of a declared
// field within each group (COUNT(DISTINCT field)). Reads as int64.
func CountDistinctOf(field string) Aggregate { return Aggregate{fn: aggCountDistinct, field: field} }

// SumOf sums a declared numeric field within each group. Integer-kind
// columns read as int64 (exact, loud past the int64 range — unlike the
// single-value Sum there is no float64 opt-out per aggregate); float
// columns read as float64.
func SumOf(field string) Aggregate { return Aggregate{fn: aggSum, field: field} }

// AvgOf averages a declared numeric field within each group. Reads as
// float64.
func AvgOf(field string) Aggregate { return Aggregate{fn: aggAvg, field: field} }

// MinOf takes the smallest value of a declared field within each group.
// Integer columns read as int64, float columns as float64, time columns
// as time.Time.
func MinOf(field string) Aggregate { return Aggregate{fn: aggMin, field: field} }

// MaxOf takes the largest value of a declared field within each group;
// the mirror of MinOf.
func MaxOf(field string) Aggregate { return Aggregate{fn: aggMax, field: field} }

// AggValue is one aggregate result within a group. Its dynamic kind is
// fixed by the Aggregate that produced it (see the constructors), so the
// matching accessor is known statically at the call site; the ok bool
// only turns false on a kind mismatch or a NULL. A NULL — a group whose
// every value for that column was NULL — reports IsNull true and every
// accessor false; COUNT kinds are never NULL.
type AggValue struct {
	kind aggValueKind
	i    int64
	f    float64
	t    time.Time
}

type aggValueKind uint8

const (
	aggValueNull aggValueKind = iota
	aggValueInt
	aggValueFloat
	aggValueTime
)

// IsNull reports whether the aggregate saw no non-NULL input value.
func (v AggValue) IsNull() bool { return v.kind == aggValueNull }

// Int64 returns the value of an int64-kind aggregate (counts, and
// Sum/Min/Max over integer columns). ok is false for NULL and for other
// kinds — an int64 reading of a float aggregate would truncate, so it is
// refused rather than rounded.
func (v AggValue) Int64() (int64, bool) {
	if v.kind != aggValueInt {
		return 0, false
	}
	return v.i, true
}

// Float64 returns the value of a float64-kind aggregate. int64-kind
// values convert (the usual IEEE-754 precision trade past 2^53), so
// dashboard code can read every numeric aggregate through one accessor.
func (v AggValue) Float64() (float64, bool) {
	switch v.kind {
	case aggValueFloat:
		return v.f, true
	case aggValueInt:
		return float64(v.i), true
	}
	return 0, false
}

// Time returns the value of a time-kind aggregate (Min/Max over a time
// column). Compare with Equal, not == — SQLite hands back the writer's
// zone offset.
func (v AggValue) Time() (time.Time, bool) {
	if v.kind != aggValueTime {
		return time.Time{}, false
	}
	return v.t, true
}

// Group is one GROUP BY bucket: the group key plus the aggregate values
// in the order the aggregates were declared (Values[i] belongs to
// aggs[i]).
type Group[K comparable] struct {
	Key    K
	Values []AggValue
}

// GroupBy buckets the rows matching the filter options by a declared
// field and computes one or more aggregates per bucket:
//
//	groups, err := store.GroupBy[string](ctx, orders, "status",
//	    []store.Aggregate{store.CountRows(), store.SumOf("amount")},
//	    where.WithFilterOp("created_at", where.Gte, since))
//
// The group field resolves through the query allowlist and K must match
// its wire kind exactly: string columns read as K=string, integer as
// int64, unsigned as uint64, float as float64, bool as bool, time as
// time.Time. A NULL group key is a loud error, not a zero-value bucket —
// SQL keeps the NULL group separate from the zero-value group, and
// collapsing them Go-side would silently merge two different answers;
// filter NULLs out with where.WithFilterNotNull (or group by a NOT NULL
// column).
//
// Results are ordered by the group key ascending — deterministic output,
// using only the allowlisted group column. Ordering by an AGGREGATE
// value (top-N) is deliberately not pushed down in v1: expression ORDER
// BY stays outside the allowlist model, and a GROUP BY result is sized
// by the group column's distinct values — dashboard-shaped columns
// (status, type, day buckets) yield sets small enough to sort in memory.
// For the same reason GroupBy carries no LIMIT, and non-filter options
// are rejected rather than stripped: a silently dropped
// WithOrder+WithLimit would look exactly like a top-N query while
// returning key-ordered buckets.
//
// Scopes and soft-delete rules apply as in every read; filters narrow
// the rows BEFORE grouping (SQL WHERE — a HAVING equivalent is
// deliberately absent, see the package documentation on docs/db.md).
// Zero matching rows return an empty, non-nil slice.
func GroupBy[K comparable, T db.Modeler](ctx context.Context, s *Store[T], field string, aggs []Aggregate, opts ...where.Option) ([]Group[K], error) {
	if len(aggs) == 0 {
		return nil, fmt.Errorf("store: GroupBy: at least one Aggregate is required (distinct group keys alone are PluckDistinct's job)")
	}
	keyCol, keySpec, err := s.aggFieldSpec("GroupBy", field)
	if err != nil {
		return nil, err
	}
	if err := validateGroupKeyType[K](field, keySpec); err != nil {
		return nil, err
	}

	selects := make([]string, 0, len(aggs)+1)
	selects = append(selects, keyCol)
	kinds := make([]aggValueKind, len(aggs))
	for i, agg := range aggs {
		expr, kind, err := s.aggPlan(agg)
		if err != nil {
			return nil, err
		}
		selects = append(selects, expr)
		kinds[i] = kind
	}

	q, err := s.aggBase(ctx, opts, true)
	if err != nil {
		return nil, err
	}
	rows, err := q.Select(strings.Join(selects, ", ")).
		Group(keyCol).
		Order(keyCol + " ASC").
		Rows()
	if err != nil {
		return nil, s.mapError(err)
	}
	defer rows.Close()

	out := []Group[K]{}
	targets := make([]any, len(aggs)+1)
	for rows.Next() {
		raws := make([]any, len(aggs)+1)
		for i := range raws {
			targets[i] = &raws[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, s.mapError(err)
		}
		if raws[0] == nil {
			return nil, fmt.Errorf("store: GroupBy: field %q produced a NULL group key; group by a NOT NULL column or add where.WithFilterNotNull(%q)", field, field)
		}
		key, err := coerceGroupKey[K](raws[0])
		if err != nil {
			return nil, fmt.Errorf("store: GroupBy: field %q group key: %w", field, err)
		}
		values := make([]AggValue, len(aggs))
		for i, raw := range raws[1:] {
			v, err := coerceAggValue(raw, kinds[i])
			if err != nil {
				return nil, fmt.Errorf("store: GroupBy: aggregate %d (%s): %w", i, aggs[i].fn, err)
			}
			values[i] = v
		}
		out = append(out, Group[K]{Key: key, Values: values})
	}
	if err := rows.Err(); err != nil {
		return nil, s.mapError(err)
	}
	return out, nil
}

// --- internals ---------------------------------------------------------

// aggFn names the SQL aggregate; the values render directly into the
// select list, over columns that already passed allowlist + identifier
// validation.
type aggFn string

const (
	aggSum           aggFn = "SUM"
	aggAvg           aggFn = "AVG"
	aggMin           aggFn = "MIN"
	aggMax           aggFn = "MAX"
	aggCountRows     aggFn = "COUNT"
	aggCountDistinct aggFn = "COUNT DISTINCT"
)

// singleAggregate is the shared single-value path: allowlist + kind
// gate, Count's query skeleton, then convergence to the caller's N.
func singleAggregate[N AggregateScalar, T db.Modeler](ctx context.Context, s *Store[T], fn aggFn, field string, opts []where.Option) (N, bool, error) {
	var zero N
	col, spec, err := s.aggFieldSpec(string(fn), field)
	if err != nil {
		return zero, false, err
	}
	want, err := aggTargetKind[N](fn, field, spec)
	if err != nil {
		return zero, false, err
	}
	raw, err := s.runSingleAggregate(ctx, string(fn)+"("+col+")", opts)
	if err != nil {
		return zero, false, err
	}
	if raw == nil {
		// SQL NULL: zero rows matched, or every matched value was NULL.
		return zero, false, nil
	}
	v, err := coerceAggValue(raw, want)
	if err != nil {
		return zero, false, fmt.Errorf("store: %s: field %q: %w", fn, field, err)
	}
	return aggValueAs[N](v), true, nil
}

// runSingleAggregate executes SELECT expr over the scoped, filtered row
// set and returns the raw driver value (nil = SQL NULL). It reuses
// Count's skeleton: scopes first, then ApplyFiltersOnly so pagination /
// ordering / count options are stripped — an aggregate is total-shaped,
// they could not change the one result row.
func (s *Store[T]) runSingleAggregate(ctx context.Context, expr string, opts []where.Option) (any, error) {
	q, err := s.aggBase(ctx, opts, false)
	if err != nil {
		return nil, err
	}
	var raw any
	if err := q.Select(expr).Row().Scan(&raw); err != nil {
		return nil, s.mapError(err)
	}
	return raw, nil
}

// aggBase builds the scoped, filtered *gorm.DB every aggregate runs
// over. guarded appends the filters-only guard (GroupBy: non-filter
// options are rejected, not stripped); without it, ApplyFiltersOnly
// already no-ops pagination/order/count exactly as countInternal does.
func (s *Store[T]) aggBase(ctx context.Context, opts []where.Option, guarded bool) (*gorm.DB, error) {
	base, err := s.applyScopes(ctx, s.effectiveDB(ctx).Model(new(T)))
	if err != nil {
		return nil, err
	}
	if guarded {
		// Appended after the caller options so the guard observes their
		// Config flags (same placement as ListIn's guard).
		opts = append(append([]where.Option{}, opts...), groupByFilterOnlyGuard())
	}
	q, err := where.ApplyFiltersOnly(base, s.queryFieldMap, opts)
	if err != nil {
		return nil, mapQueryError(err)
	}
	return q, nil
}

// groupByFilterOnlyGuard rejects options that do not compose with GROUP
// BY, the aggregation sibling of listInFilterOnlyGuard: row ordering,
// pagination, cursors and page-size caps all describe the ROW set, and
// silently stripping them from a grouped query would let a
// WithOrder+WithLimit call masquerade as top-N.
func groupByFilterOnlyGuard() where.Option {
	return func(db *gorm.DB, cfg *where.Config, _ map[string]string) (*gorm.DB, error) {
		if cfg.HasPage || cfg.HasCursor || cfg.HasOrder || cfg.MaxPageSize > 0 {
			return nil, fmt.Errorf("%w: GroupBy accepts filter options only; ordering, pagination and page-size caps describe rows, not groups — sort or truncate the returned groups in memory", where.ErrInvalidParam)
		}
		return db, nil
	}
}

// aggColumn resolves a field through the query allowlist. The error
// keeps mapQueryError's provenance: an unknown field on this
// programmatic entry point stays a raw where.ErrUnknownField (500).
func (s *Store[T]) aggColumn(field string) (string, error) {
	col, err := where.ResolveField(s.queryFieldMap, field)
	if err != nil {
		return "", mapQueryError(err)
	}
	return col, nil
}

// aggFieldSpec resolves a field and derives its wire-kind spec via the
// cursor probe (zero row through the full GORM value pipeline —
// serializer and driver.Valuer fields classify by wire type). Fields
// whose kind cannot be statically derived are refused: aggregating a
// column the framework cannot type is a server-side configuration error.
func (s *Store[T]) aggFieldSpec(fnName, field string) (string, cursorFieldSpec, error) {
	col, err := s.aggColumn(field)
	if err != nil {
		return "", cursorFieldSpec{}, err
	}
	if s.modelSchema == nil {
		return "", cursorFieldSpec{}, fmt.Errorf("store: %s: model schema unavailable", fnName)
	}
	fieldSchema := s.modelSchema.LookUpField(col)
	if fieldSchema == nil {
		return "", cursorFieldSpec{}, fmt.Errorf("store: %s: field %q resolves to column %q, which is missing from the model schema", fnName, field, col)
	}
	spec, ok := cursorSpecForSchemaField(s.modelSchema.ModelType, fieldSchema)
	if !ok {
		return "", cursorFieldSpec{}, fmt.Errorf("store: %s: field %q has no statically derivable scalar kind; aggregates need plain scalar columns", fnName, field)
	}
	return col, spec, nil
}

// aggPlan renders one Aggregate into its select expression and pins the
// kind its values coerce to.
func (s *Store[T]) aggPlan(agg Aggregate) (string, aggValueKind, error) {
	switch agg.fn {
	case aggCountRows:
		return "COUNT(*)", aggValueInt, nil
	case aggCountDistinct:
		col, err := s.aggColumn(agg.field)
		if err != nil {
			return "", 0, err
		}
		return "COUNT(DISTINCT " + col + ")", aggValueInt, nil
	case aggSum, aggAvg, aggMin, aggMax:
		col, spec, err := s.aggFieldSpec(string(agg.fn), agg.field)
		if err != nil {
			return "", 0, err
		}
		kind, err := aggResultKind(agg.fn, agg.field, spec)
		if err != nil {
			return "", 0, err
		}
		return string(agg.fn) + "(" + col + ")", kind, nil
	}
	return "", 0, fmt.Errorf("store: GroupBy: invalid Aggregate (zero value?); use the CountRows/SumOf/... constructors")
}

// aggResultKind gates the column kind per aggregate function and pins
// the deterministic result kind: Sum/Avg demand numeric columns, Min/Max
// additionally accept time. String and bool columns are not
// aggregatable — MIN over text is collation-defined per dialect.
func aggResultKind(fn aggFn, field string, spec cursorFieldSpec) (aggValueKind, error) {
	switch spec.kind {
	case cursorKindInt, cursorKindUint:
		if fn == aggAvg {
			return aggValueFloat, nil
		}
		return aggValueInt, nil
	case cursorKindFloat:
		return aggValueFloat, nil
	case cursorKindTime:
		if fn == aggMin || fn == aggMax {
			return aggValueTime, nil
		}
	}
	return 0, fmt.Errorf("store: %s: field %q is %s-kind; %s", fn, field, specKindName(spec.kind), aggKindRequirement(fn))
}

func aggKindRequirement(fn aggFn) string {
	if fn == aggMin || fn == aggMax {
		return "MIN/MAX require a numeric or time column"
	}
	return string(fn) + " requires a numeric column"
}

func specKindName(kind string) string {
	if kind == "" {
		return "unknown"
	}
	return kind
}

// aggTargetKind validates the caller-declared N against the column's
// wire kind for the single-value aggregates and returns the coercion
// target. The rules are asymmetric on purpose: float64 may widen from
// integer columns (documented 2^53 trade), int64 never narrows from
// float columns, and time.Time is only orderable (Min/Max).
func aggTargetKind[N AggregateScalar](fn aggFn, field string, spec cursorFieldSpec) (aggValueKind, error) {
	kind, err := aggResultKind(fn, field, spec)
	if err != nil {
		return 0, err
	}
	var n N
	switch any(n).(type) {
	case int64:
		if kind != aggValueInt {
			return 0, fmt.Errorf("store: %s: field %q is %s-kind; reading it as int64 would lose information — use float64 (or time.Time for time columns)", fn, field, specKindName(spec.kind))
		}
		return aggValueInt, nil
	case float64:
		if kind != aggValueInt && kind != aggValueFloat {
			return 0, fmt.Errorf("store: %s: field %q is %s-kind, not numeric; it cannot be read as float64", fn, field, specKindName(spec.kind))
		}
		return aggValueFloat, nil
	case time.Time:
		if kind != aggValueTime {
			return 0, fmt.Errorf("store: %s: field %q is %s-kind, not a time column; it cannot be read as time.Time", fn, field, specKindName(spec.kind))
		}
		return aggValueTime, nil
	}
	return 0, fmt.Errorf("store: %s: unsupported result type %T", fn, n)
}

// aggValueAs unwraps a coerced AggValue into the caller's N. The kind
// was validated up front, so the switch is total.
func aggValueAs[N AggregateScalar](v AggValue) N {
	var n N
	switch p := any(&n).(type) {
	case *int64:
		*p = v.i
	case *float64:
		*p = v.f
	case *time.Time:
		*p = v.t
	}
	return n
}

// coerceAggValue converges one raw driver value onto the planned kind.
// This function IS the cross-dialect result-type contract: PostgreSQL
// returns int64/string (numeric rides the text protocol), MySQL returns
// []byte for DECIMAL and time.Time under parseTime, SQLite returns
// dynamically-typed int64/float64/string. nil (SQL NULL) becomes the
// null AggValue.
func coerceAggValue(raw any, kind aggValueKind) (AggValue, error) {
	if raw == nil {
		return AggValue{kind: aggValueNull}, nil
	}
	switch kind {
	case aggValueInt:
		i, err := aggInt64(raw)
		if err != nil {
			return AggValue{}, err
		}
		return AggValue{kind: aggValueInt, i: i}, nil
	case aggValueFloat:
		f, err := aggFloat64(raw)
		if err != nil {
			return AggValue{}, err
		}
		return AggValue{kind: aggValueFloat, f: f}, nil
	case aggValueTime:
		t, err := aggTime(raw)
		if err != nil {
			return AggValue{}, err
		}
		return AggValue{kind: aggValueTime, t: t}, nil
	}
	return AggValue{}, fmt.Errorf("unsupported aggregate kind %d", kind)
}

// aggInt64 reads an integer aggregate result. Sums of integer columns
// come back as int64 (SQLite), bigint int64 (PostgreSQL SUM of int4),
// or an arbitrary-precision decimal string ([]byte on MySQL DECIMAL,
// string on PostgreSQL numeric for SUM of int8) — parsed exactly, with
// range errors surfaced loudly rather than truncated.
func aggInt64(raw any) (int64, error) {
	switch v := raw.(type) {
	case int64:
		return v, nil
	case []byte:
		return parseAggInt(string(v))
	case string:
		return parseAggInt(v)
	}
	return 0, fmt.Errorf("driver returned %T for an integer aggregate; cannot read it as int64", raw)
}

func parseAggInt(s string) (int64, error) {
	i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("integer aggregate value %q does not fit int64 (use a float64 reading for beyond-range sums): %w", s, err)
	}
	return i, nil
}

// aggFloat64 reads a float aggregate result: native float64, an integer
// widened, or a decimal string ([]byte on MySQL, string on PostgreSQL
// numeric).
func aggFloat64(raw any) (float64, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case int64:
		return float64(v), nil
	case []byte:
		return parseAggFloat(string(v))
	case string:
		return parseAggFloat(v)
	}
	return 0, fmt.Errorf("driver returned %T for a numeric aggregate; cannot read it as float64", raw)
}

func parseAggFloat(s string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("numeric aggregate value %q is not a number: %w", s, err)
	}
	return f, nil
}

// aggTimeFormats are the text timestamp layouts a MIN/MAX over a time
// column can come back in. PostgreSQL and MySQL (parseTime) hand over
// time.Time directly; SQLite stores timestamps as text and expression
// results carry no column decltype, so the driver returns the raw
// string — in the writer's zone, offset included (the same property the
// cursor encoder relies on).
var aggTimeFormats = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
}

func aggTime(raw any) (time.Time, error) {
	switch v := raw.(type) {
	case time.Time:
		return v, nil
	case []byte:
		return parseAggTime(string(v))
	case string:
		return parseAggTime(v)
	}
	return time.Time{}, fmt.Errorf("driver returned %T for a time aggregate; cannot read it as time.Time", raw)
}

func parseAggTime(s string) (time.Time, error) {
	for _, layout := range aggTimeFormats {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("time aggregate value %q matches no supported timestamp layout", s)
}

// validateGroupKeyType checks K against the group column's wire kind
// before the query runs. The mapping is exact — defined types and width
// variants are rejected so the coercion below stays a total type switch.
func validateGroupKeyType[K comparable](field string, spec cursorFieldSpec) error {
	var k K
	var want string
	switch any(k).(type) {
	case string:
		want = cursorKindString
	case int64:
		want = cursorKindInt
	case uint64:
		want = cursorKindUint
	case float64:
		want = cursorKindFloat
	case bool:
		want = cursorKindBool
	case time.Time:
		want = cursorKindTime
	default:
		return fmt.Errorf("store: GroupBy: unsupported group key type %T; use string, int64, uint64, float64, bool or time.Time", k)
	}
	if spec.kind != want {
		return fmt.Errorf("store: GroupBy: field %q is %s-kind; group key type %T expects a %s column", field, specKindName(spec.kind), k, want)
	}
	return nil
}

// coerceGroupKey converges a raw driver group-key value onto K. The
// kinds were validated against the schema up front, so this only bridges
// driver representation differences (SQLite bools are 0/1 integers,
// MySQL strings arrive as []byte, ...).
func coerceGroupKey[K comparable](raw any) (K, error) {
	var k K
	switch p := any(&k).(type) {
	case *string:
		switch v := raw.(type) {
		case string:
			*p = v
		case []byte:
			*p = string(v)
		default:
			return k, fmt.Errorf("driver returned %T for a string group key", raw)
		}
	case *int64:
		i, err := aggInt64(raw)
		if err != nil {
			return k, err
		}
		*p = i
	case *uint64:
		u, err := aggUint64(raw)
		if err != nil {
			return k, err
		}
		*p = u
	case *float64:
		f, err := aggFloat64(raw)
		if err != nil {
			return k, err
		}
		*p = f
	case *bool:
		b, err := aggBool(raw)
		if err != nil {
			return k, err
		}
		*p = b
	case *time.Time:
		t, err := aggTime(raw)
		if err != nil {
			return k, err
		}
		*p = t
	default:
		return k, fmt.Errorf("unsupported group key type %T", k)
	}
	return k, nil
}

func aggUint64(raw any) (uint64, error) {
	switch v := raw.(type) {
	case int64:
		if v < 0 {
			return 0, fmt.Errorf("unsigned group key came back negative (%d)", v)
		}
		return uint64(v), nil
	case uint64:
		return v, nil
	case []byte:
		return parseAggUint(string(v))
	case string:
		return parseAggUint(v)
	}
	return 0, fmt.Errorf("driver returned %T for an unsigned group key", raw)
}

func parseAggUint(s string) (uint64, error) {
	u, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unsigned group key value %q: %w", s, err)
	}
	return u, nil
}

// aggBool reads a bool group key: native bools (PostgreSQL), 0/1
// integers (SQLite, MySQL TINYINT), or their text forms.
func aggBool(raw any) (bool, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case int64:
		return v != 0, nil
	case []byte:
		return parseAggBool(string(v))
	case string:
		return parseAggBool(v)
	}
	return false, fmt.Errorf("driver returned %T for a bool group key", raw)
}

func parseAggBool(s string) (bool, error) {
	switch strings.TrimSpace(s) {
	case "0", "false", "FALSE", "f":
		return false, nil
	case "1", "true", "TRUE", "t":
		return true, nil
	}
	return false, fmt.Errorf("bool group key value %q is neither a 0/1 nor a true/false form", s)
}
