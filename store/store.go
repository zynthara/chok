// Package store provides a generic CRUD store backed by GORM.
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/internal/txctx"
	"github.com/zynthara/chok/v2/kernel/event"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
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
	h      *db.DB
	logger log.Logger

	// txDB pins a transaction clone (see Tx) to that transaction's
	// handle; txCtx is the RunInTx context that created the clone, kept
	// so event publication through the clone still stages on the
	// transaction's after-commit buffer even when callers pass their
	// own outer context. Both are nil on a root Store.
	txDB  *gorm.DB
	txCtx context.Context

	bus              *event.Bus // nil unless WithBus was given
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

// hooks holds the registered before-callbacks.
//
// Before-hooks run inside the operation, before the DB write. Returning an
// error aborts the operation — the caller sees the hook's error, no row is
// written.
//
// v1's after-hooks are gone (SPEC §3.5): post-write notification is the
// event bus via WithBus — asynchronous, anchored to transaction commit.
// Logic that must run synchronously inside the write path belongs in
// before-hooks or explicit code around the call.
type hooks[T any] struct {
	beforeCreate []func(ctx context.Context, obj *T) error
	beforeUpdate []func(ctx context.Context, loc Locator, changes Changes) error
	beforeDelete []func(ctx context.Context, loc Locator) error
}

// StoreOption configures a Store.
type StoreOption func(*storeConfig)

