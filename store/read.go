package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/store/where"
)

// read applies opts to a scoped query of T. Without a principal, an
// owner-scoped read matches nothing.
func (s *Store[T]) read(ctx context.Context, cfg *where.Config, opts []where.Option) (*gorm.DB, error) {
	tx, err := s.readScoped(ctx, s.d.Unsafe(ctx).Model(new(T)))
	if err != nil {
		return nil, err
	}
	return s.apply(ctx, tx, cfg, opts)
}

// apply runs the options, then the soft-delete modifiers and the preloads
// they asked for.
func (s *Store[T]) apply(ctx context.Context, tx *gorm.DB, cfg *where.Config, opts []where.Option) (*gorm.DB, error) {
	for _, o := range opts {
		if o == nil {
			continue
		}
		next, err := o(tx, cfg, s.columns)
		if err != nil {
			return nil, s.codeError(err)
		}
		if next == nil {
			return nil, errors.New("store: a where.Option returned a nil *gorm.DB")
		}
		tx = next
	}
	switch {
	case cfg.OnlyTrashed && s.soft:
		tx = tx.Unscoped().Where(s.table() + ".deleted_at IS NOT NULL")
	case cfg.OnlyTrashed:
		tx = tx.Where("1 = 0") // without soft delete nothing is trashed
	case cfg.WithTrashed:
		tx = tx.Unscoped()
	}
	if !cfg.FiltersOnly {
		for _, rel := range cfg.Preloads {
			var err error
			if tx, err = s.preload(ctx, tx, rel); err != nil {
				return nil, err
			}
		}
	}
	return tx, nil
}

var ownerType = reflect.TypeFor[owner]()

// preload loads relation, a path of associations such as "Author" or
// "Comments.Author", or "*" for every direct association. An association
// whose model embeds an Owned base is fenced like a store of it would
// fence it, under this store's policy: the principal's rows only, all of
// them for an admin, none without a principal. The store's custom scopes
// are written for its own table and do not apply to associations.
func (s *Store[T]) preload(ctx context.Context, tx *gorm.DB, relation string) (*gorm.DB, error) {
	if relation == "*" || relation == clause.Associations {
		names := slices.Sorted(maps.Keys(s.schema.Relationships.Relations))
		for _, name := range names {
			var err error
			if tx, err = s.preload(ctx, tx, name); err != nil {
				return nil, err
			}
		}
		return tx, nil
	}
	sch := s.schema
	path := ""
	for part := range strings.SplitSeq(relation, ".") {
		rel := sch.Relationships.Relations[part]
		if rel == nil || rel.FieldSchema == nil {
			return nil, fmt.Errorf("store: Preload(%q): %s has no association %q", relation, sch.Name, part)
		}
		path, sch = strings.TrimPrefix(path+"."+part, "."), rel.FieldSchema
		if fence := s.associationFence(ctx, sch); fence != nil {
			tx = tx.Preload(path, fence)
		}
	}
	if _, registered := tx.Statement.Preloads[relation]; !registered {
		tx = tx.Preload(relation)
	}
	return tx, nil
}

// associationFence is the condition that fences an owned association,
// or nil for one without an owner or under a policy without the scope.
func (s *Store[T]) associationFence(ctx context.Context, sch *schema.Schema) func(*gorm.DB) *gorm.DB {
	if s.policy.OwnerScope != nil && !*s.policy.OwnerScope {
		return nil
	}
	if f := sch.LookUpField("owner_id"); f == nil || !reflect.PointerTo(sch.ModelType).Implements(ownerType) {
		return nil
	}
	p, ok := chok.PrincipalFrom(ctx)
	return func(q *gorm.DB) *gorm.DB {
		switch {
		case !ok || p.UserID == "":
			return q.Where("1 = 0")
		case s.isAdmin(p):
			return q
		}
		return q.Where(sch.Table+".owner_id = ?", p.UserID)
	}
}

// qualify prefixes a bare column with the store's table.
func (s *Store[T]) qualify(col string) string {
	if strings.Contains(col, ".") {
		return col
	}
	return s.table() + "." + col
}

