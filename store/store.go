// Package store provides a generic CRUD store backed by GORM.
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"gorm.io/gorm"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/rid"
	"github.com/zynthara/chok/store/where"
)

// Sentinel errors — business code uses these without importing GORM.
var (
	ErrNotFound          = errors.New("store: record not found")
	ErrStaleVersion      = errors.New("store: version conflict, row was modified by another request")
	ErrMissingColumns    = errors.New("store: update called without columns")
	ErrMissingConditions = errors.New("store: operation called without conditions")
	ErrDuplicate         = errors.New("store: duplicate entry")

	// ErrUnknownUpdateField indicates the field name is not in the update whitelist.
	// This is a programming error (code passes a wrong field constant), not client input.
	ErrUnknownUpdateField = errors.New("store: unknown update field")

	// ErrUpdateFieldsNotConfigured indicates WithUpdateFields was not called.
	// This is a programming error (Store misconfigured), not client input.
	ErrUpdateFieldsNotConfigured = errors.New("store: update fields not configured")
)

// ScopeFunc applies context-derived query conditions directly to *gorm.DB.
// It bypasses the WithQueryFields whitelist (scope is an internal security
// constraint, not a client-facing query field).
// Returns error to enforce fail-closed: unauthenticated contexts must return
// an error rather than silently skipping the filter.
// If the error should map to a specific HTTP status (e.g. 401), return *apierr.Error.
type ScopeFunc func(ctx context.Context, db *gorm.DB) (*gorm.DB, error)

// Store is a generic CRUD store for models embedding db.Model.
type Store[T db.Modeler] struct {
	db              *gorm.DB
	logger          log.Logger
	queryFieldMap   map[string]string // filter + order
	updateFieldMap  map[string]string // update SET columns
	soft            bool              // true if T embeds SoftDeleteModel
	scopes          []ScopeFunc
	defaultPageSize int // default page size for ListFromQuery (0 = where.DefaultPageSize)
}

// StoreOption configures a Store.
type StoreOption func(*storeConfig)

type storeConfig struct {
	queryFields         []string
	updateFields        []string
	aliases             map[string]string
	scopes              []ScopeFunc
	defaultPageSize     int
	autoQueryFields     bool
	queryFieldsExclude  []string
	updateFieldsExclude []string
	noOwnerScope        bool
}

// WithScope registers a scope function applied to every read/update/delete
// query (not Create/BatchCreate). See ScopeFunc for semantics.
// Panics if scope is nil (configuration error caught at startup).
func WithScope(scope ScopeFunc) StoreOption {
	if scope == nil {
		panic("store: WithScope scope must not be nil")
	}
	return func(c *storeConfig) {
		c.scopes = append(c.scopes, scope)
	}
}

// WithoutOwnerScope disables automatic OwnerScope for models embedding db.Owned.
// Use this when an owned model should be visible to all users.
func WithoutOwnerScope() StoreOption {
	return func(c *storeConfig) { c.noOwnerScope = true }
}

// WithQueryFields declares which fields are queryable/sortable. Column name defaults to the field name.
// Fields not declared here are rejected by WithFilter/WithOrder.
func WithQueryFields(fields ...string) StoreOption {
	return func(c *storeConfig) {
		c.queryFields = append(c.queryFields, fields...)
	}
}

// WithUpdateFields declares which fields are updatable. Column name defaults to the field name.
// Fields not declared here are rejected by Update.
func WithUpdateFields(fields ...string) StoreOption {
	return func(c *storeConfig) {
		c.updateFields = append(c.updateFields, fields...)
	}
}

// WithColumnAlias maps a public field name to a different database column (e.g. "id" → "rid").
// The field must be declared via WithQueryFields or WithUpdateFields; otherwise panics at Store construction.
func WithColumnAlias(field, column string) StoreOption {
	return func(c *storeConfig) {
		if c.aliases == nil {
			c.aliases = make(map[string]string)
		}
		c.aliases[field] = column
	}
}

