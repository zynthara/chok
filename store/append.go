package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/internal/txctx"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// AppendStore is the restricted store for append-only tables — models
// embedding db.AppendOnlyModel. Its surface is Create, BatchCreate and
// List; the write-modify paths (Update, Delete, soft delete, optimistic
// locking, locators) do not exist on the type, so "rows are never
// rewritten" holds at compile time rather than by runtime guards.
//
// List speaks the same where DSL as Store.List (filters, WithPage /
// WithOffset, WithOrder, WithCount) with one append-only refinement:
// results are always deterministically ordered. Without an explicit
// order the default is insertion order (created_at, then the internal
// PK); with one, the internal PK is appended as a trailing tie-breaker
// so offset pages stay stable across created_at ties (batch inserts
// land on the same millisecond routinely). The PK participates in
// ORDER BY only — it is never exposed in responses (json:"-" on the
// base model) and there is no per-row lookup to leak it through.
//
// Keyset pagination (ListWithCursor) is not available: its tie-breaker
// binds to the RID column, which append-only models deliberately lack.
// Incremental consumers advance a created_at watermark instead:
// where.WithFilter("created_at", where.Gt, lastSeen).
//
// Transactions join via context propagation exactly like Store — pass
// a db.RunInTx txCtx and every call rides that transaction. There is
// no Tx method (no clone-based variant to compose with).
type AppendStore[T db.AppendModeler] struct {
	h                *db.DB
	logger           log.Logger
	queryFieldMap    map[string]string // filter + order allowlist
	constraintFields map[string]string // WithConstraintFields: constraint identifier → public field name
	modelSchema      *schema.Schema    // GORM's authoritative field/column mapping
	idColumn         string            // internal PK column — trailing ORDER BY tie-breaker, never client-visible
	createdAtColumn  string            // default order column (insertion order)
	scopes           []ScopeFunc
	maxPageSize      int  // max page size (0 = unlimited)
	strict           bool // strict mode: reject auto-discovered fields
	readOnly         bool // Create/BatchCreate fail with db.ErrReadOnly
}