// Get returns the row loc names, or NotFoundError when there is none the
// caller may see. Options add filters, WithTrashed and Preload.
func (s *Store[T]) Get(ctx context.Context, loc Locator, opts ...where.Option) (*T, error) {
	return s.get(ctx, loc, opts, false)
}

// GetForUpdate is Get that also locks the row until the transaction in ctx
// ends (SELECT ... FOR UPDATE), so no one else writes it in between; it
// fails outside a transaction. On SQLite the transaction already holds the
// only write connection, which is the stronger lock.
func (s *Store[T]) GetForUpdate(ctx context.Context, loc Locator, opts ...where.Option) (*T, error) {
	if !s.d.InTx(ctx) {
		return nil, errors.New("store: GetForUpdate needs a transaction: call it inside Tx")
	}
	return s.get(ctx, loc, opts, true)
}

func (s *Store[T]) get(ctx context.Context, loc Locator, opts []where.Option, lock bool) (*T, error) {
	if loc == nil {
		return nil, errors.New("store: nil Locator")
	}
	cfg := &where.Config{}
	tx, err := s.read(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}
	if lock && len(cfg.Preloads) > 0 {
		return nil, errors.New("store: GetForUpdate cannot preload: the association queries would run outside the lock")
	}
	if tx, err = loc.apply(s, tx); err != nil {
		return nil, s.codeError(err)
	}
	if lock && s.dialect != "sqlite" {
		tx = tx.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	var obj T
	if err := tx.First(&obj).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &NotFoundError{Locator: loc.String()}
		}
		return nil, s.mapError(err)
	}
	return &obj, nil
}

// Exists reports whether loc names a row the caller may see.
func (s *Store[T]) Exists(ctx context.Context, loc Locator) (bool, error) {
	if loc == nil {
		return false, errors.New("store: nil Locator")
	}
	tx, err := s.readScoped(ctx, s.d.Unsafe(ctx).Model(new(T)))
	if err != nil {
		return false, err
	}
	return s.exists(tx, loc)
}

func (s *Store[T]) exists(tx *gorm.DB, loc Locator) (bool, error) {
	tx, err := loc.apply(s, tx)
	if err != nil {
		return false, s.codeError(err)
	}
	var one int
	res := tx.Select("1").Limit(1).Scan(&one)
	if res.Error != nil {
		return false, s.mapError(res.Error)
	}
	return res.RowsAffected > 0, nil
}

// Count returns the number of rows the filters match; order and
// pagination options are ignored.
func (s *Store[T]) Count(ctx context.Context, opts ...where.Option) (int64, error) {
	tx, err := s.read(ctx, &where.Config{FiltersOnly: true}, opts)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := tx.Count(&n).Error; err != nil {
		return 0, s.mapError(err)
	}
	return n, nil
}

// List returns one page of the rows the filters match: where.Page or
// Limit and Offset pick it (the policy's default page size otherwise, and
// never more than its max), where.Order sorts it, and the row ID breaks
// ties so pages never overlap. Page is the page number — 1 without
// pagination options, 0 for Offset and Limit, which have none. HasMore
// says whether rows follow; Total is filled only with where.Count. With
// where.Cursor, List is ListWithCursor.
func (s *Store[T]) List(ctx context.Context, opts ...where.Option) (*chok.Page[T], error) {
	cfg := &where.Config{}
	tx, err := s.read(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}
	if cfg.Cursor != "" || cfg.FirstPage {
		return s.keyset(ctx, tx, cfg, opts)
	}
	def, maxSize := s.pageSizes()
	size := cfg.Limit
	if size == 0 {
		size = def
	}
	size = min(size, maxSize)
	offset := int64(cfg.Offset)
	if cfg.PageNumber > 0 {
		offset = int64(cfg.PageNumber-1) * int64(size)
	}
	if offset > math.MaxInt32 {
		return nil, queryError(fmt.Errorf("%w: offset %d is too far", where.ErrInvalidParam, offset))
	}
	page := &chok.Page[T]{Page: cfg.PageNumber, Size: size}
	if !cfg.HasPage {
		page.Page = 1 // no pagination asked for: the first page
	}
	if cfg.Count {
		if page.Total, err = s.Count(ctx, opts...); err != nil {
			return nil, err
		}
	}
	for _, o := range cfg.Orders {
		tx = tx.Order(clause.OrderByColumn{Column: clause.Column{Name: s.qualify(o.Column), Raw: true}, Desc: o.Desc})
	}
	tx = tx.Order(s.table() + ".id")
	var items []T
	if err := tx.Offset(int(offset)).Limit(size + 1).Find(&items).Error; err != nil {
		return nil, s.mapError(err)
	}
	if len(items) > size {
		items, page.HasMore = items[:size], true
	}
	page.Items = nonNil(items)
	return page, nil
}

