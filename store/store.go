// Package store provides a generic CRUD store backed by GORM.
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/store/where"
)

// Sentinel errors — business code uses these without importing GORM.
// errors.Is works with both the sentinels and the structured *XxxError types
// (which implement Is(target) bool).
var (
	ErrNotFound          = errors.New("store: record not found")
	ErrStaleVersion      = errors.New("store: version conflict, row was modified by another request")
	ErrMissingColumns    = errors.New("store: update called without columns")
	ErrMissingConditions = errors.New("store: operation called without conditions")
	// ErrDegenerateConditions means the locator's filter is present but
	// collapses to match-nothing (e.g. WithFilterIn over an empty slice,
	// WithFilter with a nil value). Distinguishing this from "no filter"
	// lets Update/Delete callers surface a precise client-input error
	// rather than silently succeeding with zero rows affected.
	ErrDegenerateConditions = errors.New("store: filter matches nothing")
	ErrDuplicate            = errors.New("store: duplicate entry")

	// ErrUnknownUpdateField indicates the field name is not in the update whitelist.
	// This is a programming error (code passes a wrong field constant), not client input.
	ErrUnknownUpdateField = errors.New("store: unknown update field")

	// ErrUpdateFieldsNotConfigured indicates WithUpdateFields was not called.
	// This is a programming error (Store misconfigured), not client input.
	ErrUpdateFieldsNotConfigured = errors.New("store: update fields not configured")

	// ErrUpsertScoped indicates Upsert was called on a Store that has
	// scopes registered. SQL INSERT ... ON CONFLICT DO UPDATE does not
	// honour WHERE-based scope conditions on the conflict update path,
	// so a conflict on a globally unique column could silently bypass
	// tenant isolation or other scope invariants. Use Create + Update,
	// or s.DB() as an escape hatch if you understand the implications.
	ErrUpsertScoped = errors.New("store: upsert is not safe with scoped stores (scopes are ignored on conflict update); use separate Create + Update")
)

// --- Structured error types --------------------------------------------------
//
// These carry diagnostic context (locator, version, DB detail) while remaining
// compatible with the sentinel errors via Is(). Callers can use errors.Is for
// branching and errors.As for extracting context:
//
//	if errors.Is(err, store.ErrNotFound) { ... }   // still works
//	var nfe *store.NotFoundError
//	if errors.As(err, &nfe) { log("locator", nfe.Locator) }

// NotFoundError carries context about what was not found.
//
// Locator is a redacted representation safe for error.Error() output
// (numeric IDs are printed as "id" without the value to avoid leaking
// internal primary keys to logs that might propagate to clients). For
// server-side diagnostics, RID and IDValue are populated when the
// locator was store.RID(...) / store.ID(...) respectively — callers can
// use errors.As and type-read these fields without parsing Locator.
//
// HasRID / HasID flags distinguish "locator was RID/ID" from zero-value
// ambiguity: store.RID("") or store.ID(0) are unusual but the error
// path should still signal which kind of lookup was attempted.
type NotFoundError struct {
	Locator string // e.g. "rid:usr_abc", "id", "where"
	RID     string // populated when by=store.RID(...); see HasRID
	IDValue uint   // populated when by=store.ID(...); see HasID
	HasRID  bool
	HasID   bool
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("store: record not found (%s)", e.Locator)
}

// Is makes errors.Is(err, ErrNotFound) return true.
func (e *NotFoundError) Is(target error) bool { return target == ErrNotFound }

// VersionConflictError carries the locator and the stale version that was
// supplied. Useful for logging and API error detail.
//
// Locator is the redacted display form; RID / IDValue carry the concrete
// key (populated when the locator was store.RID / store.ID) for
// server-side diagnostics via errors.As. HasRID / HasID mirror the
// NotFoundError flags.
type VersionConflictError struct {
	Locator string
	Version int // the stale version the caller supplied
	RID     string
	IDValue uint
	HasRID  bool
	HasID   bool
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("store: version conflict (%s, version=%d)", e.Locator, e.Version)
}

// Is makes errors.Is(err, ErrStaleVersion) return true.
func (e *VersionConflictError) Is(target error) bool { return target == ErrStaleVersion }

// DuplicateEntryError carries the raw database error detail so callers can
// report which constraint was violated.
type DuplicateEntryError struct {
	Detail string // driver-specific constraint/message
}

func (e *DuplicateEntryError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("store: duplicate entry (%s)", e.Detail)
	}
	return "store: duplicate entry"
}

// Is makes errors.Is(err, ErrDuplicate) return true.
func (e *DuplicateEntryError) Is(target error) bool { return target == ErrDuplicate }

// ScopeFunc applies context-derived query conditions directly to *gorm.DB.
// It bypasses the WithQueryFields whitelist (scope is an internal security
// constraint, not a client-facing query field).
// Returns error to enforce fail-closed: unauthenticated contexts must return
// an error rather than silently skipping the filter.
// If the error should map to a specific HTTP status (e.g. 401), return *apierr.Error.
type ScopeFunc func(ctx context.Context, db *gorm.DB) (*gorm.DB, error)