// NewAppend creates an AppendStore bound to the given database handle.
// T must be a struct type embedding db.AppendOnlyModel; the constraint
// is compile-time — full models (db.Model embedders) do not satisfy
// db.AppendModeler, and append-only models do not satisfy db.Modeler,
// so neither type can enter the other constructor.
//
// It shares Store's option vocabulary and construction rules where they
// apply: query-side field declaration (`store:"query"` tags preferred,
// WithQueryFields / WithAllQueryFields at the call site), WithColumnAlias,
// WithConstraintFields, WithScope, WithStrict / WithoutStrict,
// WithMaxPageSize, WithReadOnly, and the handle's db.store policy for
// the unset knobs (Strict / MaxPageSize; RequirePrincipal and
// AdminRoles have no owner machinery to act on and are ignored).
//
// Options with no meaning on an append-only store panic at
// construction instead of silently no-opping: the update side
// (WithUpdateFields / WithAllUpdateFields, `store:"update"` tags), the
// owner family (WithAdminRoles / WithoutOwnerScope /
// WithRequirePrincipal / WithoutRequirePrincipal), hooks, WithBus
// (entity events bind identity to RID), and WithDefaultPageSize (it
// only feeds ListFromQuery, which AppendStore does not have).
//
// Unlike store.New there is no automatic "id" → "rid" alias: append
// models have no RID column, and their numeric PK stays internal.
func NewAppend[T db.AppendModeler](h *db.DB, logger log.Logger, opts ...StoreOption) *AppendStore[T] {
	if h == nil {
		panic("store.NewAppend: nil *db.DB handle (use db.From(k) or db.Open)")
	}
	t := reflect.TypeOf((*T)(nil)).Elem()

	if t.Kind() == reflect.Ptr {
		panic(fmt.Sprintf("store.NewAppend: T must be a struct type, got pointer %s", t))
	}

	model := reflect.New(t).Interface()
	if err := db.ValidateAppendModel(model); err != nil {
		panic(fmt.Sprintf("store.NewAppend: %v", err))
	}
	stmt := &gorm.Statement{DB: h.Unsafe(context.Background())}
	if err := stmt.Parse(model); err != nil {
		panic(fmt.Sprintf("store.NewAppend: parse GORM schema for %s: %v", t.Name(), err))
	}
	modelSchema := stmt.Schema

	// Resolve the deterministic-order columns once. ValidateAppendModel
	// guarantees the AppendOnlyModel embed, so both fields always parse.
	idField := modelSchema.LookUpField("ID")
	if idField == nil || idField.DBName == "" {
		panic(fmt.Sprintf("store.NewAppend: %s has no ID column in its parsed schema", t.Name()))
	}
	createdAtField := modelSchema.LookUpField("CreatedAt")
	if createdAtField == nil || createdAtField.DBName == "" {
		panic(fmt.Sprintf("store.NewAppend: %s has no CreatedAt column in its parsed schema", t.Name()))
	}

	cfg := &storeConfig{}
	for _, o := range opts {
		o(cfg)
	}
	newAppendRejectOptions(t, cfg)
	if h.ReadOnly() && !cfg.readOnly {
		panic("store.NewAppend: read-only db handle requires store.WithReadOnly() (or bind a writable instance)")
	}

	// Policy inheritance mirrors store.New for the knobs that exist
	// here. RequirePrincipal / AdminRoles are owner machinery — an
	// append model has no owner column for them to act on.
	pol := h.StorePolicy()
	strictFromPolicy := false
	if !cfg.strictSet {
		cfg.strict = pol.Strict
		strictFromPolicy = pol.Strict
	}
	if !cfg.maxPageSizeSet {
		cfg.maxPageSize = pol.MaxPageSize
	}

	// The model's own `store` tag declaration. The query side works
	// exactly as on full models (AppendOnlyModel contributes created_at
	// through the same LookUpField probe; RID/UpdatedAt simply don't
	// resolve). An update-side declaration is a contradiction — there
	// is no update path for it to feed — so it fails construction.
	tagQuery, tagUpdate, tagged := tagDeclaredFields(t, modelSchema)
	if len(tagUpdate) > 0 {
		panic(fmt.Sprintf("store.NewAppend: %s declares store:\"update\" fields %v but append-only stores have no update path — drop the update tag",
			t.Name(), sortedKeys(tagUpdate)))
	}

	// Build queryFieldMap. Priority mirrors store.New:
	// WithQueryFields > WithAllQueryFields > `store` tags > auto-discover.
	var queryFieldMap map[string]string
	queryExplicit := len(cfg.queryFields) > 0
	queryTagged := tagged && !queryExplicit && !cfg.autoQueryFields
	switch {
	case queryExplicit:
		queryFieldMap = make(map[string]string, len(cfg.queryFields))
		for _, f := range cfg.queryFields {
			queryFieldMap[f] = f
		}
	case queryTagged:
		queryFieldMap = tagQuery
	default:
		var queryCollisions []string
		queryFieldMap, queryCollisions = discoverFields(modelSchema, cfg.queryFieldsExclude)
		if len(queryCollisions) > 0 && logger != nil {
			logger.Warn("store: duplicate json tag names in query fields — last one wins, consider WithQueryFields to disambiguate",
				"model", t.Name(),
				"collisions", strings.Join(queryCollisions, ", "),
			)
		}
	}

	if cfg.strict {
		mode := "strict mode"
		if strictFromPolicy {
			mode = "strict mode (from db.store policy)"
		}
		if !queryExplicit && !queryTagged && !cfg.autoQueryFields && len(queryFieldMap) > 0 {
			panic(fmt.Sprintf("store: %s: %s has auto-discovered query fields %v; declare them with `store` tags, WithQueryFields or WithAllQueryFields", mode, t.Name(), sortedKeys(queryFieldMap)))
		}
	}
	if logger != nil && !queryExplicit && !queryTagged && len(queryFieldMap) > 0 {
		logger.Warn("store: auto-discovered query fields; declare them with `store` tags or WithQueryFields",
			"model", t.Name(), "count", len(queryFieldMap))
	}

	// Apply aliases — query side only; there is no update map for the
	// full store's "whichever map contains the field" rule to consult.
	for f, col := range cfg.aliases {
		if queryFieldMap == nil || queryFieldMap[f] == "" {
			panic(fmt.Sprintf("store.NewAppend: WithColumnAlias(%q, %q): field %q not declared in WithQueryFields", f, col, f))
		}
		queryFieldMap[f] = col
	}

	for field, col := range queryFieldMap {
		if _, err := where.ResolveField(queryFieldMap, field); err != nil {
			panic(fmt.Sprintf("store.NewAppend: query field %q has invalid column %q: %v", field, col, err))
		}
	}

	return &AppendStore[T]{
		h:                h,
		logger:           logger,
		queryFieldMap:    queryFieldMap,
		constraintFields: cfg.constraintFields,
		modelSchema:      modelSchema,
		idColumn:         idField.DBName,
		createdAtColumn:  createdAtField.DBName,
		scopes:           cfg.scopes,
		maxPageSize:      cfg.maxPageSize,
		strict:           cfg.strict,
		readOnly:         cfg.readOnly,
	}
}

