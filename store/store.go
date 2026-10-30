// Package store is the typed data access of a model: CRUD with optimistic
// locking and soft delete, filtered and paged lists, aggregates, and an
// owner scope that fences every query to the caller's rows.
//
//	posts := store.New[Post](d)
//	p, err := posts.Get(ctx, store.RID(id))          // 404 when missing or not yours
//	page, err := posts.List(ctx, where.Eq("status", "draft"), where.Page(1, 20))
//	err = posts.Update(ctx, store.RID(id), store.Patch(req).Onto(p))
//
// # Fields
//
// A field is named by its public name: its JSON name, or its column when
// the model's JSON has no name for it (json:"-", or a field inside a
// named struct). id is the RID; the numeric key has no name (store.ID,
// PluckIDs, ListByIDs).
//
// The store tags say what a client may touch: store:"query" is what a
// URL filters and orders on (ListFromQuery and chok.List, QueryFields,
// the OpenAPI filters), store:"update" what Patch writes from a request.
// id, created_at and updated_at are always queryable. A model with no
// store tag at all is refused by New: the whitelist is never implicit.
//
// The code is not fenced by the tags: where options, store.Where,
// aggregates, Pluck, ListIn, Upsert's conflict fields and Set take every
// column of the model, the managed ones too (Set and Patch never write
// those). A field name that the code got from a client must be checked
// against QueryFields first; an unknown name in code is a programming
// error, a 500 that still matches where.ErrUnknownField. Alias maps a
// field to another column.
//
// # Owner scope
//
// A model embedding db.OwnedModel or db.OwnedSoftDeleteModel is fenced to
// chok.PrincipalFrom(ctx): reads see only the principal's rows (none
// without a principal), writes stamp and check the owner, and a write
// without a principal fails with NoPrincipalError. Principals holding one
// of the policy's admin roles see every row.
//
// # Errors
//
// Every error a handler should answer with implements chok.Coder:
// NotFoundError (404), VersionConflictError and DuplicateError (409),
// EmptyChangesError (400), NoPrincipalError (401). Return them as they are.
package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbx"
	"github.com/zynthara/chok/store/internal/q"
)

// Store is the data access of model T on a database handle.
type Store[T db.Modeler] struct {
	d            *db.DB
	schema       *schema.Schema
	queryFields  map[string]string       // public name → column: what a URL filters and orders on
	updateFields map[string]string       // public name → column: what Patch writes
	columns      map[string]string       // public name → column: every column, for the code
	filterTypes  map[string]reflect.Type // query fields whose URL values ListFromQuery parses
	constraints  map[string]string       // constraint or index name → public field
	soft         bool
	owned        bool
	policy       db.StorePolicy
	adminRoles   []string
	scopes       []ScopeFunc
	hooks        HookSet[T]
	dialect      string
}

// ScopeFunc narrows every query of a store: it receives the query and
// returns it with more conditions. An error fails the operation closed.
type ScopeFunc func(ctx context.Context, tx *gorm.DB) (*gorm.DB, error)

// HookSet holds the functions a store calls before it writes; a hook's
// error aborts the write. BeforeCreate may still change the object; it
// runs after the owner is stamped. BeforeUpdate receives a copy of the
// changes, field name to the value about to be written, after they
// passed validation. Upsert and BatchUpsert run BeforeCreate.
type HookSet[T any] struct {
	BeforeCreate func(ctx context.Context, obj *T) error
	BeforeUpdate func(ctx context.Context, loc Locator, changes map[string]any) error
	BeforeDelete func(ctx context.Context, loc Locator) error
}

// Option configures New.
type Option func(*config)

type config struct {
	policy      *db.StorePolicy
	scopes      []ScopeFunc
	hooks       []any
	aliases     map[string]string
	constraints map[string]string
}

// Policy replaces db.store, the handle's default posture, for this store.
// The policy is copied: changing p afterwards changes nothing.
func Policy(p db.StorePolicy) Option {
	p.AdminRoles = slices.Clone(p.AdminRoles)
	if p.OwnerScope != nil {
		on := *p.OwnerScope
		p.OwnerScope = &on
	}
	return func(c *config) { c.policy = &p }
}

// Scope adds fn to the conditions of every query, after the owner scope;
// it panics on a nil fn.
func Scope(fn ScopeFunc) Option {
	if fn == nil {
		panic("store.Scope: nil ScopeFunc")
	}
	return func(c *config) { c.scopes = append(c.scopes, fn) }
}