// ListFromQuery serves chok.List: page, size, order=field:asc|desc and
// cursor from the query string, and every query field as an equality
// filter (a repeated parameter matches any of its values). A filter on a
// boolean, numeric or time field is read as that type first: true or
// false (1 and 0 too), a number, an RFC 3339 time. An unknown parameter,
// or a value that does not parse, is a 400. The page always carries Total.
func (s *Store[T]) ListFromQuery(ctx context.Context, q url.Values) (*chok.Page[T], error) {
	q, typed, err := s.typedFilters(q)
	if err != nil {
		return nil, queryError(err)
	}
	opts, err := where.FromQuery(q, s.queryFields, true)
	if err != nil {
		return nil, queryError(err)
	}
	return s.List(ctx, append(append(opts, typed...), where.Count())...)
}

// typedFilters takes the filters on filterTypes fields out of q and
// returns them as options holding values of the field's Go type. As text,
// the way where.FromQuery passes every value, ?flag=true matched no row on
// SQLite and the false rows on MySQL: both read "true" as 0 against a
// boolean column. As there, an empty value is no filter and a repeated
// parameter matches any of its values.
func (s *Store[T]) typedFilters(q url.Values) (url.Values, []where.Option, error) {
	rest := maps.Clone(q)
	var opts []where.Option
	for _, name := range slices.Sorted(maps.Keys(q)) {
		t, ok := s.filterTypes[name]
		if !ok || slices.Contains(listParams, name) {
			continue
		}
		var args []any
		for _, v := range q[name] {
			if v == "" {
				continue
			}
			x, err := parseFilter(t, v)
			if err != nil {
				return nil, nil, fmt.Errorf("%w: %s must be %v, got %q", where.ErrInvalidParam, name, err, v)
			}
			args = append(args, x)
		}
		delete(rest, name)
		switch len(args) {
		case 0:
		case 1:
			opts = append(opts, where.Eq(name, args[0]))
		default:
			opts = append(opts, where.In(name, args...))
		}
	}
	return rest, opts, nil
}

// listParams are where.FromQuery's own parameters: a field of the same
// name is not a URL filter.
var listParams = []string{"page", "size", "order", "cursor"}

// filterTypes: the query fields whose URL values ListFromQuery parses —
// booleans, numbers and times, behind a pointer too. A type with its own
// driver.Valuer decides its database form and keeps the text.
func filterTypes(t reflect.Type, sch *schema.Schema, query map[string]string) map[string]reflect.Type {
	types := map[string]reflect.Type{}
	for _, f := range sch.Fields {
		if f.DBName == "" || f.DBName == "id" {
			continue
		}
		name, ft := publicName(t, f), f.IndirectFieldType
		if _, ok := query[name]; !ok || ft.Implements(valuerType) || reflect.PointerTo(ft).Implements(valuerType) {
			continue
		}
		switch ft.Kind() {
		case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			types[name] = ft
		default:
			if ft == timeType {
				types[name] = ft
			}
		}
	}
	return types
}