// WithAllQueryFields auto-discovers queryable fields from the model's JSON tags.
// Fields tagged json:"-" are excluded (internal fields like PasswordHash, OwnerID).
// Optional exclude list removes specific fields by JSON name:
//
//	store.WithAllQueryFields()                // all public fields
//	store.WithAllQueryFields("content")       // all except content
func WithAllQueryFields(exclude ...string) StoreOption {
	return func(c *storeConfig) {
		c.autoQueryFields = true
		c.queryFieldsExclude = exclude
	}
}

// WithAllUpdateFields auto-discovers updatable fields from the model's JSON tags.
// Base model fields (id, version, created_at, updated_at) and json:"-" fields
// are excluded. Text/blob fields are NOT excluded (updating content is normal).
// Optional exclude list removes additional fields by JSON name.
func WithAllUpdateFields(exclude ...string) StoreOption {
	return func(c *storeConfig) { c.updateFieldsExclude = exclude }
}

// WithDefaultPageSize sets the default page size for ListFromQuery
// when the client does not provide a "size" parameter. Default is 20.
func WithDefaultPageSize(size int) StoreOption {
	return func(c *storeConfig) { c.defaultPageSize = size }
}

// New creates a Store. T must be a struct type (not a pointer).
// Panics if T is a pointer type or has an invalid RIDPrefix.
func New[T db.Modeler](gdb *gorm.DB, logger log.Logger, opts ...StoreOption) *Store[T] {
	t := reflect.TypeOf((*T)(nil)).Elem()

	// Reject pointer types: store.New[*User] is a programming error.
	if t.Kind() == reflect.Ptr {
		panic(fmt.Sprintf("store.New: T must be a struct type, got pointer %s", t))
	}

	// Validate model metadata (RIDPrefix, etc.).
	model := reflect.New(t).Interface()
	if err := db.ValidateModel(model); err != nil {
		panic(fmt.Sprintf("store.New: %v", err))
	}

	cfg := &storeConfig{}
	for _, o := range opts {
		o(cfg)
	}

	// Build queryFieldMap.
	// Priority: WithQueryFields (explicit) > WithAllQueryFields (explicit auto) > default auto-discover.
	var queryFieldMap map[string]string
	if len(cfg.queryFields) > 0 {
		queryFieldMap = make(map[string]string, len(cfg.queryFields))
		for _, f := range cfg.queryFields {
			queryFieldMap[f] = f
		}
	} else {
		queryFieldMap = discoverFields(t, cfg.queryFieldsExclude)
	}

	// Build updateFieldMap.
	// Priority: WithUpdateFields (explicit) > default auto-discover.
	var updateFieldMap map[string]string
	if len(cfg.updateFields) > 0 {
		updateFieldMap = make(map[string]string, len(cfg.updateFields))
		for _, f := range cfg.updateFields {
			updateFieldMap[f] = f
		}
	} else {
		updateFieldMap = discoverUpdateFields(t, cfg.updateFieldsExclude)
	}

	// Apply aliases to whichever maps contain the field.
	for f, col := range cfg.aliases {
		inQuery := queryFieldMap != nil && queryFieldMap[f] != ""
		inUpdate := updateFieldMap != nil && updateFieldMap[f] != ""
		if !inQuery && !inUpdate {
			panic(fmt.Sprintf("store.New: WithColumnAlias(%q, %q): field %q not declared in WithQueryFields or WithUpdateFields", f, col, f))
		}
		if inQuery {
			queryFieldMap[f] = col
		}
		if inUpdate {
			updateFieldMap[f] = col
		}
	}

	// Auto-register "id" → "rid" alias: db.Model exposes RID as JSON "id",
	// so this mapping is always correct. Users can override via explicit WithColumnAlias.
	if _, explicit := cfg.aliases["id"]; !explicit {
		if queryFieldMap != nil && queryFieldMap["id"] == "id" {
			queryFieldMap["id"] = "rid"
		}
		if updateFieldMap != nil && updateFieldMap["id"] == "id" {
			updateFieldMap["id"] = "rid"
		}
	}

	// Auto-detect OwnerScope for models embedding db.Owned.
	scopes := cfg.scopes
	if !cfg.noOwnerScope {
		if _, ok := model.(db.OwnerAccessor); ok {
			scopes = append([]ScopeFunc{OwnerScope(defaultAdminRoles...)}, scopes...)
		}
	}

	return &Store[T]{
		db:              gdb,
		logger:          logger,
		queryFieldMap:   queryFieldMap,
		updateFieldMap:  updateFieldMap,
		soft:            db.IsSoftDeleteModel(model),
		scopes:          scopes,
		defaultPageSize: cfg.defaultPageSize,
	}
}

