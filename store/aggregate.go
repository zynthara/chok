package store

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/store/internal/q"
	"github.com/zynthara/chok/store/where"
)

// Aggregates and projections read like Count: any column of the model,
// the scopes and soft delete apply, and the filters narrow the rows. Results
// converge on the Go type asked for, whatever the dialect returns: Sum as
// int64 is exact and fails past the int64 range rather than wrapping, Avg
// is always float64, and time columns compare by instant (on SQLite
// through a UTC expression, at millisecond precision).

// Number is what Sum reads: int64 from an integer column, or float64 from
// any numeric column, at float precision.
type Number interface{ int64 | float64 }

// Scalar is what Min and Max read: a Number, or time.Time from a timestamp
// column.
type Scalar interface{ int64 | float64 | time.Time }

// Sum returns the sum of field over the rows the filters match, and false
// when there was nothing to sum (no rows, or only NULLs).
func (s *Store[T]) Sum[N Number](ctx context.Context, field string, opts ...where.Option) (N, bool, error) {
	return single[N](ctx, s, "SUM", field, opts)
}

// Avg returns the average of field, a numeric column, and false when there
// was nothing to average.
func (s *Store[T]) Avg(ctx context.Context, field string, opts ...where.Option) (float64, bool, error) {
	return single[float64](ctx, s, "AVG", field, opts)
}

// Min returns the smallest value of field, a numeric or time column, and
// false when there was none. Compare times with Equal: the zone differs by
// dialect.
func (s *Store[T]) Min[N Scalar](ctx context.Context, field string, opts ...where.Option) (N, bool, error) {
	return single[N](ctx, s, "MIN", field, opts)
}

// Max returns the largest value of field, as Min.
func (s *Store[T]) Max[N Scalar](ctx context.Context, field string, opts ...where.Option) (N, bool, error) {
	return single[N](ctx, s, "MAX", field, opts)
}

// CountDistinct returns the number of distinct non-NULL values of field.
// Strings count by the column's collation, which may fold case.
func (s *Store[T]) CountDistinct(ctx context.Context, field string, opts ...where.Option) (int64, error) {
	col, spec, err := s.aggField("CountDistinct", field)
	if err != nil {
		return 0, err
	}
	tx, err := s.read(ctx, &where.Config{FiltersOnly: true}, opts)
	if err != nil {
		return 0, err
	}
	var raw any
	if err := tx.Select("COUNT(DISTINCT " + s.aggExpr(col, spec.kind) + ")").Row().Scan(&raw); err != nil {
		return 0, s.mapError(err)
	}
	if raw == nil {
		return 0, nil
	}
	return toInt64(raw)
}

// Aggregate is one value GroupBy computes per group. Func is count (the
// group's rows; no Field), count_distinct, sum, avg, min or max of Field.
type Aggregate struct {
	Func  string
	Field string
}

// Group is one bucket of GroupBy: the key and the aggregates, in the order
// they were asked for.
type Group[K comparable] struct {
	Key    K          `json:"key"`
	Values []AggValue `json:"values"`
}

// AggValue is one aggregate of a Group. Its kind follows from the
// Aggregate: counts and integer sum, min and max are Int64, avg and float
// columns Float64, min and max of a time column Time. A group whose values
// were all NULL is IsNull. It encodes to JSON as a number, a time or null.
type AggValue struct {
	kind aggKind
	i    int64
	f    float64
	t    time.Time
}

type aggKind uint8

const (
	aggNull aggKind = iota
	aggInt
	aggFloat
	aggTime
)

// IsNull reports whether the aggregate saw no value.
func (v AggValue) IsNull() bool { return v.kind == aggNull }

// Int64 returns an integer aggregate; false for other kinds, which an
// int64 would truncate.
func (v AggValue) Int64() (int64, bool) { return v.i, v.kind == aggInt }

// Float64 returns a numeric aggregate, converting an integer one.
func (v AggValue) Float64() (float64, bool) {
	switch v.kind {
	case aggFloat:
		return v.f, true
	case aggInt:
		return float64(v.i), true
	}
	return 0, false
}

// Time returns a time aggregate.
func (v AggValue) Time() (time.Time, bool) { return v.t, v.kind == aggTime }

// MarshalJSON encodes the value as a number, an RFC 3339 time or null.
func (v AggValue) MarshalJSON() ([]byte, error) {
	switch v.kind {
	case aggInt:
		return strconv.AppendInt(nil, v.i, 10), nil
	case aggFloat:
		return json.Marshal(v.f)
	case aggTime:
		return json.Marshal(v.t)
	}
	return []byte("null"), nil
}