// parseFilter reads a URL value as a filterTypes type. Its error says
// what the value must be.
func parseFilter(t reflect.Type, v string) (any, error) {
	if t == timeType {
		at, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, errors.New("an RFC 3339 time")
		}
		return at.UTC(), nil
	}
	switch t.Kind() {
	case reflect.Bool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, errors.New("true or false")
		}
		return b, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(v, 10, t.Bits())
		if err != nil {
			return nil, errors.New("an integer")
		}
		return n, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(v, 10, t.Bits())
		if err != nil {
			return nil, errors.New("a non-negative integer")
		}
		return n, nil
	default:
		f, err := strconv.ParseFloat(v, t.Bits())
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, errors.New("a number")
		}
		return f, nil
	}
}

// ListWithCursor returns a keyset page: the rows after where.Cursor's
// cursor (none for the first page), ordered by the one where.Order given,
// or by ID. The page's Cursor continues it and is empty on the last page;
// clients pass it back untouched. Unlike offset pages, rows added or
// removed meanwhile never shift a cursor page. The order field must be a
// NOT NULL scalar; Total is filled only with where.Count.
func (s *Store[T]) ListWithCursor(ctx context.Context, opts ...where.Option) (*chok.Page[T], error) {
	cfg := &where.Config{}
	tx, err := s.read(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}
	return s.keyset(ctx, tx, cfg, opts)
}

func (s *Store[T]) keyset(ctx context.Context, tx *gorm.DB, cfg *where.Config, opts []where.Option) (*chok.Page[T], error) {
	if cfg.PageNumber > 0 || cfg.Offset > 0 || cfg.Limit > 0 {
		return nil, queryError(fmt.Errorf("%w: a cursor page takes where.Cursor, not page, limit or offset", where.ErrInvalidParam))
	}
	if len(cfg.Orders) > 1 {
		return nil, queryError(fmt.Errorf("%w: a cursor page orders by one field", where.ErrInvalidParam))
	}
	field, col, desc := "id", "rid", false
	if len(cfg.Orders) == 1 {
		field, col, desc = cfg.Orders[0].Field, cfg.Orders[0].Column, cfg.Orders[0].Desc
	}
	bare := col[strings.LastIndexByte(col, '.')+1:]
	f, ridField := s.schema.LookUpField(bare), s.schema.LookUpField("rid")
	if f == nil || ridField == nil {
		return nil, fmt.Errorf("store: ListWithCursor: %s has no column %q or rid", s.schema.Name, bare)
	}
	spec, ok := specOf(s.schema.ModelType, f)
	if !ok || f.FieldType.Kind() == reflect.Pointer {
		// A NULL boundary has no position: nullable fields cannot key a page.
		return nil, queryError(fmt.Errorf("%w: %s cannot order a cursor page; use a NOT NULL scalar field", where.ErrInvalidParam, field))
	}
	def, maxSize := s.pageSizes()
	size := cfg.CursorSize
	if size == 0 {
		size = def
	}
	size = min(size, maxSize)
	page := &chok.Page[T]{Size: size}
	if cfg.Count {
		var err error
		if page.Total, err = s.Count(ctx, opts...); err != nil {
			return nil, err
		}
	}
	key, rid := s.qualify(col), s.table()+".rid"
	if cfg.Cursor != "" {
		value, after, err := decodeCursor(cfg.Cursor, field, desc, spec)
		if err != nil {
			return nil, queryError(err)
		}
		op := " > "
		if desc {
			op = " < "
		}
		if bare == "rid" {
			tx = tx.Where(rid+op+"?", after)
		} else {
			tx = tx.Where("("+key+", "+rid+")"+op+"(?, ?)", value, after)
		}
	}
	tx = tx.Order(clause.OrderByColumn{Column: clause.Column{Name: key, Raw: true}, Desc: desc})
	if bare != "rid" {
		tx = tx.Order(clause.OrderByColumn{Column: clause.Column{Name: rid, Raw: true}, Desc: desc})
	}
	var items []T
	if err := tx.Limit(size + 1).Find(&items).Error; err != nil {
		return nil, s.mapError(err)
	}
	if len(items) > size {
		items, page.HasMore = items[:size], true
		next, err := encodeCursor(reflect.ValueOf(&items[size-1]).Elem(), field, desc, f, ridField, spec)
		if err != nil {
			return nil, err
		}
		page.Cursor = next
	}
	page.Items = nonNil(items)
	return page, nil
}

func nonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}