// Create inserts a new record.
// If the model embeds db.Owned and OwnerID is empty, it is auto-filled
// from the authenticated principal's Subject.
// Returns ErrDuplicate on unique constraint violation.
func (s *Store[T]) Create(ctx context.Context, obj *T) error {
	fillOwner(ctx, obj)
	if err := s.db.WithContext(ctx).Create(obj).Error; err != nil {
		return mapError(err)
	}
	return nil
}

// BatchCreate inserts multiple records in a single transaction.
// Empty slice returns nil (no-op). Single failure rolls back the entire batch.
// If the model embeds db.Owned, OwnerID is auto-filled from the principal.
// Returns ErrDuplicate on unique constraint violation.
func (s *Store[T]) BatchCreate(ctx context.Context, objs []*T) error {
	if len(objs) == 0 {
		return nil
	}
	for _, obj := range objs {
		fillOwner(ctx, obj)
	}
	err := db.Transaction(ctx, s.db, func(tx *gorm.DB) error {
		for _, obj := range objs {
			if err := tx.Create(obj).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return mapError(err)
}

// UpdateOne performs an optimistic-lock update on a single record identified by RID.
// WHERE rid=? AND version=? then increments version.
// fields are public field names (resolved via WithUpdateFields); at least one is required.
// Requires WithUpdateFields to be configured.
// Returns ErrMissingColumns, ErrNotFound, or ErrStaleVersion.
func (s *Store[T]) UpdateOne(ctx context.Context, obj *T, fields ...string) error {
	if len(fields) == 0 {
		return ErrMissingColumns
	}

	// Resolve public field names to DB columns.
	columns := make([]string, 0, len(fields))
	for _, f := range fields {
		col, err := s.resolveUpdateColumn(f)
		if err != nil {
			return err
		}
		columns = append(columns, col)
	}

	// Apply scopes before touching the in-memory object, so a scope error
	// doesn't leave the version incremented.
	q, err := s.applyScopes(ctx, s.db.WithContext(ctx))
	if err != nil {
		return err
	}

	// Extract Model from the concrete object.
	m := extractModel(obj)

	// Always update version.
	updateCols := append(columns, "version")
	oldVersion := m.Version
	m.Version++

	result := q.
		Model(obj).
		Where("rid = ? AND version = ?", m.RID, oldVersion).
		Select(updateCols).
		Updates(obj)

	if result.Error != nil {
		m.Version = oldVersion // rollback in-memory
		return mapError(result.Error)
	}

	if result.RowsAffected == 0 {
		m.Version = oldVersion // rollback in-memory
		// Distinguish not-found from stale version.
		var count int64
		q2, err := s.applyScopes(ctx, s.db.WithContext(ctx))
		if err != nil {
			return err
		}
		if err := q2.Model(obj).Where("rid = ?", m.RID).Count(&count).Error; err != nil {
			return mapError(err)
		}
		if count == 0 {
			return ErrNotFound
		}
		return ErrStaleVersion
	}

	return nil
}

// GetOne retrieves a single record by RID.
// Returns ErrNotFound if no record matches.
func (s *Store[T]) GetOne(ctx context.Context, resourceID string) (*T, error) {
	q, err := s.applyScopes(ctx, s.db.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	var obj T
	if err := q.Where("rid = ?", resourceID).First(&obj).Error; err != nil {
		return nil, mapError(err)
	}
	return &obj, nil
}

// Get retrieves a single record. Returns ErrMissingConditions if no filter
// conditions are present (ordering/pagination alone is not sufficient).
// Returns ErrNotFound if no record matches.
func (s *Store[T]) Get(ctx context.Context, opts ...where.Option) (*T, error) {
	base, err := s.applyScopes(ctx, s.db.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	query, cfg, err := where.Apply(base, s.queryFieldMap, opts)
	if err != nil {
		return nil, mapQueryError(err)
	}
	if !cfg.HasFilter {
		return nil, ErrMissingConditions
	}

	var obj T
	if err := query.First(&obj).Error; err != nil {
		return nil, mapError(err)
	}
	return &obj, nil
}

// List retrieves records. Zero matches returns ([]T{}, total, nil).
// total = -1 by default (no COUNT). Use where.WithCount() to get actual total.
func (s *Store[T]) List(ctx context.Context, opts ...where.Option) ([]T, int64, error) {
	base, err := s.applyScopes(ctx, s.db.WithContext(ctx).Model(new(T)))
	if err != nil {
		return nil, 0, err
	}
	query, cfg, err := where.Apply(base, s.queryFieldMap, opts)
	if err != nil {
		return nil, 0, mapQueryError(err)
	}

	var total int64 = -1
	if cfg.Count {
		// COUNT with filters only — pagination/order stripped so total is unaffected.
		countBase, err := s.applyScopes(ctx, s.db.WithContext(ctx).Model(new(T)))
		if err != nil {
			return nil, 0, err
		}
		countQuery, err := where.ApplyFiltersOnly(countBase, s.queryFieldMap, opts)
		if err != nil {
			return nil, 0, mapQueryError(err)
		}
		if err := countQuery.Count(&total).Error; err != nil {
			return nil, 0, mapError(err)
		}
	}

	var items []T
	if err := query.Find(&items).Error; err != nil {
		return nil, 0, mapError(err)
	}

	// Guarantee non-nil slice for JSON serialization.
	if items == nil {
		items = []T{}
	}

	return items, total, nil
}

// ListFromQuery parses URL query parameters and returns a paginated list.
// Supported query params: page, size, order (field:asc|desc), and any field
// declared via WithQueryFields as an equality filter.
// Fixed filters should be applied via WithScope at Store construction time.
func (s *Store[T]) ListFromQuery(ctx context.Context, query url.Values) ([]T, int64, error) {
	opts, err := where.FromQuery(query, s.queryFieldMap, s.defaultPageSize)
	if err != nil {
		return nil, 0, mapQueryError(err)
	}
	return s.List(ctx, opts...)
}

// DeleteMany removes records matching the conditions. May affect multiple rows.
// Returns ErrMissingConditions if no filter conditions are present.
// Idempotent: zero matches returns nil.
// SoftDeleteModel → soft-delete + auto DeleteToken; Model → physical delete.
func (s *Store[T]) DeleteMany(ctx context.Context, opts ...where.Option) error {
	base, err := s.applyScopes(ctx, s.db.WithContext(ctx))
	if err != nil {
		return err
	}
	query, cfg, err := where.Apply(base, s.queryFieldMap, opts)
	if err != nil {
		return mapQueryError(err)
	}
	if !cfg.HasFilter {
		return ErrMissingConditions
	}

	if s.soft {
		// Soft delete: set DeletedAt + DeleteToken.
		return query.Model(new(T)).Updates(map[string]any{
			"deleted_at":   gorm.Expr("CURRENT_TIMESTAMP"),
			"delete_token": rid.NewRaw(),
		}).Error
	}

	return query.Delete(new(T)).Error
}

// DeleteOne deletes a single record by RID with optional optimistic locking.
// version > 0: WHERE rid=? AND version=? → RowsAffected==0 → ErrNotFound / ErrStaleVersion.
// version == 0: WHERE rid=? → RowsAffected==0 → nil (idempotent).
// SoftDeleteModel → soft-delete + auto DeleteToken; Model → physical delete.
func (s *Store[T]) DeleteOne(ctx context.Context, resourceID string, version int) error {
	q, err := s.applyScopes(ctx, s.db.WithContext(ctx))
	if err != nil {
		return err
	}
	query := q.Where("rid = ?", resourceID)
	if version > 0 {
		query = query.Where("version = ?", version)
	}

	var result *gorm.DB
	if s.soft {
		result = query.Model(new(T)).Updates(map[string]any{
			"deleted_at":   gorm.Expr("CURRENT_TIMESTAMP"),
			"delete_token": rid.NewRaw(),
		})
	} else {
		result = query.Delete(new(T))
	}

	if result.Error != nil {
		return mapError(result.Error)
	}

	if result.RowsAffected == 0 {
		if version > 0 {
			// Distinguish not-found from stale version.
			q2, err := s.applyScopes(ctx, s.db.WithContext(ctx))
			if err != nil {
				return err
			}
			var count int64
			if err := q2.Model(new(T)).Where("rid = ?", resourceID).Count(&count).Error; err != nil {
				return mapError(err)
			}
			if count == 0 {
				return ErrNotFound
			}
			return ErrStaleVersion
		}
		return nil // idempotent when version == 0
	}

	return nil
}

// Transaction runs fn inside a transaction scoped to this Store.
func (s *Store[T]) Transaction(ctx context.Context, fn func(tx *Store[T]) error) error {
	return db.Transaction(ctx, s.db, func(gormTx *gorm.DB) error {
		txStore := s.withDB(gormTx)
		return fn(txStore)
	})
}

// WithTx creates a new Store sharing config but using an external transaction.
// Useful for cross-Store transactions.
func (s *Store[T]) WithTx(tx *gorm.DB) *Store[T] {
	return s.withDB(tx)
}

// DB returns the underlying *gorm.DB as an escape hatch for complex queries.
func (s *Store[T]) DB() *gorm.DB {
	return s.db
}

func (s *Store[T]) withDB(gdb *gorm.DB) *Store[T] {
	return &Store[T]{
		db:              gdb,
		logger:          s.logger,
		queryFieldMap:   s.queryFieldMap,
		updateFieldMap:  s.updateFieldMap,
		soft:            s.soft,
		scopes:          s.scopes,
		defaultPageSize: s.defaultPageSize,
	}
}

// applyScopes runs all registered ScopeFuncs against the given DB.
func (s *Store[T]) applyScopes(ctx context.Context, db *gorm.DB) (*gorm.DB, error) {
	for _, scope := range s.scopes {
		var err error
		db, err = scope(ctx, db)
		if err != nil {
			return nil, err
		}
		if db == nil {
			return nil, fmt.Errorf("store: scope returned nil *gorm.DB without error")
		}
	}
	return db, nil
}

// resolveUpdateColumn maps a public field name to a DB column for Update.
// Requires WithUpdateFields to be configured.
func (s *Store[T]) resolveUpdateColumn(field string) (string, error) {
	if s.updateFieldMap == nil {
		return "", fmt.Errorf("%w, cannot resolve field %q", ErrUpdateFieldsNotConfigured, field)
	}
	col, ok := s.updateFieldMap[field]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownUpdateField, field)
	}
	return col, nil
}

// mapQueryError classifies query-building errors:
// - Client errors (bad params, unknown fields) → apierr.ErrInvalidArgument (400)
// - Server errors (unconfigured field set) → pass through (500)
func mapQueryError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, where.ErrInvalidParam) || errors.Is(err, where.ErrUnknownField) {
		return apierr.ErrInvalidArgument.WithMessage(err.Error())
	}
	// ErrFieldNotConfigured is a server-side bug — pass through as 500.
	return err
}

// extractModel gets the *db.Model from a concrete model struct.
func extractModel(obj any) *db.Model {
	v := reflect.ValueOf(obj)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	// Try direct Model field.
	if f := v.FieldByName("Model"); f.IsValid() && f.CanAddr() {
		if m, ok := f.Addr().Interface().(*db.Model); ok {
			return m
		}
	}
	// Try SoftDeleteModel.Model.
	if f := v.FieldByName("SoftDeleteModel"); f.IsValid() {
		if mf := f.FieldByName("Model"); mf.IsValid() && mf.CanAddr() {
			if m, ok := mf.Addr().Interface().(*db.Model); ok {
				return m
			}
		}
	}
	panic("store: cannot extract db.Model from object")
}

// mapError translates GORM errors to store sentinel errors.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if isDuplicateError(err) {
		return ErrDuplicate
	}
	return err
}