// Store is a generic CRUD store for models embedding db.Model.
type Store[T db.Modeler] struct {
	db               *gorm.DB
	logger           log.Logger
	queryFieldMap    map[string]string // filter + order
	updateFieldMap   map[string]string // update SET columns
	soft             bool              // true if T embeds SoftDeleteModel
	scopes           []ScopeFunc
	defaultPageSize  int  // default page size for ListFromQuery (0 = where.DefaultPageSize)
	maxPageSize      int  // max page size (0 = unlimited)
	strict           bool // strict mode: reject auto-discovered fields, unknown params
	requirePrincipal bool // fail-closed: Create/Upsert on Owned models reject no-principal contexts
	hooks            hooks[T]
}

// hooks holds the registered lifecycle callbacks.
//
// Before-hooks run inside the operation, before the DB write. Returning an
// error aborts the operation — the caller sees the hook's error, no row is
// written.
//
// After-hooks are fire-and-forget notifications: they run after a successful
// DB write and cannot affect the caller's result. Typical uses: audit
// logging, cache invalidation, async event publishing.
type hooks[T any] struct {
	beforeCreate []func(ctx context.Context, obj *T) error
	beforeUpdate []func(ctx context.Context, loc Locator, changes Changes) error
	beforeDelete []func(ctx context.Context, loc Locator) error
	afterCreate  []func(ctx context.Context, obj *T)
	afterUpdate  []func(ctx context.Context, loc Locator, changes Changes)
	afterDelete  []func(ctx context.Context, loc Locator)
}

// StoreOption configures a Store.
type StoreOption func(*storeConfig)