// Hooks sets the functions called before creates, updates and deletes.
func Hooks[T db.Modeler](h HookSet[T]) Option {
	return func(c *config) { c.hooks = append(c.hooks, h) }
}

// Alias maps a declared field to another column, for a JSON name that
// differs from the column gorm derives.
func Alias(field, column string) Option {
	return func(c *config) {
		if c.aliases == nil {
			c.aliases = map[string]string{}
		}
		c.aliases[field] = column
	}
}

// Constraints names the field a unique constraint or index guards, so a
// duplicate-key error says which field collided. The model's own unique
// indexes and its SoftUnique indexes are known without it.
func Constraints(m map[string]string) Option {
	m = maps.Clone(m)
	return func(c *config) {
		if c.constraints == nil {
			c.constraints = map[string]string{}
		}
		maps.Copy(c.constraints, m)
	}
}

// New returns the store of T on d. It panics on a declaration mistake,
// which surfaces at startup: a model without store tags, a store tag
// value other than query and update, an alias of an undeclared field, a
// hook for another model, a blank admin role or constraint.
func New[T db.Modeler](d *db.DB, opts ...Option) *Store[T] {
	if d == nil {
		panic("store.New: nil *db.DB")
	}
	t := reflect.TypeFor[T]()
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("store.New: T must be a struct, got %s", t))
	}
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	model := new(T)
	stmt := &gorm.Statement{DB: d.Unsafe(context.Background())}
	if err := stmt.Parse(model); err != nil {
		panic(fmt.Sprintf("store.New: parse %s: %v", t.Name(), err))
	}
	s := &Store[T]{d: d, schema: stmt.Schema, dialect: stmt.Name()}
	s.queryFields, s.updateFields, s.columns = declaredFields(t, stmt.Schema)
	s.filterTypes = filterTypes(t, stmt.Schema, s.queryFields)
	for field, col := range cfg.aliases {
		if _, ok := s.columns[field]; !ok {
			panic(fmt.Sprintf("store.New: Alias(%q, %q): %s declares no field %q", field, col, t.Name(), field))
		}
		if !q.SafeIdent(col) {
			panic(fmt.Sprintf("store.New: Alias(%q, %q): not a plain column name", field, col))
		}
		for _, m := range []map[string]string{s.queryFields, s.updateFields, s.columns} {
			if _, ok := m[field]; ok {
				m[field] = col
			}
		}
	}
	for field, col := range s.updateFields {
		if protectedColumns[col] {
			panic(fmt.Sprintf("store.New: %s: update field %q is the managed column %q", t.Name(), field, col))
		}
	}
	s.constraints = map[string]string{}
	for _, idx := range stmt.Schema.ParseIndexes() {
		if idx.Class != "UNIQUE" {
			continue
		}
		var cols []string
		for _, f := range idx.Fields {
			if f.Field != nil {
				cols = append(cols, f.DBName)
			}
		}
		if f := s.fieldOfColumns(cols); f != "" {
			s.constraints[idx.Name] = f
		}
	}
	for _, f := range stmt.Schema.Fields {
		if f.Unique && f.DBName != "" {
			// gorm's name for the constraint of a unique tag.
			s.constraints["uni_"+stmt.Schema.Table+"_"+f.DBName] = s.fieldOfColumns([]string{f.DBName})
		}
	}
	for _, idx := range dbx.Indexes(t) {
		if f := s.fieldOfColumns(idx.Columns); f != "" {
			s.constraints[idx.Name] = f
		}
	}
	for name, field := range cfg.constraints {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(field) == "" {
			panic(fmt.Sprintf("store.New: Constraints(%q: %q): name a constraint and a field", name, field))
		}
		s.constraints[name] = field
	}
	s.soft = stmt.Schema.LookUpField("DeleteToken") != nil && stmt.Schema.LookUpField("DeletedAt") != nil
	_, s.owned = any(model).(owner)
	s.policy = d.StorePolicy()
	if cfg.policy != nil {
		s.policy = *cfg.policy
	}
	for i, r := range s.policy.AdminRoles {
		if strings.TrimSpace(r) == "" {
			panic(fmt.Sprintf("store.New: Policy: admin role %d is blank", i))
		}
	}
	s.adminRoles = slices.Clone(s.policy.AdminRoles)
	if s.owned && (s.policy.OwnerScope == nil || *s.policy.OwnerScope) {
		s.scopes = append(s.scopes, s.ownerScope)
	} else {
		s.owned = false
	}
	s.scopes = append(s.scopes, cfg.scopes...)
	for _, h := range cfg.hooks {
		hs, ok := h.(HookSet[T])
		if !ok {
			panic(fmt.Sprintf("store.New: Hooks for %T given to the store of %s", h, t.Name()))
		}
		if hs.BeforeCreate != nil {
			s.hooks.BeforeCreate = hs.BeforeCreate
		}
		if hs.BeforeUpdate != nil {
			s.hooks.BeforeUpdate = hs.BeforeUpdate
		}
		if hs.BeforeDelete != nil {
			s.hooks.BeforeDelete = hs.BeforeDelete
		}
	}
	return s
}