// Base model fields that must never be updated.
var baseModelExclude = map[string]bool{
	"id": true, "version": true, "created_at": true, "updated_at": true,
}

// discoverFields builds a queryFieldMap from JSON tags.
// Excludes json:"-", text/blob columns, and user-specified names.
func discoverFields(t reflect.Type, exclude []string) map[string]string {
	ex := toSet(exclude)
	result := make(map[string]string)
	scanJSONFields(t, ex, true, result)
	return result
}

// discoverUpdateFields builds an updateFieldMap from JSON tags.
// Excludes json:"-", base model fields (id/version/timestamps), and user-specified names.
// Does NOT exclude text/blob (updating content is normal).
func discoverUpdateFields(t reflect.Type, exclude []string) map[string]string {
	ex := toSet(exclude)
	for k := range baseModelExclude {
		ex[k] = true
	}
	result := make(map[string]string)
	scanJSONFields(t, ex, false, result)
	return result
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

func scanJSONFields(t reflect.Type, exclude map[string]bool, skipLarge bool, out map[string]string) {
	for i := range t.NumField() {
		f := t.Field(i)

		// Recurse into embedded structs.
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				scanJSONFields(ft, exclude, skipLarge, out)
			}
			continue
		}

		// Parse json tag.
		jsonTag := f.Tag.Get("json")
		if jsonTag == "" || jsonTag == "-" {
			continue
		}
		name, _, _ := strings.Cut(jsonTag, ",")
		if name == "-" || name == "" {
			continue
		}
		if exclude[name] {
			continue
		}
		if skipLarge && isLargeColumnType(f) {
			continue
		}

		col := gormColumnName(f)
		out[name] = col
	}
}