type storeConfig struct {
	queryFields         []string
	updateFields        []string
	aliases             map[string]string
	scopes              []ScopeFunc
	defaultPageSize     int
	maxPageSize         int // 0 = unlimited
	autoQueryFields     bool
	autoUpdateFields    bool
	queryFieldsExclude  []string
	updateFieldsExclude []string
	noOwnerScope        bool
	strict              bool  // when true: reject unknown query params, require explicit whitelist
	requirePrincipal    bool  // when true: Create/Upsert on Owned models reject no-principal contexts
	beforeCreate        []any // []func(ctx, *T) error — stored as any to avoid generic storeConfig
	beforeUpdate        []any // []func(ctx, Locator, Changes) error
	beforeDelete        []any // []func(ctx, Locator) error
	afterCreate         []any // []func(ctx, *T) — fire-and-forget, no error return
	afterUpdate         []any // []func(ctx, Locator, Changes)
	afterDelete         []any // []func(ctx, Locator)
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

// WithStrict enables strict mode for production safety:
//   - Auto-discovered query/update fields are rejected at construction
//     time unless the caller explicitly opts in via WithAllQueryFields /
//     WithAllUpdateFields. Without either, strict construction panics
//     (prevents accidental "all fields queryable" exposure).
//   - ListFromQuery rejects unknown query parameters with
//     apierr.ErrInvalidArgument instead of silently dropping them.
//
// Intended for production configs where the implicit "discover from JSON tags"
// behavior is too permissive.
func WithStrict() StoreOption {
	return func(c *storeConfig) { c.strict = true }
}

// WithRequirePrincipal makes Create / BatchCreate / Upsert reject contexts
// without an authenticated principal when the model embeds db.Owned. This
// is fail-closed behaviour — safer for HTTP paths where a missing Authn
// middleware would otherwise let a client set OwnerID freely.
//
// Background jobs and tests that legitimately write Owned rows without a
// principal must either:
//   - Not enable this option on those stores, or
//   - Attach a system principal to ctx via auth.WithPrincipal before Create.
//
// Non-Owned models are unaffected.
func WithRequirePrincipal() StoreOption {
	return func(c *storeConfig) { c.requirePrincipal = true }
}

// WithMaxPageSize sets a hard cap on page size for List / ListFromQuery.
// Requests exceeding this limit are silently clamped. Zero disables the cap.
func WithMaxPageSize(n int) StoreOption {
	return func(c *storeConfig) { c.maxPageSize = n }
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
//
// In strict mode (WithStrict), calling this option signals explicit consent
// to auto-discover so construction does not panic. Without this option,
// strict mode requires WithUpdateFields.
func WithAllUpdateFields(exclude ...string) StoreOption {
	return func(c *storeConfig) {
		c.autoUpdateFields = true
		c.updateFieldsExclude = exclude
	}
}

// WithDefaultPageSize sets the default page size for ListFromQuery
// when the client does not provide a "size" parameter. Default is 20.
func WithDefaultPageSize(size int) StoreOption {
	return func(c *storeConfig) { c.defaultPageSize = size }
}

// WithBeforeCreate registers a callback that runs before a Create writes to
// the database. Returning an error aborts the Create — no row is written and
// the caller sees the hook's error. Multiple callbacks run in registration
// order; the first error short-circuits.
//
// Typical uses: cross-field validation, value normalisation, permission
// checks that can't be expressed as scopes.
func WithBeforeCreate[T db.Modeler](fn func(ctx context.Context, obj *T) error) StoreOption {
	return func(c *storeConfig) {
		c.beforeCreate = append(c.beforeCreate, any(fn))
	}
}

// WithBeforeUpdate registers a callback that runs before an Update writes
// to the database. Returning an error aborts the Update.
func WithBeforeUpdate(fn func(ctx context.Context, loc Locator, changes Changes) error) StoreOption {
	return func(c *storeConfig) {
		c.beforeUpdate = append(c.beforeUpdate, any(fn))
	}
}

// WithBeforeDelete registers a callback that runs before a Delete writes
// to the database. Returning an error aborts the Delete.
func WithBeforeDelete(fn func(ctx context.Context, loc Locator) error) StoreOption {
	return func(c *storeConfig) {
		c.beforeDelete = append(c.beforeDelete, any(fn))
	}
}

// WithAfterCreate registers a fire-and-forget callback that runs after a
// successful Create. The row is already committed — the callback cannot
// affect the caller's result. Multiple callbacks run in registration order.
//
// Typical uses: audit logging, cache invalidation, async event publishing.
func WithAfterCreate[T db.Modeler](fn func(ctx context.Context, obj *T)) StoreOption {
	return func(c *storeConfig) {
		c.afterCreate = append(c.afterCreate, any(fn))
	}
}

// WithAfterUpdate registers a fire-and-forget callback that runs after a
// successful Update. The row is already committed.
func WithAfterUpdate(fn func(ctx context.Context, loc Locator, changes Changes)) StoreOption {
	return func(c *storeConfig) {
		c.afterUpdate = append(c.afterUpdate, any(fn))
	}
}

// WithAfterDelete registers a fire-and-forget callback that runs after a
// successful Delete. The row is already committed.
func WithAfterDelete(fn func(ctx context.Context, loc Locator)) StoreOption {
	return func(c *storeConfig) {
		c.afterDelete = append(c.afterDelete, any(fn))
	}
}

// Submitter is the interface for async task dispatch (satisfied by
// *scheduler.Pool). Kept minimal to avoid importing the scheduler package.
type Submitter interface {
	SubmitFunc(ctx context.Context, name string, fn func(context.Context) error) error
}

// WithAsyncAfterCreate wraps fn in a Pool.SubmitFunc call so the hook runs
// asynchronously in the Pool's worker goroutines instead of blocking the
// request. The pool must be initialized before the Store is used.
func WithAsyncAfterCreate[T db.Modeler](pool Submitter, fn func(ctx context.Context, obj *T)) StoreOption {
	syncFn := func(ctx context.Context, obj *T) {
		objCopy := *obj // shallow copy to avoid data race
		_ = pool.SubmitFunc(ctx, "after_create", func(taskCtx context.Context) error {
			fn(taskCtx, &objCopy)
			return nil
		})
	}
	return func(c *storeConfig) {
		c.afterCreate = append(c.afterCreate, any(syncFn))
	}
}

// WithAsyncAfterUpdate wraps fn in a Pool.SubmitFunc call.
func WithAsyncAfterUpdate(pool Submitter, fn func(ctx context.Context, loc Locator, changes Changes)) StoreOption {
	syncFn := func(ctx context.Context, loc Locator, changes Changes) {
		_ = pool.SubmitFunc(ctx, "after_update", func(taskCtx context.Context) error {
			fn(taskCtx, loc, changes)
			return nil
		})
	}
	return func(c *storeConfig) {
		c.afterUpdate = append(c.afterUpdate, any(syncFn))
	}
}

// WithAsyncAfterDelete wraps fn in a Pool.SubmitFunc call.
func WithAsyncAfterDelete(pool Submitter, fn func(ctx context.Context, loc Locator)) StoreOption {
	syncFn := func(ctx context.Context, loc Locator) {
		_ = pool.SubmitFunc(ctx, "after_delete", func(taskCtx context.Context) error {
			fn(taskCtx, loc)
			return nil
		})
	}
	return func(c *storeConfig) {
		c.afterDelete = append(c.afterDelete, any(syncFn))
	}
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
	queryExplicit := len(cfg.queryFields) > 0
	if queryExplicit {
		queryFieldMap = make(map[string]string, len(cfg.queryFields))
		for _, f := range cfg.queryFields {
			queryFieldMap[f] = f
		}
	} else {
		var queryCollisions []string
		queryFieldMap, queryCollisions = discoverFields(t, cfg.queryFieldsExclude)
		if len(queryCollisions) > 0 && logger != nil {
			logger.Warn("store: duplicate json tag names in query fields — last one wins, consider WithQueryFields to disambiguate",
				"model", t.Name(),
				"collisions", strings.Join(queryCollisions, ", "),
			)
		}
	}

	// Build updateFieldMap.
	// Priority: WithUpdateFields (explicit) > default auto-discover.
	var updateFieldMap map[string]string
	updateExplicit := len(cfg.updateFields) > 0
	if updateExplicit {
		updateFieldMap = make(map[string]string, len(cfg.updateFields))
		for _, f := range cfg.updateFields {
			updateFieldMap[f] = f
		}
	} else {
		var updateCollisions []string
		updateFieldMap, updateCollisions = discoverUpdateFields(t, cfg.updateFieldsExclude)
		if len(updateCollisions) > 0 && logger != nil {
			logger.Warn("store: duplicate json tag names in update fields — last one wins, consider WithUpdateFields to disambiguate",
				"model", t.Name(),
				"collisions", strings.Join(updateCollisions, ", "),
			)
		}
	}

	// Strict mode: reject auto-discovered fields unless the caller opted
	// in via WithAllQueryFields / WithAllUpdateFields — those options set
	// autoQueryFields / autoUpdateFields to signal "yes, I really do
	// want auto-discovery even though I'm in strict mode". Production
	// code should generally use explicit WithQueryFields /
	// WithUpdateFields so the exposed surface is visible in source.
	if cfg.strict {
		if !queryExplicit && !cfg.autoQueryFields && len(queryFieldMap) > 0 {
			panic(fmt.Sprintf("store: strict mode: %s has auto-discovered query fields %v; use WithQueryFields or WithAllQueryFields to declare explicitly", t.Name(), sortedKeys(queryFieldMap)))
		}
		if !updateExplicit && !cfg.autoUpdateFields && len(updateFieldMap) > 0 {
			panic(fmt.Sprintf("store: strict mode: %s has auto-discovered update fields %v; use WithUpdateFields or WithAllUpdateFields to declare explicitly", t.Name(), sortedKeys(updateFieldMap)))
		}
	}

	// Warn when fields are auto-discovered so developers know what's
	// exposed. Use WithQueryFields / WithUpdateFields to restrict. We
	// intentionally log only the count rather than the field list — a
	// production log of "which columns are filterable / writable" is a
	// low-effort inventory an attacker can read off a log aggregator.
	if logger != nil {
		if !queryExplicit && len(queryFieldMap) > 0 {
			logger.Warn("store: auto-discovered query fields; use WithQueryFields to restrict",
				"model", t.Name(), "count", len(queryFieldMap))
		}
		if !updateExplicit && len(updateFieldMap) > 0 {
			logger.Warn("store: auto-discovered update fields; use WithUpdateFields to restrict",
				"model", t.Name(), "count", len(updateFieldMap))
		}
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
	hasOwnerScope := false
	if !cfg.noOwnerScope {
		if _, ok := model.(db.OwnerAccessor); ok {
			scopes = append([]ScopeFunc{OwnerScope(getDefaultAdminRoles()...)}, scopes...)
			hasOwnerScope = true
		}
	}

	// Log scope configuration so the security posture is explicit. Models
	// embedding db.Owned get OwnerScope automatically (fail-closed: unauthenticated
	// requests return 401). WithoutOwnerScope or non-Owned models have no
	// automatic access control — make this visible.
	if logger != nil {
		customCount := len(cfg.scopes)
		switch {
		case hasOwnerScope && customCount > 0:
			logger.Info("store: scopes configured",
				"model", t.Name(),
				"owner_scope", true,
				"custom_scopes", customCount,
				"mode", "fail-closed")
		case hasOwnerScope:
			logger.Debug("store: owner scope active",
				"model", t.Name(),
				"mode", "fail-closed")
		case customCount > 0:
			logger.Info("store: custom scopes only (no owner scope)",
				"model", t.Name(),
				"custom_scopes", customCount)
		case cfg.noOwnerScope:
			logger.Warn("store: owner scope explicitly disabled",
				"model", t.Name(),
				"note", "all users can access all records")
		}
	}

	s := &Store[T]{
		db:               gdb,
		logger:           logger,
		queryFieldMap:    queryFieldMap,
		updateFieldMap:   updateFieldMap,
		soft:             db.IsSoftDeleteModel(model),
		scopes:           scopes,
		defaultPageSize:  cfg.defaultPageSize,
		maxPageSize:      cfg.maxPageSize,
		strict:           cfg.strict,
		requirePrincipal: cfg.requirePrincipal,
	}

	// Wire hooks from storeConfig (stored as any) into the typed hooks struct.
	// Panic on type mismatch — a mismatched hook signature is a programming
	// error that should be caught at construction time, not silently dropped.
	for i, fn := range cfg.beforeCreate {
		h, ok := fn.(func(context.Context, *T) error)
		if !ok {
			panic(fmt.Sprintf("store: beforeCreate hook #%d has wrong type %T, expected func(context.Context, *%s) error", i, fn, t.Name()))
		}
		s.hooks.beforeCreate = append(s.hooks.beforeCreate, h)
	}
	for i, fn := range cfg.beforeUpdate {
		h, ok := fn.(func(context.Context, Locator, Changes) error)
		if !ok {
			panic(fmt.Sprintf("store: beforeUpdate hook #%d has wrong type %T", i, fn))
		}
		s.hooks.beforeUpdate = append(s.hooks.beforeUpdate, h)
	}
	for i, fn := range cfg.beforeDelete {
		h, ok := fn.(func(context.Context, Locator) error)
		if !ok {
			panic(fmt.Sprintf("store: beforeDelete hook #%d has wrong type %T", i, fn))
		}
		s.hooks.beforeDelete = append(s.hooks.beforeDelete, h)
	}
	for i, fn := range cfg.afterCreate {
		h, ok := fn.(func(context.Context, *T))
		if !ok {
			panic(fmt.Sprintf("store: afterCreate hook #%d has wrong type %T, expected func(context.Context, *%s)", i, fn, t.Name()))
		}
		s.hooks.afterCreate = append(s.hooks.afterCreate, h)
	}
	for i, fn := range cfg.afterUpdate {
		h, ok := fn.(func(context.Context, Locator, Changes))
		if !ok {
			panic(fmt.Sprintf("store: afterUpdate hook #%d has wrong type %T", i, fn))
		}
		s.hooks.afterUpdate = append(s.hooks.afterUpdate, h)
	}
	for i, fn := range cfg.afterDelete {
		h, ok := fn.(func(context.Context, Locator))
		if !ok {
			panic(fmt.Sprintf("store: afterDelete hook #%d has wrong type %T", i, fn))
		}
		s.hooks.afterDelete = append(s.hooks.afterDelete, h)
	}

	return s
}

// safeAfterHook calls fn with panic recovery. After-hooks are
// fire-and-forget; a panic in a hook must not crash the request handler.
// Panics are logged through the Store's logger when present; otherwise
// they are reported to stderr so they never vanish into silence.
func (s *Store[T]) safeAfterHook(fn func()) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if s.logger != nil {
			s.logger.Error("store: after-hook panicked", "panic", r, "stack", string(debug.Stack()))
			return
		}
		fmt.Fprintf(os.Stderr, "store: after-hook panicked: %v\n%s\n", r, debug.Stack())
	}()
	fn()
}

// Create inserts a new record.
// If the model embeds db.Owned and OwnerID is empty, it is auto-filled
// from the authenticated principal's Subject.
// Returns ErrDuplicate on unique constraint violation.
func (s *Store[T]) Create(ctx context.Context, obj *T) error {
	if err := fillOwner(ctx, obj, s.requirePrincipal); err != nil {
		return err
	}
	for _, h := range s.hooks.beforeCreate {
		if err := h(ctx, obj); err != nil {
			return err
		}
	}
	if err := s.effectiveDB(ctx).Create(obj).Error; err != nil {
		return mapError(err)
	}
	for _, h := range s.hooks.afterCreate {
		h := h
		s.safeAfterHook(func() { h(ctx, obj) })
	}
	return nil
}

// BatchCreate inserts multiple records in a single transaction.
// Empty slice returns nil (no-op). Single failure rolls back the entire batch.
// If the model embeds db.Owned, OwnerID is auto-filled from the principal.
// Before-hooks run for each object before the transaction starts.
// Returns ErrDuplicate on unique constraint violation.
func (s *Store[T]) BatchCreate(ctx context.Context, objs []*T) error {
	if len(objs) == 0 {
		return nil
	}
	// Preflight the principal/owner check against a throwaway instance so
	// a strict-mode failure doesn't leave the batch half-mutated (items
	// ahead of the failure already had their OwnerID overwritten). The
	// policy is context-uniform — every object in the batch sees the same
	// principal and same strict setting — so a single probe is sufficient.
	//
	// Skipped entirely for non-Owned models: fillOwner is a no-op on them,
	// so running the probe just costs a reflect interface check with no
	// possible failure.
	if s.requirePrincipal && db.IsOwnedModel(new(T)) {
		var probe T
		if err := fillOwner(ctx, &probe, s.requirePrincipal); err != nil {
			return err
		}
	}
	for _, obj := range objs {
		if err := fillOwner(ctx, obj, s.requirePrincipal); err != nil {
			// Unreachable given the preflight above, but kept as a
			// defence-in-depth so any future per-object policy still
			// surfaces the error instead of silently succeeding.
			return err
		}
	}
	for _, obj := range objs {
		for _, h := range s.hooks.beforeCreate {
			if err := h(ctx, obj); err != nil {
				return err
			}
		}
	}
	baseDB := s.effectiveDB(ctx)
	// If already inside a context-scoped transaction, skip wrapping in a
	// new one — effectiveDB returns the tx handle directly.
	doCreate := func(gdb *gorm.DB) error {
		return gdb.CreateInBatches(objs, 100).Error
	}
	var err error
	if db.DBFromContext(ctx) != nil {
		err = doCreate(baseDB)
	} else {
		err = db.Transaction(ctx, baseDB, func(tx *gorm.DB) error {
			return doCreate(tx)
		})
	}
	if err != nil {
		return mapError(err)
	}
	for _, obj := range objs {
		for _, h := range s.hooks.afterCreate {
			h := h
			obj := obj
			s.safeAfterHook(func() { h(ctx, obj) })
		}
	}
	return nil
}

// ListByIDs retrieves records matching the given internal numeric IDs.
// Empty ids returns an empty slice. Order is not guaranteed.
// Intended for server-side batch joins across tables.
func (s *Store[T]) ListByIDs(ctx context.Context, ids []uint) ([]T, error) {
	if len(ids) == 0 {
		return []T{}, nil
	}
	q, err := s.applyScopes(ctx, s.effectiveDB(ctx).Model(new(T)))
	if err != nil {
		return nil, err
	}
	var items []T
	if err := q.Where("id IN ?", ids).Find(&items).Error; err != nil {
		return nil, mapError(err)
	}
	if items == nil {
		items = []T{}
	}
	return items, nil
}

// List retrieves records. Zero matches returns a Page with empty Items slice.
// Total is populated only when where.WithCount() is included; zero otherwise.
func (s *Store[T]) List(ctx context.Context, opts ...where.Option) (*Page[T], error) {
	return s.listInternal(ctx, nil, opts)
}

// ListQ is like List but additionally accepts QueryOptions (e.g.
// WithTrashed, WithPreload). QueryOptions are applied after scopes,
// so security constraints remain in effect.
//
//	page, err := s.ListQ(ctx, []store.QueryOption{store.WithTrashed()}, where.WithCount())
func (s *Store[T]) ListQ(ctx context.Context, qopts []QueryOption, opts ...where.Option) (*Page[T], error) {
	return s.listInternal(ctx, qopts, opts)
}

func (s *Store[T]) listInternal(ctx context.Context, qopts []QueryOption, opts []where.Option) (*Page[T], error) {
	// Enforce max page size: prepend so caller-provided WithMaxPageSize
	// options (e.g. from ListWithCursor) can override it by appearing later.
	if s.maxPageSize > 0 {
		opts = append([]where.Option{where.WithMaxPageSize(s.maxPageSize)}, opts...)
	}

	base, err := s.applyScopes(ctx, s.effectiveDB(ctx).Model(new(T)))
	if err != nil {
		return nil, err
	}
	base = s.applyQueryOpts(ctx, base, qopts)
	query, cfg, err := where.Apply(base, s.queryFieldMap, opts)
	if err != nil {
		return nil, mapQueryError(err)
	}

	var total int64
	if cfg.Count {
		// COUNT with filters only — pagination/order stripped so total is unaffected.
		countBase, err := s.applyScopes(ctx, s.effectiveDB(ctx).Model(new(T)))
		if err != nil {
			return nil, err
		}
		countBase = s.applyQueryOpts(ctx, countBase, qopts)
		countQuery, err := where.ApplyFiltersOnly(countBase, s.queryFieldMap, opts)
		if err != nil {
			return nil, mapQueryError(err)
		}
		if err := countQuery.Count(&total).Error; err != nil {
			return nil, mapError(err)
		}
	}

	var items []T
	if err := query.Find(&items).Error; err != nil {
		return nil, mapError(err)
	}

	// Guarantee non-nil slice for JSON serialization.
	if items == nil {
		items = []T{}
	}

	return &Page[T]{Items: items, Total: total}, nil
}

// ListFromQuery parses URL query parameters and returns a paginated list.
// Supported query params: page, size, order (field:asc|desc), and any field
// declared via WithQueryFields as an equality filter.
// Fixed filters should be applied via WithScope at Store construction time.
//
// Unknown query parameters are silently ignored unless the Store was
// constructed with WithStrict, in which case they return
// apierr.ErrInvalidArgument so clients get immediate feedback instead of
// silent "my filter didn't apply, why?" debugging.
func (s *Store[T]) ListFromQuery(ctx context.Context, query url.Values) ([]T, int64, error) {
	var opts []where.Option
	var err error
	if s.strict {
		opts, err = where.FromQueryStrict(query, s.queryFieldMap, s.defaultPageSize)
	} else {
		opts, err = where.FromQuery(query, s.queryFieldMap, s.defaultPageSize)
	}
	if err != nil {
		return nil, 0, mapQueryError(err)
	}
	page, err := s.List(ctx, opts...)
	if err != nil {
		return nil, 0, err
	}
	return page.Items, page.Total, nil
}

// Page is the result of a paginated list query. Items is guaranteed non-nil.
// Total is the total number of matching records when where.WithCount() is
// included in the query options; zero when count is not requested.
type Page[T any] struct {
	Items []T   `json:"items"`
	Total int64 `json:"total"`
}

// CursorPage is the result of a cursor-based paginated query. NextCursor
// is the value to pass as the cursor argument for the next page; empty
// string means no more pages. Items are guaranteed non-nil.
type CursorPage[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// ListWithCursor performs keyset (cursor-based) pagination. Unlike offset
// pagination, cursor pagination is O(1) regardless of page depth, making
// it suitable for mobile infinite-scroll and public APIs.
//
// cursorField is validated against the query whitelist. cursorValue is the
// last-seen value from the previous page (empty string or nil for the
// first page). size is the max items per page. Additional opts can add
// filters.
//
// Example:
//
//	page, err := s.ListWithCursor(ctx, "id", where.CursorAfter, lastID, 20)
func (s *Store[T]) ListWithCursor(ctx context.Context, cursorField string, direction where.CursorDirection, cursorValue any, size int, opts ...where.Option) (*CursorPage[T], error) {
	if size < 1 {
		return nil, fmt.Errorf("%w: cursor page size %d, must be >= 1", where.ErrInvalidParam, size)
	}
	// Enforce the Store's maxPageSize BEFORE deriving fetchSize, so a
	// caller that requests size > store cap gets rejected up front instead
	// of silently truncated. Reserve one slot below the ceiling for the
	// size+1 peek used to detect has-next-page: the WithCursor option
	// below rejects size > MaxPageSize, and we pass fetchSize = size+1
	// there, so the effective user-visible cap is MaxPageSize-1 when no
	// Store override is configured.
	cap := s.maxPageSize
	if cap <= 0 || cap > where.MaxPageSize-1 {
		cap = where.MaxPageSize - 1
	}
	if size > cap {
		return nil, fmt.Errorf("%w: cursor page size %d exceeds maximum %d", where.ErrInvalidParam, size, cap)
	}
	// Fetch size+1 to detect whether there's a next page.
	fetchSize := size + 1
	allOpts := make([]where.Option, 0, len(opts)+2)
	allOpts = append(allOpts, opts...)
	// Override the Store's maxPageSize with fetchSize so the sentinel
	// size+1 fetch isn't clamped back to size (which would break
	// has-next-page detection when maxPageSize == size). The bump is
	// safe because the `size <= cap` check above already ensured that
	// fetchSize stays within the package ceiling (cap+1 ≤ MaxPageSize+1,
	// and WithCursor/WithLimit reject size > MaxPageSize independently).
	allOpts = append(allOpts, where.WithMaxPageSize(fetchSize))
	if cursorValue != nil && cursorValue != "" {
		allOpts = append(allOpts, where.WithCursor(cursorField, direction, cursorValue, fetchSize))
	} else {
		// First page: just order + limit, no cursor WHERE.
		desc := direction == where.CursorBefore
		allOpts = append(allOpts, where.WithOrder(cursorField, desc), where.WithLimit(fetchSize))
	}

	result, err := s.List(ctx, allOpts...)
	if err != nil {
		return nil, err
	}

	page := &CursorPage[T]{Items: result.Items}
	if len(result.Items) > size {
		page.Items = result.Items[:size]
		page.NextCursor = extractCursorValue(result.Items[size-1], cursorField, s.queryFieldMap)
	}
	return page, nil
}

// Tx runs fn inside a transaction scoped to this Store. fn receives
// a Store bound to the transaction's DB handle.
//
// If a context-scoped transaction is already active (via db.RunInTx),
// Tx reuses it instead of opening a nested transaction. The txCtx is
// threaded through db.RunInTx so that any other Store / helper called
// with txCtx inside fn will also join the same transaction — matching
// the contract of db.RunInTx.
func (s *Store[T]) Tx(ctx context.Context, fn func(tx *Store[T]) error) error {
	return db.RunInTx(ctx, s.db, func(txCtx context.Context) error {
		tx := db.DBFromContext(txCtx)
		return fn(s.withDB(tx))
	})
}

// WithTx creates a new Store sharing config but using an external transaction.
// Useful for cross-Store transactions.
func (s *Store[T]) WithTx(tx *gorm.DB) *Store[T] {
	return s.withDB(tx)
}

// DB returns the underlying *gorm.DB as an escape hatch for complex queries.
// The returned handle has NO scopes applied — use ScopedDB when you need
// OwnerScope / custom scopes to remain in force.
func (s *Store[T]) DB() *gorm.DB {
	return s.db
}

// ScopedDB returns a *gorm.DB with WithContext(ctx) applied and all registered
// scopes (including auto-detected OwnerScope) evaluated.
//
// Use this when writing custom queries on an extended Store wrapper — e.g.:
//
//	func (s *BookshelfStore) FindByBookID(ctx context.Context, id uint) (*Item, error) {
//	    q, err := s.ScopedDB(ctx)
//	    if err != nil { return nil, err }
//	    var item Item
//	    return &item, q.Where("book_id = ?", id).First(&item).Error
//	}
//
// Scope errors (e.g. ErrUnauthenticated from OwnerScope with no principal in
// ctx) are returned to the caller; do NOT fall back to s.DB() on error — that
// would leak cross-tenant data.
func (s *Store[T]) ScopedDB(ctx context.Context) (*gorm.DB, error) {
	return s.applyScopes(ctx, s.effectiveDB(ctx))
}

// withDB returns a clone of s bound to a different *gorm.DB (used for
// transactional scopes). Implemented via struct-copy so future Store
// fields are automatically preserved in the clone — an explicit field
// list would silently drop new state (requirePrincipal was one such
// drift bug introduced in round-6 and caught in round-7).
func (s *Store[T]) withDB(gdb *gorm.DB) *Store[T] {
	cp := *s
	cp.db = gdb
	return &cp
}

// effectiveDB returns the *gorm.DB for the current operation. If a
// context-scoped transaction was started via db.RunInTx, the transaction
// handle is used; otherwise the Store's own connection is used.
func (s *Store[T]) effectiveDB(ctx context.Context) *gorm.DB {
	if tx := db.DBFromContext(ctx); tx != nil {
		return tx.WithContext(ctx)
	}
	return s.db.WithContext(ctx)
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

// mapError translates GORM errors to structured store errors.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound // bare sentinel — Get/Update/Delete add locator context
	}
	if isDuplicateError(err) {
		return &DuplicateEntryError{Detail: err.Error()}
	}
	return err
}

// Base model fields that must never be updated.
var baseModelExclude = map[string]bool{
	"id": true, "version": true, "created_at": true, "updated_at": true,
}

// baseQueryExclude is the default exclusion set for the query whitelist.
// version is hidden because the optimistic-lock counter is an internal
// concurrency primitive — exposing it through `?version=X` or
// `?order=version:desc` would leak schema details and let clients tamper
// with version semantics. id/created_at/updated_at remain queryable via
// the rid alias / standard sort fields.
var baseQueryExclude = map[string]bool{
	"version": true,
}

// discoverFields builds a queryFieldMap from JSON tags.
// Excludes json:"-", text/blob columns, version, and user-specified names.
// Returns the collisions encountered (duplicate JSON tag names routing
// to different DB columns) so the caller can warn at construction time.
func discoverFields(t reflect.Type, exclude []string) (map[string]string, []string) {
	ex := toSet(exclude)
	for k := range baseQueryExclude {
		ex[k] = true
	}
	result := make(map[string]string)
	var collisions []string
	scanJSONFieldsWithCollisions(t, ex, true, result, &collisions)
	return result, collisions
}

// discoverUpdateFields builds an updateFieldMap from JSON tags.
// Excludes json:"-", base model fields (id/version/timestamps), and user-specified names.
// Does NOT exclude text/blob (updating content is normal).
// Returns collisions — see discoverFields.
func discoverUpdateFields(t reflect.Type, exclude []string) (map[string]string, []string) {
	ex := toSet(exclude)
	for k := range baseModelExclude {
		ex[k] = true
	}
	result := make(map[string]string)
	var collisions []string
	scanJSONFieldsWithCollisions(t, ex, false, result, &collisions)
	return result, collisions
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// scanJSONFieldsWithCollisions is the field-discovery workhorse. If
// collisions is non-nil, any duplicate JSON tag name encountered is
// appended — so callers that care (e.g. Store construction) can warn
// instead of silently accepting the "last write wins" behaviour.
func scanJSONFieldsWithCollisions(t reflect.Type, exclude map[string]bool, skipLarge bool, out map[string]string, collisions *[]string) {
	for i := range t.NumField() {
		f := t.Field(i)

		// Recurse into embedded structs.
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				scanJSONFieldsWithCollisions(ft, exclude, skipLarge, out, collisions)
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
		if collisions != nil {
			if existing, ok := out[name]; ok && existing != col {
				*collisions = append(*collisions, fmt.Sprintf("%q (columns %q vs %q)", name, existing, col))
			}
		}
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
			// Insert underscore before uppercase when preceded by a lowercase
			// letter, handling acronyms correctly: "UserID" → "user_id",
			// "HTTPStatus" → "http_status".
			if i > 0 && s[i-1] >= 'a' && s[i-1] <= 'z' {
				buf = append(buf, '_')
			}
			buf = append(buf, byte(c)+32)
		} else {
			buf = append(buf, byte(c))
		}
	}
	return string(buf)
}

// extractCursorValue gets the value of the cursor field from the last
// item in a page result using the JSON-to-column mapping. It converts
// the value to a string suitable for passing as the next cursor.
func extractCursorValue(item any, field string, fieldMap map[string]string) string {
	col := field
	if c, ok := fieldMap[field]; ok {
		col = c
	}

	rv := reflect.ValueOf(item)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return ""
	}

	return extractCursorFromStruct(rv, col)
}