// newAppendRejectOptions fails construction for StoreOptions that have
// no effect on an append-only store. A silently ignored option is a
// misconfiguration the operator believes is active — panic instead,
// naming the option. Keep this list in sync when storeConfig grows
// (see the StoreOption doc comment).
func newAppendRejectOptions(t reflect.Type, cfg *storeConfig) {
	reject := func(option, why string) {
		panic(fmt.Sprintf("store.NewAppend: %s: %s has no effect on an append-only store — %s", t.Name(), option, why))
	}
	if len(cfg.updateFields) > 0 {
		reject("WithUpdateFields", "there is no update path")
	}
	if cfg.autoUpdateFields || len(cfg.updateFieldsExclude) > 0 {
		reject("WithAllUpdateFields", "there is no update path")
	}
	if cfg.adminRolesSet {
		reject("WithAdminRoles", "append models have no owner machinery")
	}
	if cfg.requirePrincipalSet {
		reject("WithRequirePrincipal/WithoutRequirePrincipal", "append models have no owner machinery")
	}
	if cfg.noOwnerScope {
		reject("WithoutOwnerScope", "append models have no automatic OwnerScope to disable")
	}
	if cfg.bus != nil {
		reject("WithBus", "entity events bind identity to RID, which append models lack")
	}
	if len(cfg.beforeCreate) > 0 || len(cfg.beforeUpdate) > 0 || len(cfg.beforeDelete) > 0 {
		reject("hooks", "AppendStore registers no hooks")
	}
	if cfg.defaultPageSizeSet {
		reject("WithDefaultPageSize", "it only feeds ListFromQuery, which AppendStore does not provide")
	}
}

// Create inserts a new record. Returns ErrDuplicate on unique
// constraint violation — pair a unique index with WithConstraintFields
// to blame the public field name; INSERT + unique key is the blessed
// idempotency mechanism for append-only tables.
func (s *AppendStore[T]) Create(ctx context.Context, obj *T) error {
	if err := s.rejectWrite("Create"); err != nil {
		return err
	}
	if err := s.effectiveDB(ctx).Create(obj).Error; err != nil {
		return mapErrorWithConstraints(err, s.constraintFields)
	}
	return nil
}

// BatchCreate inserts multiple records in a single transaction.
// Empty slice returns nil (no-op). Single failure rolls back the
// entire batch. Returns ErrDuplicate on unique constraint violation.
func (s *AppendStore[T]) BatchCreate(ctx context.Context, objs []*T) error {
	if err := s.rejectWrite("BatchCreate"); err != nil {
		return err
	}
	if len(objs) == 0 {
		return nil
	}
	// Already inside a context-scoped transaction: write directly so
	// the batch stays atomic with the caller's transaction. Otherwise
	// open one so a mid-batch failure rolls the whole batch back.
	var err error
	if txctx.DB(ctx, s.h) != nil {
		err = s.effectiveDB(ctx).CreateInBatches(objs, createBatchSize).Error
	} else {
		err = s.h.RunInTx(ctx, func(txCtx context.Context) error {
			return s.effectiveDB(txCtx).CreateInBatches(objs, createBatchSize).Error
		})
	}
	if err != nil {
		return mapErrorWithConstraints(err, s.constraintFields)
	}
	return nil
}