// protectedColumns are managed by db and store; no update may set them.
var protectedColumns = map[string]bool{
	"id": true, "rid": true, "version": true, "created_at": true, "updated_at": true,
	"deleted_at": true, "delete_token": true, "owner_id": true,
}

// declaredFields reads the model's columns by public name, and the store
// tags' subsets of them. id is the RID: the numeric key has no name.
// created_at and updated_at join id among the query fields of every
// model.
func declaredFields(t reflect.Type, s *schema.Schema) (query, update, columns map[string]string) {
	query, update, columns = map[string]string{}, map[string]string{}, map[string]string{}
	tagged := false
	for _, f := range s.Fields {
		if f.DBName == "" || f.DBName == "id" {
			continue
		}
		name := publicName(t, f)
		addField(t, columns, name, f)
		tag, ok := f.StructField.Tag.Lookup("store")
		if !ok {
			continue
		}
		tagged = true
		for part := range strings.SplitSeq(tag, ",") {
			switch strings.TrimSpace(part) {
			case "query":
				addField(t, query, name, f)
			case "update":
				addField(t, update, name, f)
			default:
				panic(fmt.Sprintf("store.New: %s.%s: store:%q; the values are query, update or query,update", t.Name(), f.Name, tag))
			}
		}
	}
	if !tagged {
		panic(fmt.Sprintf("store.New: %s declares no field with a store tag. Tag what the API may filter on and update:\n"+
			"\tTitle string `json:\"title\" store:\"query,update\"`\n"+
			"id, created_at and updated_at are always queryable.", t.Name()))
	}
	for _, name := range []string{"id", "created_at", "updated_at"} {
		if col, ok := columns[name]; ok {
			if _, set := query[name]; !set {
				query[name] = col
			}
		}
	}
	return query, update, columns
}

func addField(t reflect.Type, m map[string]string, name string, f *schema.Field) {
	if prev, dup := m[name]; dup && prev != f.DBName {
		panic(fmt.Sprintf("store.New: %s: field name %q maps to columns %q and %q", t.Name(), name, prev, f.DBName))
	}
	m[name] = f.DBName
}

// publicName is the name the model's JSON gives a field: its JSON name
// when the field shows at the top level, else its column — for
// json:"-", no JSON name, or a field inside a named struct.
func publicName(t reflect.Type, f *schema.Field) string {
	if !promoted(t, f.BindNames) {
		return f.DBName
	}
	name, _, _ := strings.Cut(f.StructField.Tag.Get("json"), ",")
	if name == "" || name == "-" {
		return f.DBName
	}
	return name
}

// promoted reports whether each struct on the path to a field embeds
// into the JSON of the model, as encoding/json/v2 has it: Go embedding
// without a JSON name, or the embed option.
func promoted(t reflect.Type, path []string) bool {
	for _, name := range path[:max(len(path)-1, 0)] {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return false
		}
		sf, ok := t.FieldByName(name)
		if !ok {
			return false
		}
		jname, opts, _ := strings.Cut(sf.Tag.Get("json"), ",")
		embedded := sf.Anonymous && jname == ""
		if !embedded && !slices.Contains(strings.Split(opts, ","), "embed") {
			return false
		}
		t = sf.Type
	}
	return true
}