func extractCursorFromStruct(rv reflect.Value, col string) string {
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.Anonymous {
			fv := rv.Field(i)
			if fv.Kind() == reflect.Ptr {
				if fv.IsNil() {
					continue
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				if v := extractCursorFromStruct(fv, col); v != "" {
					return v
				}
			}
			continue
		}
		if gormColumnName(f) == col {
			val := rv.Field(i).Interface()
			// Only formats with round-trippable string representations
			// are supported. Binary / JSON / user-defined column types
			// would produce cursors that don't round-trip through the
			// next query, so they're explicitly rejected at this layer.
			switch v := val.(type) {
			case time.Time:
				// RFC3339Nano for reliable SQL driver round-trip.
				return v.UTC().Format(time.RFC3339Nano)
			case string:
				return v
			case int, int8, int16, int32, int64,
				uint, uint8, uint16, uint32, uint64,
				float32, float64, bool:
				return fmt.Sprint(v)
			}
			return ""
		}
	}
	return ""
}

// DuplicateError is an optional interface that database drivers or error
// wrappers can implement for reliable duplicate-key detection without
// string matching. When the error chain contains a DuplicateError
// implementation, isDuplicateError trusts it over heuristics.
type DuplicateError interface {
	IsDuplicate() bool
}

// isDuplicateError detects duplicate key errors using a three-tier strategy:
//
//  1. DuplicateError interface — most reliable, no string dependency.
//  2. GORM's ErrDuplicatedKey (v1.25.0+) — covers drivers that translate
//     into GORM's sentinel via translate plugin.
//  3. String matching fallback — catches MySQL, SQLite, PostgreSQL error
//     messages without importing driver packages.
func isDuplicateError(err error) bool {
	if err == nil {
		return false
	}
	// Tier 1: behaviour interface.
	var de DuplicateError
	if errors.As(err, &de) {
		return de.IsDuplicate()
	}
	// Tier 2: GORM sentinel.
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	// Tier 3: string heuristic (backward compatibility).
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "unique_violation") ||
		strings.Contains(msg, "constraint failed")
}