// List retrieves records. Zero matches returns a Page with empty Items
// slice. Total is populated only when where.WithCount() is included.
//
// Ordering is always deterministic: without an explicit where.WithOrder
// the result is insertion order (created_at ASC, internal PK ASC); with
// one, the internal PK is appended as a trailing ASC tie-breaker so
// offset pages cannot shuffle rows that share a created_at value. The
// PK never leaves the process — it participates in ORDER BY only.
func (s *AppendStore[T]) List(ctx context.Context, opts ...where.Option) (*Page[T], error) {
	// Enforce max page size; caller options may tighten but not raise.
	if s.maxPageSize > 0 {
		opts = append([]where.Option{where.WithMaxPageSize(s.maxPageSize)}, opts...)
	}

	base, err := s.applyScopes(ctx, s.effectiveDB(ctx).Model(new(T)))
	if err != nil {
		return nil, err
	}
	query, cfg, err := where.Apply(base, s.queryFieldMap, opts)
	if err != nil {
		return nil, mapQueryError(err)
	}

	var total int64
	if cfg.Count {
		total, err = s.count(ctx, opts)
		if err != nil {
			return nil, err
		}
	}

	if !cfg.HasOrder {
		query = query.Order(clause.OrderByColumn{Column: clause.Column{Name: s.createdAtColumn}})
	}
	query = query.Order(clause.OrderByColumn{Column: clause.Column{Name: s.idColumn}})

	var items []T
	if err := query.Find(&items).Error; err != nil {
		return nil, mapErrorWithConstraints(err, s.constraintFields)
	}
	if items == nil {
		items = []T{}
	}

	meta := cfg.PageInfo()
	if cfg.Count {
		meta.HasMore = int64(meta.Offset)+int64(len(items)) < total
	}
	return &Page[T]{Items: items, Total: total, Meta: meta}, nil
}

// count is List's WithCount branch: scopes, then filters only —
// pagination/order stripped so the total is unaffected.
func (s *AppendStore[T]) count(ctx context.Context, opts []where.Option) (int64, error) {
	base, err := s.applyScopes(ctx, s.effectiveDB(ctx).Model(new(T)))
	if err != nil {
		return 0, err
	}
	q, err := where.ApplyFiltersOnly(base, s.queryFieldMap, opts)
	if err != nil {
		return 0, mapQueryError(err)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return 0, mapErrorWithConstraints(err, s.constraintFields)
	}
	return total, nil
}

// effectiveDB returns the *gorm.DB for the current operation: the
// context's transaction when one is active on this handle (db.RunInTx
// propagation), else the root pool. AppendStore has no Tx clones, so
// there is no pinned-transaction middle case.
func (s *AppendStore[T]) effectiveDB(ctx context.Context) *gorm.DB {
	if tx := txctx.DB(ctx, s.h); tx != nil {
		return tx.WithContext(ctx)
	}
	return s.h.Unsafe(ctx)
}

func (s *AppendStore[T]) rejectWrite(op string) error {
	if !s.readOnly {
		return nil
	}
	return fmt.Errorf("store: %s: %w", op, db.ErrReadOnly)
}

// applyScopes runs all registered ScopeFuncs against the given DB.
func (s *AppendStore[T]) applyScopes(ctx context.Context, gdb *gorm.DB) (*gorm.DB, error) {
	for _, scope := range s.scopes {
		var err error
		gdb, err = scope(ctx, gdb)
		if err != nil {
			return nil, err
		}
		if gdb == nil {
			return nil, fmt.Errorf("store: scope returned nil *gorm.DB without error")
		}
	}
	return gdb, nil
}