// GroupBy buckets the rows the filters match by field and computes aggs
// per bucket, ordered by key:
//
//	groups, err := orders.GroupBy[string](ctx, "status",
//	    []store.Aggregate{{Func: "count"}, {Func: "sum", Field: "amount"}})
//
// K must match the field: string, int64, uint64, float64, bool or
// time.Time. A NULL key is an error (filter with where.NotNull). Order,
// pagination and Count are refused: they describe rows, not groups.
func (s *Store[T]) GroupBy[K comparable](ctx context.Context, field string, aggs []Aggregate, opts ...where.Option) ([]Group[K], error) {
	if len(aggs) == 0 {
		return nil, errors.New("store: GroupBy needs at least one Aggregate")
	}
	keyCol, keySpec, err := s.aggField("GroupBy", field)
	if err != nil {
		return nil, err
	}
	if err := checkKey[K](field, keySpec); err != nil {
		return nil, err
	}
	key := s.aggExpr(keyCol, keySpec.kind)
	selects := []string{key}
	kinds := make([]aggKind, len(aggs))
	for i, a := range aggs {
		expr, kind, err := s.aggPlan(a)
		if err != nil {
			return nil, fmt.Errorf("store: GroupBy: aggregate %d: %w", i, err)
		}
		selects, kinds[i] = append(selects, expr), kind
	}
	cfg := &where.Config{}
	tx, err := s.read(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}
	if cfg.HasPage || cfg.HasOrder || cfg.Count || len(cfg.Preloads) > 0 {
		return nil, queryError(fmt.Errorf("%w: GroupBy takes filters only", where.ErrInvalidParam))
	}
	rows, err := tx.Select(strings.Join(selects, ", ")).Group(key).Order(key).Rows()
	if err != nil {
		return nil, s.mapError(err)
	}
	defer func() { _ = rows.Close() }()
	out := []Group[K]{}
	for rows.Next() {
		raws := make([]any, len(selects))
		dest := make([]any, len(raws))
		for i := range raws {
			dest[i] = &raws[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, s.mapError(err)
		}
		if raws[0] == nil {
			return nil, fmt.Errorf("store: GroupBy: %s has a NULL key; add where.NotNull(%q)", field, field)
		}
		k, err := toKey[K](raws[0])
		if err != nil {
			return nil, fmt.Errorf("store: GroupBy: %s: %w", field, err)
		}
		g := Group[K]{Key: k, Values: make([]AggValue, len(aggs))}
		for i, raw := range raws[1:] {
			if g.Values[i], err = toAggValue(raw, kinds[i]); err != nil {
				return nil, fmt.Errorf("store: GroupBy: aggregate %d: %w", i, err)
			}
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, s.mapError(err)
	}
	return out, nil
}

// Pluck returns field of the rows the filters match, in where.Order's
// order. Unlike List it takes every row unless where.Limit or where.Page
// says otherwise.
func (s *Store[T]) Pluck[F any](ctx context.Context, field string, opts ...where.Option) ([]F, error) {
	col, err := q.Column(s.columns, field)
	if err != nil {
		return nil, s.codeError(err)
	}
	return pluck[F](ctx, s, s.qualify(col), opts)
}

// PluckIDs returns the numeric primary keys of the rows the filters match,
// for server-side work; they never leave the server.
func (s *Store[T]) PluckIDs(ctx context.Context, opts ...where.Option) ([]uint, error) {
	return pluck[uint](ctx, s, s.table()+".id", opts)
}

func pluck[F any, T db.Modeler](ctx context.Context, s *Store[T], col string, opts []where.Option) ([]F, error) {
	cfg := &where.Config{}
	tx, err := s.read(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}
	if cfg.Cursor != "" || cfg.FirstPage {
		return nil, queryError(fmt.Errorf("%w: Pluck takes no cursor", where.ErrInvalidParam))
	}
	for _, o := range cfg.Orders {
		tx = tx.Order(clause.OrderByColumn{Column: clause.Column{Name: s.qualify(o.Column), Raw: true}, Desc: o.Desc})
	}
	if cfg.HasPage {
		size := cfg.Limit
		if size == 0 {
			size, _ = s.pageSizes()
		}
		offset := cfg.Offset
		if cfg.PageNumber > 0 {
			offset = (cfg.PageNumber - 1) * size
		}
		tx = tx.Offset(offset).Limit(size)
	}
	out := []F{}
	if err := tx.Pluck(col, &out).Error; err != nil {
		return nil, s.mapError(err)
	}
	return out, nil
}

// ListIn returns the rows whose field is one of values, querying 500
// values at a time so any number fits; options may only filter.
func (s *Store[T]) ListIn[V comparable](ctx context.Context, field string, values []V, opts ...where.Option) ([]T, error) {
	return listIn(ctx, s, "ListIn", values, func(vs []any) where.Option { return where.In(field, vs...) }, opts)
}

// ListByIDs is ListIn on the numeric primary key, which has no field
// name: the rows whose key is in ids, as PluckIDs or a foreign key column
// gives them, in key order.
func (s *Store[T]) ListByIDs(ctx context.Context, ids []uint, opts ...where.Option) ([]T, error) {
	return listIn(ctx, s, "ListByIDs", slices.Sorted(slices.Values(ids)), s.idIn, opts)
}

// listIn runs ListIn and ListByIDs: in builds the IN filter of a chunk.
func listIn[V comparable, T db.Modeler](ctx context.Context, s *Store[T], name string, values []V, in func([]any) where.Option, opts []where.Option) ([]T, error) {
	unique := make([]any, 0, len(values))
	seen := make(map[V]bool, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			unique = append(unique, v)
		}
	}
	if len(unique) == 0 {
		_, err := s.listChunk(ctx, name, in, nil, opts) // still reject bad options
		return []T{}, err
	}
	out := []T{}
	ids := map[uint]bool{}
	for chunk := range slices.Chunk(unique, 500) {
		items, err := s.listChunk(ctx, name, in, chunk, opts)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			// A case-insensitive collation can match a row from two chunks.
			id, _ := s.schema.PrioritizedPrimaryField.ValueOf(ctx, reflect.ValueOf(it))
			if n, ok := id.(uint); ok {
				if ids[n] {
					continue
				}
				ids[n] = true
			}
			out = append(out, it)
		}
	}
	return out, nil
}