// isLargeColumnType returns true for fields that are text/blob types,
// detected from gorm:"type:text/blob/..." tag or Go []byte type.
func isLargeColumnType(f reflect.StructField) bool {
	// Go type: []byte → binary data.
	if f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Uint8 {
		return true
	}
	// gorm type tag.
	gormTag := strings.ToLower(f.Tag.Get("gorm"))
	for _, part := range strings.Split(gormTag, ";") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "type:") {
			continue
		}
		typ := strings.TrimPrefix(part, "type:")
		switch typ {
		case "text", "mediumtext", "longtext", "blob", "mediumblob", "longblob", "bytea":
			return true
		}
	}
	return false
}

// gormColumnName returns the DB column for a struct field:
// gorm:"column:xxx" tag if present, otherwise snake_case of the field name.
func gormColumnName(f reflect.StructField) string {
	for _, part := range strings.Split(f.Tag.Get("gorm"), ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "column:") {
			return strings.TrimPrefix(part, "column:")
		}
	}
	return toSnakeCase(f.Name)
}

func toSnakeCase(s string) string {
	var buf []byte
	for i, c := range s {
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				buf = append(buf, '_')
			}
			buf = append(buf, byte(c)+32)
		} else {
			buf = append(buf, byte(c))
		}
	}
	return string(buf)
}

// isDuplicateError detects duplicate key errors across MySQL, SQLite, and PostgreSQL
// using string matching. We intentionally avoid driver-specific type checks (e.g.
// *mysql.MySQLError) to prevent a compile-time dependency on database drivers that
// the user may not be using.
func isDuplicateError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "unique_violation") ||
		strings.Contains(msg, "constraint failed")
}