// fieldOfColumns names the field a unique index guards: its first column
// that is not the owner or the delete token.
func (s *Store[T]) fieldOfColumns(cols []string) string {
	for _, c := range cols {
		if c == "owner_id" || c == "delete_token" {
			continue
		}
		for name, col := range s.queryFields {
			if col == c {
				return name
			}
		}
		for name, col := range s.updateFields {
			if col == c {
				return name
			}
		}
		for name, col := range s.columns {
			if col == c {
				return name
			}
		}
		if f := s.schema.LookUpField(c); f != nil {
			return publicName(s.schema.ModelType, f)
		}
		return c
	}
	return ""
}

// QueryFields lists the fields a URL filters and orders on: those
// ListFromQuery and chok.List accept, and the OpenAPI document lists.
func (s *Store[T]) QueryFields() []string {
	names := make([]string, 0, len(s.queryFields))
	for n := range s.queryFields {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Tx runs fn in a transaction; the store's methods called with txCtx, and
// those of any store on the same handle, join it.
func (s *Store[T]) Tx(ctx context.Context, fn func(txCtx context.Context) error) error {
	return s.d.RunInTx(ctx, fn)
}

// Unsafe returns the *gorm.DB of the store's model with the owner scope
// and the custom scopes applied, joined to the transaction in ctx. A scope
// that fails leaves the error on the returned *gorm.DB, so whatever runs
// on it fails: the escape hatch is fenced like the rest.
func (s *Store[T]) Unsafe(ctx context.Context) *gorm.DB {
	tx, err := s.scoped(ctx, s.d.Unsafe(ctx).Model(new(T)))
	if err != nil {
		tx = s.d.Unsafe(ctx).Model(new(T))
		_ = tx.AddError(err)
	}
	return tx
}

// scoped applies the owner scope and the custom scopes.
func (s *Store[T]) scoped(ctx context.Context, tx *gorm.DB) (*gorm.DB, error) {
	for _, scope := range s.scopes {
		next, err := scope(ctx, tx)
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, errors.New("store: a scope returned a nil *gorm.DB")
		}
		tx = next
	}
	return tx, nil
}

// owner is what the Owned bases add to a model.
type owner interface {
	GetOwnerID() string
	SetOwnerID(string)
}

// errNoOwner is what the owner scope returns for a write without a
// principal; reads turn it into an empty result.
var errNoOwner = &NoPrincipalError{}

// ownerScope fences a query to the principal's rows; admins see all.
func (s *Store[T]) ownerScope(ctx context.Context, tx *gorm.DB) (*gorm.DB, error) {
	p, ok := chok.PrincipalFrom(ctx)
	if !ok || p.UserID == "" {
		return nil, errNoOwner
	}
	if s.isAdmin(p) {
		return tx, nil
	}
	return tx.Where(s.schema.Table+".owner_id = ?", p.UserID), nil
}

func (s *Store[T]) isAdmin(p chok.Principal) bool {
	return slices.ContainsFunc(s.adminRoles, p.HasRole)
}

// readScoped is scoped for reads: no principal under the owner scope is
// an empty result, not an error.
func (s *Store[T]) readScoped(ctx context.Context, tx *gorm.DB) (*gorm.DB, error) {
	out, err := s.scoped(ctx, tx)
	if errors.Is(err, errNoOwner) {
		return tx.Where("1 = 0"), nil
	}
	return out, err
}

// fillOwner stamps the principal on an owned row: an admin keeps an owner
// already set, anyone else becomes the owner. Without a principal, a row
// whose OwnerID the code set is written as is unless the policy requires
// a principal; a row without an owner is refused.
func (s *Store[T]) fillOwner(ctx context.Context, obj *T) error {
	o, ok := any(obj).(owner)
	if !ok || !s.owned {
		return nil
	}
	p, has := chok.PrincipalFrom(ctx)
	if !has || p.UserID == "" {
		if s.policy.RequirePrincipal || o.GetOwnerID() == "" {
			return &NoPrincipalError{}
		}
		return nil
	}
	if s.isAdmin(p) && o.GetOwnerID() != "" {
		return nil
	}
	o.SetOwnerID(p.UserID)
	return nil
}

// pageSizes are the policy's default and largest page, within the
// 10,000 rows where accepts.
func (s *Store[T]) pageSizes() (def, maxSize int) {
	def, maxSize = s.policy.DefaultPageSize, s.policy.MaxPageSize
	if def <= 0 {
		def = 20
	}
	if maxSize <= 0 {
		maxSize = 100
	}
	maxSize = min(maxSize, 10_000)
	return min(def, maxSize), maxSize
}