func (s *Store[T]) listChunk(ctx context.Context, name string, in func([]any) where.Option, values []any, opts []where.Option) ([]T, error) {
	cfg := &where.Config{}
	tx, err := s.read(ctx, cfg, append(slices.Clone(opts), in(values)))
	if err != nil {
		return nil, err
	}
	if cfg.HasPage || cfg.HasOrder || cfg.Count {
		return nil, queryError(fmt.Errorf("%w: %s takes filters only", where.ErrInvalidParam, name))
	}
	if values == nil {
		return nil, nil
	}
	var items []T
	if err := tx.Order(s.table() + ".id").Find(&items).Error; err != nil {
		return nil, s.mapError(err)
	}
	return items, nil
}

// idIn is where.In on the numeric key, which where cannot name.
func (s *Store[T]) idIn(ids []any) where.Option {
	return func(tx *gorm.DB, cfg *where.Config, _ map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		if len(ids) == 0 {
			cfg.Degenerate = true
			return tx.Where("1 = 0"), nil
		}
		return tx.Where(s.table()+".id IN ?", ids), nil
	}
}

// single runs one aggregate function over field.
func single[N Scalar, T db.Modeler](ctx context.Context, s *Store[T], fn, field string, opts []where.Option) (N, bool, error) {
	var zero N
	col, spec, err := s.aggField(fn, field)
	if err != nil {
		return zero, false, err
	}
	kind, err := resultKind(fn, field, spec)
	if err != nil {
		return zero, false, err
	}
	if kind, err = readAs[N](fn, field, spec, kind); err != nil {
		return zero, false, err
	}
	tx, err := s.read(ctx, &where.Config{FiltersOnly: true}, opts)
	if err != nil {
		return zero, false, err
	}
	var raw any
	if err := tx.Select(fn + "(" + s.aggExpr(col, spec.kind) + ")").Row().Scan(&raw); err != nil {
		return zero, false, s.mapError(err)
	}
	if raw == nil {
		return zero, false, nil
	}
	v, err := toAggValue(raw, kind)
	if err != nil {
		return zero, false, fmt.Errorf("store: %s(%s): %w", fn, field, err)
	}
	var n N
	switch p := any(&n).(type) {
	case *int64:
		*p = v.i
	case *float64:
		*p = v.f
	case *time.Time:
		*p = v.t
	}
	return n, true, nil
}