type storeConfig struct {
	queryFields         []string
	updateFields        []string
	aliases             map[string]string
	scopes              []ScopeFunc
	bus                 *event.Bus
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

	// *Set flags distinguish "the construction site chose" from "left
	// unset": unset knobs inherit the handle's db.store policy, set
	// ones — including the Without* opt-outs — always win.
	strictSet           bool
	requirePrincipalSet bool
	defaultPageSizeSet  bool
	maxPageSizeSet      bool
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
// behavior is too permissive. The app-wide way to say the same thing is
// the handle's db.store policy (db.store.strict: true in chok.yaml),
// which this option — and WithoutStrict — override per store.
func WithStrict() StoreOption {
	return func(c *storeConfig) { c.strict = true; c.strictSet = true }
}

// WithoutStrict opts this Store out of a strict default inherited from
// the handle's db.store policy. The opt-out is deliberate call-site
// noise: a store that ships an implicit field surface inside a strict
// app should say so where it is built. No-op when nothing set strict.
func WithoutStrict() StoreOption {
	return func(c *storeConfig) { c.strict = false; c.strictSet = true }
}

// WithRequirePrincipal makes Create / BatchCreate / Upsert reject contexts
// without an authenticated principal when the model embeds db.Owned. This
// is fail-closed behaviour — safer for HTTP paths where a missing Authn
// middleware would otherwise let a client set OwnerID freely.
//
// Background jobs and tests that legitimately write Owned rows without a
// principal must either:
//   - Not enable this option on those stores (WithoutRequirePrincipal
//     when the handle's db.store policy turns it on app-wide), or
//   - Attach a system principal to ctx via auth.WithPrincipal before Create.
//
// Non-Owned models are unaffected. The app-wide counterpart is
// db.store.require_principal: true in chok.yaml.
func WithRequirePrincipal() StoreOption {
	return func(c *storeConfig) { c.requirePrincipal = true; c.requirePrincipalSet = true }
}

// WithoutRequirePrincipal opts this Store out of a require-principal
// default inherited from the handle's db.store policy — for background
// jobs and system flows that legitimately write db.Owned rows without
// an authenticated principal. Explicit at the call site by design; the
// quieter alternative is attaching a system principal to ctx. No-op
// when nothing set require-principal.
func WithoutRequirePrincipal() StoreOption {
	return func(c *storeConfig) { c.requirePrincipal = false; c.requirePrincipalSet = true }
}

// WithMaxPageSize sets a hard cap on page size for List / ListFromQuery.
// Requests exceeding this limit are silently clamped. Zero disables the
// cap — including one inherited from the handle's db.store policy.
func WithMaxPageSize(n int) StoreOption {
	return func(c *storeConfig) { c.maxPageSize = n; c.maxPageSizeSet = true }
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
// Zero restores that package default — including over a handle-policy
// value.
func WithDefaultPageSize(size int) StoreOption {
	return func(c *storeConfig) { c.defaultPageSize = size; c.defaultPageSizeSet = true }
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

// WithBus opts the Store into publishing EntityChanged[T] events on the
// given bus after successful writes (SPEC §3.5). Publication is anchored
// to transaction commit: a write inside Store.Tx / db.RunInTx stages its
// event on the transaction's after-commit buffer — flushed in write
// order when COMMIT succeeds, discarded wholesale on rollback — while a
// non-transactional write publishes as soon as the operation returns
// success. Delivery to subscribers is asynchronous (bus semantics);
// logic that must run synchronously inside the write path belongs in
// before-hooks.
//
// The bus is injected explicitly — never discovered from ctx or the DB
// handle — keeping this the store package's single kernel touch point:
//
//	posts := store.New[Post](db.From(k), k.Logger(),
//	    store.WithQueryFields(...), store.WithBus(k.Bus()))
func WithBus(b *event.Bus) StoreOption {
	if b == nil {
		panic("store: WithBus bus must not be nil")
	}
	return func(c *storeConfig) { c.bus = b }
}

// New creates a Store bound to the given database handle. T must be a
// struct type (not a pointer). Panics if T is a pointer type or has an
// invalid RIDPrefix.
//
// The first parameter is the v2 thin handle — obtain it from the db
// module (db.From(k)) or db.Open; this is the only store signature
// change from v1 (SPEC §5.1).
//
// The handle carries the app-level db.store policy (strict /
// require-principal / page-size caps, see db.StorePolicy): every knob
// the construction site leaves unset inherits it, so flipping
// db.store.strict: true in chok.yaml hardens every store at once.
// Options here override the policy per store; the explicit opt-outs
// are WithoutStrict and WithoutRequirePrincipal.
//
// Field allowlists resolve in priority order, per side (query/update):
//
//  1. WithQueryFields / WithUpdateFields — call-site lists. Use these
//     to narrow a model's declared surface for one consumer (e.g. a
//     public store that must not write privileged columns).
//  2. WithAllQueryFields / WithAllUpdateFields — explicit opt-in to
//     JSON-tag discovery with exclusions.
//  3. `store` struct tags on the model — the model's own declaration:
//
//     type Post struct {
//         db.OwnedSoftDeleteModel
//         Title   string `json:"title"   store:"query,update"`
//         Content string `json:"content" store:"update"`
//     }
//
//     Tag values are "query", "update" or both, comma-separated;
//     anything else panics at construction. The filter name is the
//     field's JSON name (snake_case of the Go name when the JSON tag
//     is absent or "-"). Embedded chok base models (db.Model and
//     friends) contribute their standard queryable fields (id,
//     created_at, updated_at) automatically; update lists never
//     include base-model fields. Tagged models skip the discovery
//     warning and satisfy WithStrict.
//  4. JSON-tag auto-discovery — the zero-config fallback. Logs a warn:
//     the implicit set silently grows with the struct, so production
//     code should declare fields via tags or options.
func New[T db.Modeler](h *db.DB, logger log.Logger, opts ...StoreOption) *Store[T] {
	if h == nil {
		panic("store.New: nil *db.DB handle (use db.From(k) or db.Open)")
	}
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

	// The handle's db.store policy fills every knob the construction
	// site left unset — production posture is a config flip, not a
	// per-call-site reminder (db-layer review #2). Explicit options,
	// including the WithoutStrict / WithoutRequirePrincipal opt-outs,
	// always win.
	pol := h.StorePolicy()
	strictFromPolicy := false
	if !cfg.strictSet {
		cfg.strict = pol.Strict
		strictFromPolicy = pol.Strict
	}
	if !cfg.requirePrincipalSet {
		cfg.requirePrincipal = pol.RequirePrincipal
	}
	if !cfg.maxPageSizeSet {
		cfg.maxPageSize = pol.MaxPageSize
	}
	if !cfg.defaultPageSizeSet {
		cfg.defaultPageSize = pol.DefaultPageSize
	}

	// The model's own `store` tag declaration, if any. Resolved before
	// the per-side switches so both sides agree on whether the model is
	// tag-declared; panics on malformed tags.
	tagQuery, tagUpdate, tagged := tagDeclaredFields(t)

	// Build queryFieldMap.
	// Priority: WithQueryFields (explicit) > WithAllQueryFields
	// (explicit auto) > `store` tags (model-declared) > auto-discover.
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
		queryFieldMap, queryCollisions = discoverFields(t, cfg.queryFieldsExclude)
		if len(queryCollisions) > 0 && logger != nil {
			logger.Warn("store: duplicate json tag names in query fields — last one wins, consider WithQueryFields to disambiguate",
				"model", t.Name(),
				"collisions", strings.Join(queryCollisions, ", "),
			)
		}
	}

	// Build updateFieldMap.
	// Priority: WithUpdateFields (explicit) > WithAllUpdateFields
	// (explicit auto) > `store` tags (model-declared) > auto-discover.
	var updateFieldMap map[string]string
	updateExplicit := len(cfg.updateFields) > 0
	updateTagged := tagged && !updateExplicit && !cfg.autoUpdateFields
	switch {
	case updateExplicit:
		updateFieldMap = make(map[string]string, len(cfg.updateFields))
		for _, f := range cfg.updateFields {
			updateFieldMap[f] = f
		}
	case updateTagged:
		updateFieldMap = tagUpdate
	default:
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
		// Name the origin so a config-flipped strict (db.store.strict)
		// panicking an app points operators at the yaml, not the code.
		mode := "strict mode"
		if strictFromPolicy {
			mode = "strict mode (from db.store policy)"
		}
		if !queryExplicit && !queryTagged && !cfg.autoQueryFields && len(queryFieldMap) > 0 {
			panic(fmt.Sprintf("store: %s: %s has auto-discovered query fields %v; declare them with `store` tags, WithQueryFields or WithAllQueryFields", mode, t.Name(), sortedKeys(queryFieldMap)))
		}
		if !updateExplicit && !updateTagged && !cfg.autoUpdateFields && len(updateFieldMap) > 0 {
			panic(fmt.Sprintf("store: %s: %s has auto-discovered update fields %v; declare them with `store` tags, WithUpdateFields or WithAllUpdateFields", mode, t.Name(), sortedKeys(updateFieldMap)))
		}
	}

	// Warn when fields are auto-discovered so developers know what's
	// exposed. Tag-declared and option-declared lists are explicit and
	// stay quiet. We intentionally log only the count rather than the
	// field list — a production log of "which columns are filterable /
	// writable" is a low-effort inventory an attacker can read off a
	// log aggregator.
	if logger != nil {
		if !queryExplicit && !queryTagged && len(queryFieldMap) > 0 {
			logger.Warn("store: auto-discovered query fields; declare them with `store` tags or WithQueryFields",
				"model", t.Name(), "count", len(queryFieldMap))
		}
		if !updateExplicit && !updateTagged && len(updateFieldMap) > 0 {
			logger.Warn("store: auto-discovered update fields; declare them with `store` tags or WithUpdateFields",
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
		h:                h,
		logger:           logger,
		bus:              cfg.bus,
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
	return s
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
	s.publishChanged(ctx, createdEvent(obj))
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
	// Already inside a transaction (context-scoped or a Tx clone):
	// write directly so the batch stays atomic with the caller's
	// transaction. Otherwise open one so a mid-batch failure rolls the
	// whole batch back (v1 semantics).
	var err error
	if txctx.DB(ctx) != nil || s.txDB != nil {
		err = s.effectiveDB(ctx).CreateInBatches(objs, 100).Error
	} else {
		err = s.h.RunInTx(ctx, func(txCtx context.Context) error {
			return s.effectiveDB(txCtx).CreateInBatches(objs, 100).Error
		})
	}
	if err != nil {
		return mapError(err)
	}
	for _, obj := range objs {
		s.publishChanged(ctx, createdEvent(obj))
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
		total, err = s.countInternal(ctx, qopts, opts)
		if err != nil {
			return nil, err
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

// Count returns the number of rows matching the filter options under
// the Store's scopes and the active soft-delete rules. Pagination and
// order options are stripped (COUNT is total-shaped by definition);
// filters resolve against the query allowlist like every read. With no
// options it counts everything the caller's scope can see — cheaper
// than List when only the number matters, and cheaper than
// List(WithCount) when the rows themselves aren't needed.
func (s *Store[T]) Count(ctx context.Context, opts ...where.Option) (int64, error) {
	return s.countInternal(ctx, nil, opts)
}

// countInternal is the COUNT path shared by Count and listInternal's
// WithCount branch: scopes, then filters only — pagination/order
// stripped so the total is unaffected. qopts lets list callers honour
// soft-delete visibility options (WithTrashed / WithOnlyTrashed) in
// their totals; the public Count passes none.
func (s *Store[T]) countInternal(ctx context.Context, qopts []QueryOption, opts []where.Option) (int64, error) {
	base, err := s.applyScopes(ctx, s.effectiveDB(ctx).Model(new(T)))
	if err != nil {
		return 0, err
	}
	base = s.applyQueryOpts(ctx, base, qopts)
	q, err := where.ApplyFiltersOnly(base, s.queryFieldMap, opts)
	if err != nil {
		return 0, mapQueryError(err)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return 0, mapError(err)
	}
	return total, nil
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

// Tx runs fn inside a transaction scoped to this Store. fn receives a
// Store clone bound to the transaction — its operations hit the
// transaction no matter which context they are called with, and its
// WithBus events stage on the transaction's after-commit buffer.
//
// If a context-scoped transaction is already active (via db.RunInTx),
// Tx reuses it instead of opening a nested transaction. Cross-store
// atomic writes are context propagation's job: inside a db.RunInTx
// callback, call any store with txCtx and it joins the same
// transaction — v1's WithTx wiring is gone (SPEC §5.1).
func (s *Store[T]) Tx(ctx context.Context, fn func(tx *Store[T]) error) error {
	return db.RunInTx(ctx, s.h, func(txCtx context.Context) error {
		return fn(s.txClone(txCtx))
	})
}

// Unsafe returns a raw *gorm.DB — transaction-aware (context
// transaction first, then a Tx clone's binding, then the root pool)
// with WithContext(ctx) and every registered scope (auto-detected
// OwnerScope included) applied. It replaces v1's DB()/ScopedDB() pair
// as the single escape hatch (SPEC §5.2); the name is the warning:
// the update whitelist, owner enforcement and optimistic locking do
// NOT apply to what you run on it.
//
//	func (s *BookshelfStore) FindByBookID(ctx context.Context, id uint) (*Item, error) {
//	    q, err := s.Unsafe(ctx)
//	    if err != nil { return nil, err }
//	    var item Item
//	    return &item, q.Where("book_id = ?", id).First(&item).Error
//	}
//
// Scope errors (e.g. ErrUnauthenticated from OwnerScope with no
// principal in ctx) are returned; do NOT fall back to a scope-free
// handle on error — that would leak cross-tenant data.
func (s *Store[T]) Unsafe(ctx context.Context) (*gorm.DB, error) {
	return s.applyScopes(ctx, s.effectiveDB(ctx))
}

// txClone returns a copy of s pinned to the transaction carried by
// txCtx. Struct-copy so future Store fields are automatically
// preserved (an explicit field list would silently drop new state —
// requirePrincipal was one such drift bug in v1 review round 6).
func (s *Store[T]) txClone(txCtx context.Context) *Store[T] {
	cp := *s
	cp.txDB = txctx.DB(txCtx)
	cp.txCtx = txCtx
	return &cp
}

// effectiveDB returns the *gorm.DB for the current operation:
// the context's transaction when one is active (db.RunInTx
// propagation), else the clone's pinned transaction (Store.Tx), else
// the root pool. The ordering matches v1: an explicit transactional
// context always wins.
func (s *Store[T]) effectiveDB(ctx context.Context) *gorm.DB {
	if tx := txctx.DB(ctx); tx != nil {
		return tx.WithContext(ctx)
	}
	if s.txDB != nil {
		return s.txDB.WithContext(ctx)
	}
	return s.h.Unsafe(ctx)
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

// storeTagName is the struct tag store.New reads for model-declared
// field allowlists: `store:"query"`, `store:"update"` or both.
const storeTagName = "store"

// chokDBPkgPath identifies embedded chok base models (db.Model and
// friends) during tag scanning without hard-coding the import path.
var chokDBPkgPath = reflect.TypeFor[db.Model]().PkgPath()

// tagDeclaredFields collects the model's own `store` tag declaration.
// Returned maps are filter-name → column. tagged reports whether any
// field carries a store tag — when false both maps are nil and the
// caller falls back to discovery.
//
// The filter name is the field's JSON name; fields hidden from JSON
// (no tag or json:"-") fall back to snake_case of the Go name, so an
// internal column can still be declared queryable. Embedded chok base
// models contribute their standard queryable fields (the JSON-visible
// set discovery would expose, minus version) to the query side only —
// update lists never gain base-model fields. Malformed tag values and
// duplicate names mapping to different columns panic: a declaration
// typo must fail construction, not silently narrow the surface.
func tagDeclaredFields(t reflect.Type) (query, update map[string]string, tagged bool) {
	query = make(map[string]string)
	update = make(map[string]string)
	base := make(map[string]string)
	scanStoreTags(t, query, update, base, &tagged)
	if !tagged {
		return nil, nil, false
	}
	// Base-model fields join the query side unless the declaration
	// already claimed the name.
	for name, col := range base {
		if _, ok := query[name]; !ok {
			query[name] = col
		}
	}
	return query, update, true
}

func scanStoreTags(t reflect.Type, query, update, base map[string]string, tagged *bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() != reflect.Struct {
				continue
			}
			if ft.PkgPath() == chokDBPkgPath {
				scanJSONFieldsWithCollisions(ft, baseQueryExclude, true, base, nil)
			} else {
				scanStoreTags(ft, query, update, base, tagged)
			}
			continue
		}
		tag, ok := f.Tag.Lookup(storeTagName)
		if !ok {
			continue
		}
		*tagged = true
		name := storeTagFieldName(f)
		col := gormColumnName(f)
		for _, raw := range strings.Split(tag, ",") {
			switch strings.TrimSpace(raw) {
			case "query":
				addDeclaredField(query, name, col, t, f)
			case "update":
				addDeclaredField(update, name, col, t, f)
			default:
				panic(fmt.Sprintf("store: %s.%s: bad `store:%q` tag value %q — use \"query\", \"update\" or both (remove the tag to keep the field private)",
					t.Name(), f.Name, tag, strings.TrimSpace(raw)))
			}
		}
	}
}

func addDeclaredField(m map[string]string, name, col string, t reflect.Type, f reflect.StructField) {
	if existing, ok := m[name]; ok && existing != col {
		panic(fmt.Sprintf("store: %s.%s: declared field name %q maps to columns %q and %q — rename the JSON tag or drop one declaration",
			t.Name(), f.Name, name, existing, col))
	}
	m[name] = col
}

// storeTagFieldName resolves the filter name for a tag-declared field:
// the JSON name when visible, snake_case of the Go name otherwise.
func storeTagFieldName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" || name == "-" {
		return toSnakeCase(f.Name)
	}
	return name
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

// isDuplicateError detects duplicate key errors using a tiered strategy:
//
//  1. DuplicateError interface — most reliable, no string dependency.
//  2. Typed pgx error — SQLSTATE 23505 (unique_violation) is Postgres's
//     authoritative signal; any other PG code is authoritatively NOT a
//     duplicate (M3, SPEC §5.3 error-mapping acceptance).
//  3. GORM's ErrDuplicatedKey (v1.25.0+) — covers drivers that translate
//     into GORM's sentinel via translate plugin.
//  4. String matching fallback — catches MySQL and SQLite error
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
	// Tier 2: pgx SQLSTATE.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgUniqueViolation
	}
	// Tier 3: GORM sentinel.
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	// Tier 4: string heuristic (backward compatibility).
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "unique_violation") ||
		strings.Contains(msg, "constraint failed")
}

// pgUniqueViolation is SQLSTATE class 23, unique_violation.
const pgUniqueViolation = "23505"