// aggField resolves field to its column and wire kind; JSON columns and
// fields without a scalar kind cannot be aggregated. The kind follows the
// Go type; the column type the model declares only refuses what would
// compute wrongly — JSON, or text under a number or a time.
func (s *Store[T]) aggField(fn, field string) (string, fieldSpec, error) {
	col, err := q.Column(s.columns, field)
	if err != nil {
		return "", fieldSpec{}, s.codeError(err)
	}
	if table, _, qualified := strings.Cut(col, "."); qualified && table != s.table() {
		return "", fieldSpec{}, fmt.Errorf("store: %s: %s is column %q of another table; aggregates read the store's own table %s", fn, field, col, s.table())
	}
	f := s.schema.LookUpField(col[strings.LastIndexByte(col, '.')+1:])
	if f == nil {
		return "", fieldSpec{}, fmt.Errorf("store: %s: %s has no column %q", fn, s.schema.Name, col)
	}
	declared := s.declaredType(f)
	if strings.Contains(declared, "json") || strings.Contains(strings.ToLower(string(f.DataType)), "json") ||
		strings.EqualFold(f.TagSettings["SERIALIZER"], "json") {
		return "", fieldSpec{}, fmt.Errorf("store: %s: %s is a JSON column, which does not aggregate", fn, field)
	}
	spec, ok := specOf(s.schema.ModelType, f)
	if !ok {
		return "", fieldSpec{}, fmt.Errorf("store: %s: %s is not a scalar column", fn, field)
	}
	spec.textual = textual(declared)
	return s.qualify(col), spec, nil
}

// declaredType is the column type the model declares, in lower case:
// what a GormDBDataType method answers for this dialect, else the type
// tag; empty when gorm derives the type from the Go type.
func (s *Store[T]) declaredType(f *schema.Field) string {
	if dt, ok := reflect.New(f.IndirectFieldType).Interface().(interface {
		GormDBDataType(*gorm.DB, *schema.Field) string
	}); ok {
		if t := dt.GormDBDataType(s.d.Unsafe(context.Background()), f); t != "" {
			return strings.ToLower(t)
		}
	}
	return strings.ToLower(f.TagSettings["TYPE"])
}

// textual reports a declared type whose values compare as text.
func textual(declared string) bool {
	base, _, _ := strings.Cut(declared, "(")
	words := strings.Fields(base)
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "char", "character", "varchar", "nchar", "nvarchar", "text", "tinytext", "mediumtext", "longtext",
		"clob", "string", "citext", "enum", "set", "uuid":
		return true
	}
	return false
}

// aggExpr reads a column for aggregation. SQLite keeps timestamps as text
// in the writer's zone and compares them as text, so its time columns go
// through strftime, which normalizes them to UTC milliseconds.
func (s *Store[T]) aggExpr(col, kind string) string {
	if kind == kindTime && s.dialect == "sqlite" {
		return "CASE WHEN typeof(" + col + ") IN ('integer','real')" +
			" THEN strftime('%Y-%m-%d %H:%M:%f', " + col + ", 'unixepoch')" +
			" ELSE strftime('%Y-%m-%d %H:%M:%f', " + col + ") END"
	}
	return col
}

// aggPlan renders one Aggregate of GroupBy.
func (s *Store[T]) aggPlan(a Aggregate) (string, aggKind, error) {
	fn := strings.ToLower(a.Func)
	if fn == "count" {
		if a.Field != "" {
			return "", 0, errors.New(`count takes no Field; use count_distinct`)
		}
		return "COUNT(*)", aggInt, nil
	}
	col, spec, err := s.aggField(fn, a.Field)
	if err != nil {
		return "", 0, err
	}
	switch fn {
	case "count_distinct":
		return "COUNT(DISTINCT " + s.aggExpr(col, spec.kind) + ")", aggInt, nil
	case "sum", "avg", "min", "max":
		kind, err := resultKind(strings.ToUpper(fn), a.Field, spec)
		if err != nil {
			return "", 0, err
		}
		return strings.ToUpper(fn) + "(" + s.aggExpr(col, spec.kind) + ")", kind, nil
	}
	return "", 0, fmt.Errorf("unknown Func %q: use count, count_distinct, sum, avg, min or max", a.Func)
}

// resultKind is what fn of a column of spec returns.
func resultKind(fn, field string, spec fieldSpec) (aggKind, error) {
	if spec.textual {
		return 0, fmt.Errorf("store: %s(%s): the column is declared as text, which adds and orders as text; declare a numeric or time column type", fn, field)
	}
	switch spec.kind {
	case kindInt, kindUint:
		if fn == "AVG" {
			return aggFloat, nil
		}
		return aggInt, nil
	case kindFloat:
		return aggFloat, nil
	case kindTime:
		if fn == "MIN" || fn == "MAX" {
			return aggTime, nil
		}
	}
	need := "a numeric"
	if fn == "MIN" || fn == "MAX" {
		need = "a numeric or time"
	}
	return 0, fmt.Errorf("store: %s(%s): %s needs %s column, not %s", fn, field, fn, need, spec.kind)
}

// readAs checks that N can hold fn's result without loss.
func readAs[N Scalar](fn, field string, spec fieldSpec, kind aggKind) (aggKind, error) {
	var n N
	switch any(n).(type) {
	case int64:
		if kind != aggInt {
			return 0, fmt.Errorf("store: %s(%s): a %s column would lose precision as int64; read float64", fn, field, spec.kind)
		}
	case float64:
		if kind != aggInt && kind != aggFloat {
			return 0, fmt.Errorf("store: %s(%s): a %s column is not numeric", fn, field, spec.kind)
		}
		return aggFloat, nil
	case time.Time:
		if kind != aggTime {
			return 0, fmt.Errorf("store: %s(%s): a %s column is not a time", fn, field, spec.kind)
		}
	}
	return kind, nil
}

func toAggValue(raw any, kind aggKind) (AggValue, error) {
	if raw == nil {
		return AggValue{}, nil
	}
	var v AggValue
	var err error
	switch kind {
	case aggInt:
		v.i, err = toInt64(raw)
	case aggFloat:
		v.f, err = toFloat64(raw)
	case aggTime:
		v.t, err = toTime(raw)
	}
	if err != nil {
		return AggValue{}, err
	}
	v.kind = kind
	return v, nil
}

// The drivers return aggregates as int64, float64, time.Time, or as text
// (MySQL DECIMAL, Postgres numeric, SQLite strftime).

func toInt64(raw any) (int64, error) {
	switch v := raw.(type) {
	case int64:
		return v, nil
	case []byte:
		return parseInt(string(v))
	case string:
		return parseInt(v)
	}
	return 0, fmt.Errorf("the driver returned a %T for an integer", raw)
}

func parseInt(s string) (int64, error) {
	i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q does not fit an int64 (read float64 past its range)", s)
	}
	return i, nil
}

func toFloat64(raw any) (float64, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case int64:
		return float64(v), nil
	case []byte:
		return strconv.ParseFloat(strings.TrimSpace(string(v)), 64)
	case string:
		return strconv.ParseFloat(strings.TrimSpace(v), 64)
	}
	return 0, fmt.Errorf("the driver returned a %T for a number", raw)
}

var timeLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02",
}

func toTime(raw any) (time.Time, error) {
	var s string
	switch v := raw.(type) {
	case time.Time:
		return v, nil
	case []byte:
		s = string(v)
	case string:
		s = v
	default:
		return time.Time{}, fmt.Errorf("the driver returned a %T for a time", raw)
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a timestamp", s)
}

// checkKey checks that K can hold the group field's values.
func checkKey[K comparable](field string, spec fieldSpec) error {
	var k K
	want := ""
	switch any(k).(type) {
	case string:
		want = kindString
	case int64:
		want = kindInt
	case uint64:
		want = kindUint
	case float64:
		want = kindFloat
	case bool:
		want = kindBool
	case time.Time:
		want = kindTime
	default:
		return fmt.Errorf("store: GroupBy: a %T key is not supported; use string, int64, uint64, float64, bool or time.Time", k)
	}
	if spec.kind != want {
		return fmt.Errorf("store: GroupBy: %s is a %s column; a %T key needs a %s one", field, spec.kind, k, want)
	}
	return nil
}

func toKey[K comparable](raw any) (K, error) {
	var k K
	var err error
	switch p := any(&k).(type) {
	case *string:
		switch v := raw.(type) {
		case string:
			*p = v
		case []byte:
			*p = string(v)
		default:
			err = fmt.Errorf("the driver returned a %T for a string", raw)
		}
	case *int64:
		*p, err = toInt64(raw)
	case *uint64:
		var i int64
		if u, ok := raw.(uint64); ok {
			*p = u
			break
		}
		if s, ok := raw.([]byte); ok {
			*p, err = strconv.ParseUint(string(s), 10, 64)
			break
		}
		if i, err = toInt64(raw); err == nil && i < 0 {
			err = fmt.Errorf("unsigned key %d is negative", i)
		}
		*p = uint64(i)
	case *float64:
		*p, err = toFloat64(raw)
	case *bool:
		*p, err = toBool(raw)
	case *time.Time:
		*p, err = toTime(raw)
	}
	return k, err
}

func toBool(raw any) (bool, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case int64:
		if v == 0 || v == 1 {
			return v == 1, nil
		}
	case []byte:
		return strconv.ParseBool(string(v))
	case string:
		return strconv.ParseBool(v)
	}
	return false, fmt.Errorf("%v is a non-canonical bool (not 0 or 1)", raw)
}
