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

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"

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
	// ErrProtectedUpdateField indicates an update list or alias resolved to a
	// lifecycle/ownership column managed exclusively by Store.
	ErrProtectedUpdateField = errors.New("store: framework-managed field cannot be updated")
	// ErrDegenerateConditions means the locator's filter is present but
	// collapses to match-nothing (e.g. WithFilterIn over an empty slice,
	// WithFilter with a nil value). Distinguishing this from "no filter"
	// lets Update/Delete callers surface a precise client-input error
	// rather than silently succeeding with zero rows affected.
	ErrDegenerateConditions = errors.New("store: filter matches nothing")
	ErrDuplicate            = errors.New("store: duplicate entry")
	// ErrMissingConflictColumns indicates an Upsert/BatchUpsert call omitted
	// the unique columns that identify the conflict target.
	ErrMissingConflictColumns = errors.New("store: upsert called without conflict columns")
	// ErrDuplicateBatchConflict indicates two BatchUpsert inputs carry the
	// same declared conflict-key tuple. Such input is rejected before SQL so
	// statement chunk boundaries cannot change the result.
	ErrDuplicateBatchConflict = errors.New("store: duplicate conflict key in batch upsert")

	// ErrUnknownUpdateField indicates the field name is not in the update whitelist.
	// This is a programming error (code passes a wrong field constant), not client input.
	ErrUnknownUpdateField = errors.New("store: unknown update field")

	// ErrUpdateFieldsNotConfigured indicates WithUpdateFields was not called.
	// This is a programming error (Store misconfigured), not client input.
	ErrUpdateFieldsNotConfigured = errors.New("store: update fields not configured")

	// ErrUpsertScoped indicates Upsert or BatchUpsert was called on a Store that has
	// scopes registered. SQL INSERT ... ON CONFLICT DO UPDATE does not
	// honour WHERE-based scope conditions on the conflict update path,
	// so a conflict on a globally unique column could silently bypass
	// tenant isolation or other scope invariants. Use Create + Update,
	// or s.Unsafe(ctx) as an escape hatch if you understand the implications.
	ErrUpsertScoped = errors.New("store: upsert is not safe with scoped stores (scopes are ignored on conflict update); use separate Create + Update")

	// ErrLockRequiresTx indicates GetForUpdate was called outside a
	// transaction on the store's handle. A row lock under autocommit is
	// released before the caller can act on the row, so the entry point
	// enforces what the guarantee needs: wrap the call in Store.Tx or
	// db.RunInTx on the same *db.DB.
	ErrLockRequiresTx = errors.New("store: GetForUpdate requires a transaction on the store's handle (Store.Tx or db.RunInTx)")

	// ErrLockPreload indicates GetForUpdate was called with WithPreload.
	// Association rows load through separate queries the row lock does
	// not cover; a locked read with preloads would look atomic without
	// being it. Lock the row, then load associations separately.
	ErrLockPreload = errors.New("store: GetForUpdate does not support WithPreload (association queries run outside the lock)")
)

const createBatchSize = 100

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
//
// Field is the public field name the violation maps to, populated when
// the Store declared the violated constraint via WithConstraintFields;
// empty otherwise. MapError prefers it over the raw constraint name.
type DuplicateEntryError struct {
	Detail string // driver-specific constraint/message
	Field  string // declared constraint→field mapping hit, or empty
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

	bus              *event.Bus        // nil unless WithBus was given
	queryFieldMap    map[string]string // filter + order
	updateFieldMap   map[string]string // update SET columns
	constraintFields map[string]string // WithConstraintFields: constraint identifier → public field name
	modelSchema      *schema.Schema    // GORM's authoritative field/column mapping
	aggCatalog       *aggCatalogCache  // real catalog column types for the aggregate gate, resolved lazily (shared across tx clones via pointer)
	ridColumn        string            // RID's DB column — cursor tie-breaker, independent of the query allowlist
	soft             bool              // true if T embeds SoftDeleteModel
	scopes           []ScopeFunc
	defaultPageSize  int      // default page size for ListFromQuery (0 = where.DefaultPageSize)
	maxPageSize      int      // max page size (0 = unlimited)
	strict           bool     // strict mode: reject auto-discovered fields, unknown params
	requirePrincipal bool     // fail-closed: Create/Upsert on Owned models reject no-principal contexts
	adminRoles       []string // construction-resolved: drives auto-OwnerScope bypass AND owner fill
	readOnly         bool     // blessed write methods fail before hooks or model mutation
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
	beforeUpdate []func(ctx context.Context, loc Locator, changes ChangeSnapshot) error
	beforeDelete []func(ctx context.Context, loc Locator) error
}

// StoreOption configures a Store.
type StoreOption func(*storeConfig)

type storeConfig struct {
	queryFields         []string
	updateFields        []string
	aliases             map[string]string
	constraintFields    map[string]string
	scopes              []ScopeFunc
	bus                 *event.Bus
	defaultPageSize     int
	maxPageSize         int // 0 = unlimited
	autoQueryFields     bool
	autoUpdateFields    bool
	queryFieldsExclude  []string
	updateFieldsExclude []string
	noOwnerScope        bool
	adminRoles          []string // WithAdminRoles: roles bypassing OwnerScope + owner fill
	strict              bool     // when true: reject unknown query params, require explicit whitelist
	requirePrincipal    bool     // when true: Create/Upsert on Owned models reject no-principal contexts
	readOnly            bool     // explicit declaration required for read-only db handles
	beforeCreate        []any    // []func(ctx, *T) error — stored as any to avoid generic storeConfig
	beforeUpdate        []any    // []func(ctx, Locator, ChangeSnapshot) error
	beforeDelete        []any    // []func(ctx, Locator) error

	// *Set flags distinguish "the construction site chose" from "left
	// unset": unset knobs inherit the handle's db.store policy, set
	// ones — including the Without* opt-outs — always win.
	strictSet           bool
	requirePrincipalSet bool
	adminRolesSet       bool
	defaultPageSizeSet  bool
	maxPageSizeSet      bool
}

// WithReadOnly declares that this Store exposes a read-only CRUD surface.
// It is required when binding a handle configured with db.read_only: true;
// all blessed write methods then fail with db.ErrReadOnly before hooks or
// model mutation. Unsafe remains an explicitly unsafe GORM escape hatch and
// is protected by the underlying read-only handle's callback/driver guards.
func WithReadOnly() StoreOption {
	return func(c *storeConfig) { c.readOnly = true }
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

// WithAdminRoles sets, for this Store, the principal roles that bypass the
// automatic OwnerScope and may set OwnerID explicitly on create (imports,
// backfills, cross-user writes). One list drives BOTH sides of the owner
// contract — the query-side scope bypass and the write-side owner fill — so
// the two can never disagree.
//
// The list REPLACES the inherited one (db.store.admin_roles policy, else the
// deprecated package default) rather than adding to it; calling it with no
// arguments removes every admin bypass on this Store (fail-closed). This is
// the supported way to widen or narrow admin semantics per store. Passing an
// extra OwnerScope through WithScope does NOT override roles: scopes compose
// by AND, so a second OwnerScope intersects the bypass sets — nobody outside
// both lists escapes the owner filter.
//
// The roles are captured at construction; later SetDefaultAdminRoles calls
// never affect a Store built with this option. A blank role name panics —
// the same configuration error the db.store.admin_roles policy rejects at
// validation, caught here at construction (an empty string could otherwise
// match a principal whose resolver produced an empty role).
func WithAdminRoles(roles ...string) StoreOption {
	// Snapshot before validating: the variadic slice is the caller's own
	// when invoked as WithAdminRoles(roles...), so validating (or copying
	// later, when the option runs inside New) would leave a window where
	// mutating the original slice rewrites the authorization config — and
	// sneaks a blank role past this check.
	cp := append([]string(nil), roles...)
	for i, r := range cp {
		if strings.TrimSpace(r) == "" {
			panic(fmt.Sprintf("store: WithAdminRoles: role %d must not be empty", i))
		}
	}
	return func(c *storeConfig) {
		c.adminRoles = cp
		c.adminRolesSet = true
	}
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
// Requests exceeding this limit are silently clamped. A query-level cap may
// tighten but never raise this Store cap. Zero disables the Store cap —
// including one inherited from the handle's db.store policy — while the
// package-level where.MaxPageSize ceiling still applies.
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

// WithUpdateFields declares which fields are updatable. Column name defaults to
// the field name. Fields not declared here are rejected by Update. Framework-
// managed lifecycle and ownership columns are rejected even when explicitly
// listed or targeted through WithColumnAlias.
func WithUpdateFields(fields ...string) StoreOption {
	return func(c *storeConfig) {
		c.updateFields = append(c.updateFields, fields...)
	}
}

// WithColumnAlias maps a public field name to a different database column (e.g.
// "id" → "rid" on the query side). The field must be declared via
// WithQueryFields or WithUpdateFields; otherwise Store construction panics.
// An update alias may not target a framework-managed column.
func WithColumnAlias(field, column string) StoreOption {
	return func(c *storeConfig) {
		if c.aliases == nil {
			c.aliases = make(map[string]string)
		}
		c.aliases[field] = column
	}
}

// WithConstraintFields declares how unique-constraint violations report
// themselves: a map from the constraint identifier the database names in
// its duplicate-key error to the public field name the API should blame.
// When a write returns ErrDuplicate and the violated constraint is
// declared here, the error carries that field name —
// DuplicateEntryError.Field, surfaced by MapError as response metadata
// key field — instead of the raw constraint name: index names leak
// schema layout and drift with migrations, while field names are the
// API's own vocabulary (the reasoning behind Ecto's unique_constraint).
// Undeclared constraints keep the existing behaviour (constraint-name
// metadata).
//
// Keys match the identifier as the driver reports it, with table
// qualifiers stripped (MySQL 8 reports keys as table.key). SQLite names
// no index in its message — it reports the violated column list — so a
// declaration that must hold across dialects lists both spellings; note
// that SoftUnique indexes include delete_token in that list:
//
//	store.WithConstraintFields(map[string]string{
//	    "uk_email":           "email", // Postgres / MySQL: index name
//	    "email,delete_token": "email", // SQLite: column list of the SoftUnique index
//	})
//
// The field name is reported to clients verbatim. It is deliberately
// not validated against the query/update allowlists — create-path
// fields legitimately live in neither. Empty keys or values panic at
// construction. Multiple calls merge; later calls win on duplicate keys.
func WithConstraintFields(fields map[string]string) StoreOption {
	// Snapshot before validating, for the same reason WithAdminRoles
	// does: the caller keeps a reference to the map, and a mutation after
	// construction must not rewrite — or un-validate — the declaration.
	cp := make(map[string]string, len(fields))
	for constraint, field := range fields {
		if strings.TrimSpace(constraint) == "" || strings.TrimSpace(field) == "" {
			panic("store: WithConstraintFields: constraint and field names must not be empty")
		}
		cp[constraint] = field
	}
	return func(c *storeConfig) {
		if c.constraintFields == nil {
			c.constraintFields = make(map[string]string, len(cp))
		}
		for constraint, field := range cp {
			c.constraintFields[constraint] = field
		}
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

// WithBeforeUpdate registers a callback that runs before an Update or
// BatchUpdate writes to the database. Returning an error aborts the write —
// no row is touched and the caller sees the hook's error.
//
// The callback receives the resolved ChangeSnapshot: public update-field
// names mapped to the values about to be written, with a whole-whitelist
// Fields(&obj) update arriving fully expanded. Accessors return recursive
// copies, so hooks can inspect but never mutate the payload — cross-field
// validation and permission checks read it; value normalisation belongs in
// the caller or a before-create hook, which receives the mutable object.
//
// Static validation (update whitelist, protected columns) runs when the
// Changes are built, before any hook — a callback only ever observes a
// structurally valid change set.
func WithBeforeUpdate(fn func(ctx context.Context, loc Locator, changes ChangeSnapshot) error) StoreOption {
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
// Delivery guarantee: AT-MOST-ONCE, in-process. Events exist only in
// this process's memory — a crash between COMMIT and the buffer flush
// loses them, the bus's default overflow policy (drop-oldest) discards
// events when a subscriber's queue fills (counted, rate-limited warn),
// and there is no persistence or replay. That is the right trade for
// cache invalidation and other consumers that recover via TTL or
// re-read; do NOT build audit trails, projections, or anything that
// must observe every committed write on this — reliable delivery needs
// a transactional outbox, which chok does not ship (yet).
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
//
//  2. WithAllQueryFields / WithAllUpdateFields — explicit opt-in to
//     JSON-tag discovery with exclusions.
//
//  3. `store` struct tags on the model — the model's own declaration:
//
//     type Post struct {
//     db.OwnedSoftDeleteModel
//     Title   string `json:"title"   store:"query,update"`
//     Content string `json:"content" store:"update"`
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
//
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
	stmt := &gorm.Statement{DB: h.Unsafe(context.Background())}
	if err := stmt.Parse(model); err != nil {
		panic(fmt.Sprintf("store.New: parse GORM schema for %s: %v", t.Name(), err))
	}
	modelSchema := stmt.Schema

	// Resolve the RID column once: ListWithCursor's tie-breaker binds to it
	// directly — a store that doesn't expose "id" in its query allowlist
	// (or aliases it elsewhere) must still paginate. ValidateModel above
	// guarantees the db.Model embed, so the field always parses.
	ridField := modelSchema.LookUpField("RID")
	if ridField == nil || ridField.DBName == "" {
		panic(fmt.Sprintf("store.New: %s has no RID column in its parsed schema", t.Name()))
	}
	ridColumn := ridField.DBName

	cfg := &storeConfig{}
	for _, o := range opts {
		o(cfg)
	}
	if h.ReadOnly() && !cfg.readOnly {
		panic("store.New: read-only db handle requires store.WithReadOnly() (or bind a writable instance)")
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

	// Admin-role resolution mirrors the other policy knobs: explicit
	// WithAdminRoles wins, then the handle's db.store.admin_roles, then
	// the (deprecated) package-level default. The resolved list drives
	// BOTH the auto-detected OwnerScope and the write-side owner fill,
	// so query bypass and OwnerID assignment can never disagree — and it
	// is captured here, at construction, so a later SetDefaultAdminRoles
	// call cannot skew one side of an existing store.
	adminRoles := getDefaultAdminRoles()
	if len(pol.AdminRoles) > 0 {
		adminRoles = append([]string(nil), pol.AdminRoles...)
	}
	if cfg.adminRolesSet {
		adminRoles = cfg.adminRoles
	}

	// The model's own `store` tag declaration, if any. Resolved before
	// the per-side switches so both sides agree on whether the model is
	// tag-declared; panics on malformed tags.
	tagQuery, tagUpdate, tagged := tagDeclaredFields(t, modelSchema)

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
		queryFieldMap, queryCollisions = discoverFields(modelSchema, cfg.queryFieldsExclude)
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
		updateFieldMap, updateCollisions = discoverUpdateFields(modelSchema, cfg.updateFieldsExclude)
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
	// so this mapping is always correct on the query side. The update side is
	// validated below and rejects RID with every other framework-managed column.
	if _, explicit := cfg.aliases["id"]; !explicit {
		if queryFieldMap != nil && queryFieldMap["id"] == "id" {
			queryFieldMap["id"] = "rid"
		}
		if updateFieldMap != nil && updateFieldMap["id"] == "id" {
			updateFieldMap["id"] = "rid"
		}
	}

	// Aliases and explicit update lists must not reopen lifecycle or ownership
	// columns excluded by automatic discovery. Keep this after alias expansion
	// so innocuous-looking public names such as "id" cannot silently become a
	// writable RID. The same invariant is checked again when Changes builds.
	for field, col := range updateFieldMap {
		if isProtectedUpdateColumn(modelSchema, col) {
			panic(fmt.Sprintf("store.New: update field %q resolves to framework-managed column %q", field, col))
		}
		if _, err := where.ResolveField(updateFieldMap, field); err != nil {
			panic(fmt.Sprintf("store.New: update field %q has invalid column %q: %v", field, col, err))
		}
	}
	for field, col := range queryFieldMap {
		if _, err := where.ResolveField(queryFieldMap, field); err != nil {
			panic(fmt.Sprintf("store.New: query field %q has invalid column %q: %v", field, col, err))
		}
	}

	// Auto-detect OwnerScope for models embedding db.Owned. The bypass
	// roles are the construction-resolved adminRoles — the same list
	// fillOwner consults on the write side.
	scopes := cfg.scopes
	hasOwnerScope := false
	if !cfg.noOwnerScope {
		if _, ok := model.(db.OwnerAccessor); ok {
			scopes = append([]ScopeFunc{OwnerScope(adminRoles...)}, scopes...)
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
		constraintFields: cfg.constraintFields,
		modelSchema:      modelSchema,
		aggCatalog:       &aggCatalogCache{},
		ridColumn:        ridColumn,
		soft:             db.IsSoftDeleteModel(model),
		scopes:           scopes,
		defaultPageSize:  cfg.defaultPageSize,
		maxPageSize:      cfg.maxPageSize,
		strict:           cfg.strict,
		requirePrincipal: cfg.requirePrincipal,
		adminRoles:       adminRoles,
		readOnly:         cfg.readOnly,
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
		h, ok := fn.(func(context.Context, Locator, ChangeSnapshot) error)
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
	if err := s.rejectWrite("Create"); err != nil {
		return err
	}
	if err := fillOwner(ctx, obj, s.requirePrincipal, s.adminRoles); err != nil {
		return err
	}
	for _, h := range s.hooks.beforeCreate {
		if err := h(ctx, obj); err != nil {
			return err
		}
	}
	if err := s.effectiveDB(ctx).Create(obj).Error; err != nil {
		return s.mapError(err)
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
	if err := s.rejectWrite("BatchCreate"); err != nil {
		return err
	}
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
		if err := fillOwner(ctx, &probe, s.requirePrincipal, s.adminRoles); err != nil {
			return err
		}
	}
	for _, obj := range objs {
		if err := fillOwner(ctx, obj, s.requirePrincipal, s.adminRoles); err != nil {
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
	if txctx.DB(ctx, s.h) != nil || s.txDB != nil {
		err = s.effectiveDB(ctx).CreateInBatches(objs, createBatchSize).Error
	} else {
		err = s.h.RunInTx(ctx, func(txCtx context.Context) error {
			return s.effectiveDB(txCtx).CreateInBatches(objs, createBatchSize).Error
		})
	}
	if err != nil {
		return s.mapError(err)
	}
	for _, obj := range objs {
		s.publishChanged(ctx, createdEvent(obj))
	}
	return nil
}

// BatchUpdate applies per-object values to multiple rows. It is equivalent to
// Update(ctx, locator, Fields(obj, fields...)) for each item, with all SQL
// writes in one transaction. Use Update with Where + Set for a single value
// applied to many rows in one SQL statement.
//
// ID is the preferred locator when present; RID is used otherwise. Every item
// must carry one of them. Before-update hooks run for every item before this
// method opens its transaction. A hook error prevents all BatchUpdate SQL.
//
// Optimistic locking and zero-value persistence match Fields. On an error from
// the batch transaction, Version mutations made by successful earlier items
// are restored. When BatchUpdate joins a caller-owned transaction that later
// rolls back after BatchUpdate has returned success, callers must discard or
// reload the objects, as with individual Update calls in that transaction. If
// BatchUpdate returns an error inside a caller-owned transaction, the caller
// must roll that transaction back (or reload every input before continuing);
// ignoring the error may allow earlier SQL writes to commit on some databases
// even though this method restored their in-memory Version values.
func (s *Store[T]) BatchUpdate(ctx context.Context, objs []*T, fields ...string) error {
	if err := s.rejectWrite("BatchUpdate"); err != nil {
		return err
	}
	if len(objs) == 0 {
		return nil
	}

	// Resolve the shared field list once before any per-item processing so a
	// bad field name fails ahead of nil-object and locator checks. Each item
	// is then built individually below — those builds produce the snapshots
	// the hooks inspect and the payloads updateBuilt consumes.
	var probe T
	if _, err := Fields(&probe, fields...).build(ctx, s.updateFieldMap, s.modelSchema); err != nil {
		return err
	}

	locators := make([]Locator, len(objs))
	changes := make([]Changes, len(objs))
	models := make([]*db.Model, len(objs))
	for i, obj := range objs {
		if obj == nil {
			return fmt.Errorf("store: BatchUpdate item %d: obj is nil", i)
		}
		model := extractModelSafe(obj)
		if model == nil {
			return fmt.Errorf("store: BatchUpdate item %d: model does not embed db.Model", i)
		}
		switch {
		case model.ID > 0:
			locators[i] = ID(model.ID)
		case model.RID != "":
			locators[i] = RID(model.RID)
		default:
			return fmt.Errorf("store: BatchUpdate item %d: missing ID and RID", i)
		}
		changes[i] = Fields(obj, fields...)
		models[i] = model
	}

	// Build every item before hooks or SQL: static whitelist and
	// protected-column validation precede user logic (the batch doctrine),
	// hooks receive the resolved per-item snapshots, and a validation
	// failure aborts before any statement — even inside a caller-owned
	// transaction, where earlier items' SQL previously ran ahead of a later
	// item's build error.
	builts := make([]builtChanges, len(objs))
	for i := range objs {
		b, err := changes[i].build(ctx, s.updateFieldMap, s.modelSchema)
		if err != nil {
			return fmt.Errorf("store: BatchUpdate item %d: %w", i, err)
		}
		builts[i] = b
	}

	for i := range objs {
		for _, h := range s.hooks.beforeUpdate {
			if err := h(ctx, locators[i], builts[i].event); err != nil {
				return fmt.Errorf("store: BatchUpdate item %d: %w", i, err)
			}
		}
	}

	originalVersions := make([]int, len(models))
	for i, model := range models {
		originalVersions[i] = model.Version
	}
	restoreVersions := func() {
		for i, model := range models {
			model.Version = originalVersions[i]
		}
	}
	updateAll := func(txCtx context.Context) error {
		for i := range objs {
			if err := s.updateBuilt(txCtx, locators[i], builts[i], &updateConfig{}); err != nil {
				return fmt.Errorf("store: BatchUpdate item %d: %w", i, err)
			}
		}
		return nil
	}

	var err error
	if txctx.DB(ctx, s.h) != nil || s.txDB != nil {
		err = updateAll(ctx)
	} else {
		err = s.h.RunInTx(ctx, updateAll)
	}
	if err != nil {
		restoreVersions()
		return err
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
		return nil, s.mapError(err)
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
	return s.listInternalWithMaxPageSize(ctx, qopts, opts, s.maxPageSize)
}

// listInternalWithMaxPageSize is the shared list execution path. A zero
// maxPageSize skips Store-cap injection; ListWithCursor uses that only after
// validating its caller-visible size against the Store cap, so its private
// size+1 lookahead row is not mistaken for caller-visible page capacity.
func (s *Store[T]) listInternalWithMaxPageSize(ctx context.Context, qopts []QueryOption, opts []where.Option, maxPageSize int) (*Page[T], error) {
	// Enforce max page size. where.WithMaxPageSize composes caps by taking
	// the minimum, so caller options may tighten this policy but cannot raise it.
	if maxPageSize > 0 {
		opts = append([]where.Option{where.WithMaxPageSize(maxPageSize)}, opts...)
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
		return nil, s.mapError(err)
	}

	// Guarantee non-nil slice for JSON serialization.
	if items == nil {
		items = []T{}
	}

	// The effective pagination comes from the same Config that rendered
	// LIMIT/OFFSET — the only place an honest envelope can come from.
	meta := cfg.PageInfo()
	if cfg.Count {
		meta.HasMore = int64(meta.Offset)+int64(len(items)) < total
	}
	return &Page[T]{Items: items, Total: total, Meta: meta}, nil
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
		return 0, s.mapError(err)
	}
	return total, nil
}

// ListFromQuery parses URL query parameters and returns a paginated list.
// Supported query params: page, size, order (field:asc|desc), and any field
// declared via WithQueryFields as an equality filter.
// Fixed filters should be applied via WithScope at Store construction time.
//
// It is HTTP-shaped sugar over List and deliberately not part of the
// Reader contract — parsing transport input belongs to the edges
// (handler.HandleList, or this helper), not to the data interface.
//
// The returned Page.Meta is the pagination the query actually executed
// with — after the store's max-page-size cap and default page size — so
// envelope renderers never re-derive page/size from the raw request and
// drift from the SQL. The parse always includes where.WithCount(), so
// Page.Total and Meta.HasMore are filled on every call.
//
// Unknown query parameters are silently ignored unless the Store was
// constructed with WithStrict, in which case they return
// apierr.ErrInvalidArgument so clients get immediate feedback instead of
// silent "my filter didn't apply, why?" debugging.
func (s *Store[T]) ListFromQuery(ctx context.Context, query url.Values) (*Page[T], error) {
	var opts []where.Option
	var err error
	if s.strict {
		opts, err = where.FromQueryStrict(query, s.queryFieldMap, s.defaultPageSize)
	} else {
		opts, err = where.FromQuery(query, s.queryFieldMap, s.defaultPageSize)
	}
	if err != nil {
		// The whole chain is client-facing: unlike the programmatic entry
		// points, an unknown FIELD here (an order/filter name from the
		// URL) is client input and maps to 400 with the value errors.
		return nil, mapClientQueryError(err)
	}
	page, err := s.List(ctx, opts...)
	if err != nil {
		// FromQuery pre-validates every field name it accepts, so today
		// this leg cannot produce where.ErrUnknownField; the client
		// mapping stays as a seam guard so a future FromQuery change
		// cannot silently break the whole-chain 400 contract. Already
		// mapped *apierr.Error values pass through unchanged.
		return nil, mapClientQueryError(err)
	}
	return page, nil
}

// Page is the result of a paginated list query — an alias of where.Page,
// which is declared in the query layer so envelope renderers
// (handler.HandleList) can speak it without importing store. Application
// code keeps spelling it store.Page; see where.Page for field semantics.
type Page[T any] = where.Page[T]

// CursorPage is the result of a cursor-based paginated query. NextCursor
// is the OPAQUE token to pass back verbatim as the cursor argument for the
// next page; empty string means no more pages. Clients must not parse or
// construct it — see ListWithCursor. Items are guaranteed non-nil.
type CursorPage[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// ListWithCursor performs keyset (cursor-based) pagination. Unlike offset
// pagination, cursor pagination is O(1) regardless of page depth, making
// it suitable for mobile infinite-scroll and public APIs.
//
// cursor is OPAQUE: pass the empty string for the first page, then feed
// CursorPage.NextCursor back verbatim for each following page. Clients
// must not parse, build or store meaning into the token — it binds the
// pagination contract (format version, field, direction) and is rejected
// as apierr.ErrInvalidArgument when replayed against a different one.
// Tokens longer than MaxCursorTokenLen (4096 bytes) are rejected the same
// way before any decode work. Filters are deliberately not bound into the
// token (reusing a cursor under different filters grants nothing the
// filters don't already); keeping them stable across pages is the
// caller's side of the contract.
//
// The keyset is composite — (cursorField, rid) — so non-unique sort
// columns (created_at and friends) never skip rows that share a boundary
// value: the public RID breaks ties. The tie-breaker binds to the model's
// RID column directly, independent of the query allowlist — stores that
// don't expose "id" (or alias it elsewhere) paginate all the same.
// cursorField is code-chosen, validated against the query whitelist (an
// undeclared name is a server-side bug — the raw where.ErrUnknownField
// surfaces as a 500, not client input) and MUST be a
// NOT NULL column whose token kind is statically derivable: plain or
// defined scalars (strings, ints, uints, floats, bools, time.Time),
// pointers to those, and serializer or driver.Valuer fields whose
// zero-value probe yields a scalar wire sample — the probe runs the
// encoder's exact pipeline, so gorm.io/datatypes.Time (wire string) and
// `serializer:unixtime` fields (wire time.Time) work, while sql.Null*
// wrappers (zero value is NULL) and []byte are rejected up front. Custom
// serializer/driver.Valuer fields must keep that wire kind stable for all
// values; a runtime drift from the zero probe is rejected before signing.
// When the lookahead has proven a next page exists, a NULL, NaN or
// non-RFC3339-representable time boundary — or a string boundary longer
// than MaxCursorValueLen (1024 bytes; a cursor key, not a payload) —
// returns an error rather than silently ending — or poisoning — the
// client's pagination.
//
// size is the max items per page. Additional opts may add FILTERS ONLY:
// the cursor owns ORDER BY, LIMIT and the size+1 lookahead, so ordering,
// pagination, count and page-size-cap options (WithOrder / WithPage /
// WithLimit / WithOffset / WithCount / WithMaxPageSize / nested cursors)
// are rejected as apierr.ErrInvalidArgument — a stray order breaks the
// keyset invariant, an OFFSET reintroduces the drift keyset pagination
// exists to avoid, and a tighter cap silently clips the lookahead so the
// page reports "no more rows" while rows remain. The guard runs inside the
// scoped query build, so custom options execute exactly once.
//
// Example:
//
//	page, err := s.ListWithCursor(ctx, "created_at", where.CursorAfter, "", 20)
//	// render page.Items; hand page.NextCursor to the client
//	page, err = s.ListWithCursor(ctx, "created_at", where.CursorAfter, page.NextCursor, 20)
func (s *Store[T]) ListWithCursor(ctx context.Context, cursorField string, direction where.CursorDirection, cursor string, size int, opts ...where.Option) (*CursorPage[T], error) {
	// Direct-parameter validation goes through mapQueryError like every
	// option-borne error: size and direction are routinely fed from client
	// pagination input, and the raw where sentinel is not something
	// store.MapError recognises — an unmapped return would surface as a 500
	// from the handler layer instead of the invalid-argument 400 the
	// guard-rejected options already produce.
	if size < 1 {
		return nil, mapQueryError(fmt.Errorf("%w: cursor page size %d, must be >= 1", where.ErrInvalidParam, size))
	}
	// Enforce the caller-visible size before deriving the private size+1
	// lookahead. Reserve one slot below the package ceiling so the lookahead
	// remains a valid where limit; it is never returned to the caller.
	cap := s.maxPageSize
	if cap <= 0 || cap > where.MaxPageSize-1 {
		cap = where.MaxPageSize - 1
	}
	if size > cap {
		return nil, mapQueryError(fmt.Errorf("%w: cursor page size %d exceeds maximum %d", where.ErrInvalidParam, size, cap))
	}
	// Validate direction before decoding the token: a malformed direction
	// must fail as its own error rather than masquerade as a token
	// direction mismatch.
	if direction != where.CursorAfter && direction != where.CursorBefore {
		return nil, mapQueryError(fmt.Errorf("%w: cursor direction must be 'after' or 'before'", where.ErrInvalidParam))
	}
	// Resolve the cursor field's token expectation up front, with the same
	// Valuer-aware classification the encoder uses. An unknown field is a
	// server-side bug like on every programmatic entry point (the raw
	// where.ErrUnknownField passes through — cursorField is code-chosen,
	// not client input); a field whose token kind cannot be derived
	// statically is rejected before any token is issued or accepted —
	// signing a token the next page cannot consume, or skipping
	// forged-kind validation, are both worse than a loud configuration
	// error.
	col, err := where.ResolveField(s.queryFieldMap, cursorField)
	if err != nil {
		return nil, mapQueryError(err)
	}
	fieldSchema := s.modelSchema.LookUpField(col)
	if fieldSchema == nil {
		return nil, fmt.Errorf("store: ListWithCursor: cursor field %q missing from model schema", cursorField)
	}
	spec, ok := cursorSpecForSchemaField(s.modelSchema.ModelType, fieldSchema)
	if !ok {
		return nil, fmt.Errorf("store: ListWithCursor: field %q (type %s) cannot key a cursor: its token kind is not statically derivable (unsupported scalar shape, or a serializer/driver.Valuer whose zero-value probe yields no scalar wire sample); pick a NOT NULL scalar cursor field", cursorField, fieldSchema.FieldType)
	}
	// Fetch size+1 to detect whether there's a next page.
	fetchSize := size + 1
	allOpts := make([]where.Option, 0, len(opts)+3)
	allOpts = append(allOpts, opts...)
	// The FILTERS-ONLY guard sits between the caller options and the
	// cursor's own: by the time it runs, Config reflects the caller
	// options alone, and it executes inside the one real, scoped
	// where.Apply — no pre-flight probe that would run stateful custom
	// options twice or hand them an unscoped query.
	allOpts = append(allOpts, cursorFilterOnlyGuard())
	// Cap the internal query at exactly the requested page plus its lookahead.
	// The execution call below deliberately skips reinjecting s.maxPageSize:
	// size was already checked against it, and applying it again would discard
	// the lookahead when size equals the Store cap. Caller options cannot
	// tighten this internal limit — the FILTERS-ONLY guard above rejects any
	// caller WithMaxPageSize, precisely because a tighter cap would clip the
	// lookahead and end pagination with rows remaining.
	allOpts = append(allOpts, where.WithMaxPageSize(fetchSize))
	// Decode the opaque token (empty = first page). The token's kind tag is
	// client-forgeable — the schema-derived spec from the gate above is the
	// type source of truth, and the value is range-validated at the field's
	// declared width.
	var fieldCursor, tieCursor any
	if cursor != "" {
		value, rid, err := decodeCursor(cursor, cursorField, direction, spec)
		if err != nil {
			return nil, mapQueryError(err)
		}
		fieldCursor, tieCursor = value, rid
	}
	// The tie-breaker is the store's RID column, bound directly — never
	// through the public allowlist, which may not expose "id" at all or
	// may alias it to another column.
	allOpts = append(allOpts, where.WithCursorByField(cursorField, direction, fieldCursor, s.ridColumn, tieCursor, fetchSize))

	result, err := s.listInternalWithMaxPageSize(ctx, nil, allOpts, 0)
	if err != nil {
		return nil, err
	}

	page := &CursorPage[T]{Items: result.Items}
	if len(result.Items) > size {
		page.Items = result.Items[:size]
		// The lookahead proved a next page exists — an encode failure must
		// surface, not degrade into "no more pages".
		next, err := s.encodeItemCursor(result.Items[size-1], cursorField, direction, spec)
		if err != nil {
			return nil, err
		}
		page.NextCursor = next
	}
	return page, nil
}

// cursorFilterOnlyGuard enforces ListWithCursor's FILTERS-ONLY contract for
// caller options. Any pagination, ordering, count or page-size-cap signal
// means the caller tried to steer what the cursor arguments own: a stray
// ORDER BY takes precedence over the keyset ordering, an OFFSET
// reintroduces drift, a COUNT is wasted work CursorPage cannot carry, and a
// tighter MaxPageSize clips the size+1 lookahead — the page would report
// "no more rows" (empty NextCursor) while rows remain. The error surfaces
// through mapQueryError as apierr.ErrInvalidArgument, like every other
// invalid query option.
func cursorFilterOnlyGuard() where.Option {
	return func(db *gorm.DB, cfg *where.Config, _ map[string]string) (*gorm.DB, error) {
		if cfg.HasPage || cfg.HasCursor || cfg.HasOrder || cfg.Count || cfg.MaxPageSize > 0 {
			return nil, fmt.Errorf("%w: ListWithCursor accepts filter options only; ordering, pagination, count and page-size caps come from the cursor arguments", where.ErrInvalidParam)
		}
		return db, nil
	}
}

// Tx runs fn inside a transaction scoped to this Store. fn receives a
// Store clone bound to the transaction — its operations hit the
// transaction no matter which context they are called with, and its
// WithBus events stage on the transaction's after-commit buffer.
//
// If a context-scoped transaction owned by this Store's database handle is
// already active (via db.RunInTx), Tx reuses it instead of opening a nested
// transaction. Cross-store atomic writes on the same handle are context
// propagation's job: inside a db.RunInTx callback, call those stores with
// txCtx and they join the same transaction. Transactions never cross handles.
func (s *Store[T]) Tx(ctx context.Context, fn func(tx *Store[T]) error) error {
	return db.RunInTx(ctx, s.h, func(txCtx context.Context) error {
		return fn(s.txClone(txCtx))
	})
}

// Unsafe returns a raw *gorm.DB — transaction-aware (context
// same-handle context transaction first, then a Tx clone's binding, then the root pool)
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
	cp.txDB = txctx.DB(txCtx, s.h)
	cp.txCtx = txCtx
	return &cp
}

// effectiveDB returns the *gorm.DB for the current operation:
// the context's transaction when one is active (db.RunInTx
// propagation), else the clone's pinned transaction (Store.Tx), else
// the root pool. The ordering matches v1: an explicit transactional
// context always wins.
func (s *Store[T]) effectiveDB(ctx context.Context) *gorm.DB {
	if tx := txctx.DB(ctx, s.h); tx != nil {
		return tx.WithContext(ctx)
	}
	if s.txDB != nil {
		return s.txDB.WithContext(ctx)
	}
	return s.h.Unsafe(ctx)
}

func (s *Store[T]) rejectWrite(op string) error {
	if !s.readOnly {
		return nil
	}
	return fmt.Errorf("store: %s: %w", op, db.ErrReadOnly)
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

// mapQueryError classifies query-building errors on the PROGRAMMATIC
// entry points (List / ListQ / Count / Pluck* / ListIn / ListWithCursor
// and the locator-based Get / GetForUpdate / Update / Delete / Restore /
// Exists), splitting them by provenance:
//
//   - Value errors (where.ErrInvalidParam) → apierr.ErrInvalidArgument
//     (400). Pages, sizes, cursor tokens and filter VALUES routinely
//     flow from clients through handler code into these entry points.
//   - Field-NAME errors (where.ErrUnknownField) pass through raw → 500.
//     On these entry points field names are written by server code, so
//     an unknown field is a programming bug that must alarm monitoring,
//     not masquerade as a client mistake. The chain is preserved —
//     errors.Is(err, where.ErrUnknownField) works on the return.
//     Client-supplied field names belong behind ListFromQuery (which
//     validates them as input), never spliced into WithFilter/WithOrder.
//   - Configuration errors (where.ErrFieldNotConfigured) pass through
//     as server bugs (500), as always.
func mapQueryError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, where.ErrInvalidParam) {
		return apierr.ErrInvalidArgument.WithMessage(err.Error())
	}
	return err
}

// mapClientQueryError classifies the same errors on the CLIENT entry
// point — the ListFromQuery chain, where field names come from the URL:
// both value errors and field-name errors are client input there and map
// to apierr.ErrInvalidArgument (400). where.ErrFieldNotConfigured still
// passes through — an unconfigured store is a server bug no matter who
// asked. Idempotent over already-mapped *apierr.Error values, which no
// longer match the where sentinels.
func mapClientQueryError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, where.ErrInvalidParam) || errors.Is(err, where.ErrUnknownField) {
		return apierr.ErrInvalidArgument.WithMessage(err.Error())
	}
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

// mapError is the Store-aware layer over the package mapError: it applies
// the WithConstraintFields declaration to duplicate errors, resolving the
// violated constraint to the public field name the API should blame. The
// resolution must happen here — package-level MapError is registered
// app-wide and has no idea which Store produced the error, so the mapping
// rides the error value itself.
func (s *Store[T]) mapError(err error) error {
	mapped := mapError(err)
	if len(s.constraintFields) == 0 {
		return mapped
	}
	var dup *DuplicateEntryError
	if errors.As(mapped, &dup) && dup.Field == "" {
		if constraint := extractConstraintName(dup.Detail); constraint != "" {
			if field, ok := lookupConstraintField(s.constraintFields, constraint); ok {
				dup.Field = field
			}
		}
	}
	return mapped
}

// Base model fields that must never be updated.
var baseModelExclude = map[string]bool{
	"id": true, "version": true, "created_at": true, "updated_at": true,
}

// protectedUpdateColumns are owned by Store write invariants rather than
// application payloads. Ownership changes and lifecycle repair remain possible
// through the explicitly unsafe GORM door, where they are auditable as such.
var protectedUpdateColumns = map[string]struct{}{
	"id":           {},
	"rid":          {},
	"version":      {},
	"created_at":   {},
	"updated_at":   {},
	"deleted_at":   {},
	"delete_token": {},
	"owner_id":     {},
}

func isProtectedUpdateColumn(modelSchema *schema.Schema, column string) bool {
	if _, protected := protectedUpdateColumns[column]; protected {
		return true
	}
	if modelSchema == nil {
		return false
	}
	for _, name := range []string{
		"ID", "RID", "Version", "CreatedAt", "UpdatedAt",
		"DeletedAt", "DeleteToken", "OwnerID",
	} {
		if field := modelSchema.LookUpField(name); field != nil && field.DBName == column {
			return true
		}
	}
	return false
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
func discoverFields(modelSchema *schema.Schema, exclude []string) (map[string]string, []string) {
	ex := toSet(exclude)
	for k := range baseQueryExclude {
		ex[k] = true
	}
	return discoverSchemaFields(modelSchema, ex, true)
}

// discoverUpdateFields builds an updateFieldMap from JSON tags.
// Excludes json:"-", base model fields (id/version/timestamps), and user-specified names.
// Does NOT exclude text/blob (updating content is normal).
// Returns collisions — see discoverFields.
func discoverUpdateFields(modelSchema *schema.Schema, exclude []string) (map[string]string, []string) {
	ex := toSet(exclude)
	for k := range baseModelExclude {
		ex[k] = true
	}
	return discoverSchemaFields(modelSchema, ex, false)
}

// storeTagName is the struct tag store.New reads for model-declared
// field allowlists: `store:"query"`, `store:"update"` or both.
const storeTagName = "store"

// tagDeclaredFields collects the model's own `store` tag declaration.
// Returned maps are filter-name → column. tagged reports whether any
// field carries a store tag — when false both maps are nil and the
// caller falls back to discovery.
//
// The filter name is the field's JSON name; fields hidden from JSON
// (no tag or json:"-") fall back to GORM's parsed DBName, so an
// internal column can still be declared queryable. Embedded chok base
// models contribute their standard queryable fields (the JSON-visible
// set discovery would expose, minus version) to the query side only —
// update lists never gain base-model fields. Malformed tag values and
// duplicate names mapping to different columns panic: a declaration
// typo must fail construction, not silently narrow the surface.
func tagDeclaredFields(t reflect.Type, modelSchema *schema.Schema) (query, update map[string]string, tagged bool) {
	query = make(map[string]string)
	update = make(map[string]string)
	for _, field := range modelSchema.Fields {
		if field.DBName == "" {
			continue
		}
		tag, ok := field.StructField.Tag.Lookup(storeTagName)
		if !ok {
			continue
		}
		tagged = true
		name := storeTagFieldName(field)
		for _, raw := range strings.Split(tag, ",") {
			switch strings.TrimSpace(raw) {
			case "query":
				addDeclaredField(query, name, field.DBName, t, field.StructField)
			case "update":
				addDeclaredField(update, name, field.DBName, t, field.StructField)
			default:
				panic(fmt.Sprintf("store: %s.%s: bad `store:%q` tag value %q — use \"query\", \"update\" or both (remove the tag to keep the field private)",
					t.Name(), field.StructField.Name, tag, strings.TrimSpace(raw)))
			}
		}
	}
	if !tagged {
		return nil, nil, false
	}
	// Every Store model embeds db.Model. Its lifecycle fields contribute
	// the standard query surface, resolved through the same parsed schema
	// GORM will use for SQL.
	for name, goName := range map[string]string{
		"id": "RID", "created_at": "CreatedAt", "updated_at": "UpdatedAt",
	} {
		if _, exists := query[name]; exists {
			continue
		}
		if field := modelSchema.LookUpField(goName); field != nil && field.DBName != "" {
			query[name] = field.DBName
		}
	}
	return query, update, true
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
func storeTagFieldName(field *schema.Field) string {
	name, _, _ := strings.Cut(field.StructField.Tag.Get("json"), ",")
	if name == "" || name == "-" {
		return field.DBName
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

// discoverSchemaFields is the field-discovery workhorse. GORM's parsed
// Schema is authoritative for DBName, including acronym splitting, explicit
// column tags, embedded prefixes, and custom naming strategies.
func discoverSchemaFields(modelSchema *schema.Schema, exclude map[string]bool, skipLarge bool) (map[string]string, []string) {
	out := make(map[string]string)
	var collisions []string
	for _, field := range modelSchema.Fields {
		if field.DBName == "" {
			continue
		}
		f := field.StructField
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

		col := field.DBName
		if existing, ok := out[name]; ok && existing != col {
			collisions = append(collisions, fmt.Sprintf("%q (columns %q vs %q)", name, existing, col))
		}
		out[name] = col
	}
	return out, collisions
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

// DuplicateError is an optional interface that database drivers or error
// wrappers can implement for reliable duplicate-key detection without
// string matching. When the error chain contains a DuplicateError
// implementation, isDuplicateError trusts it over heuristics.
type DuplicateError interface {
	IsDuplicate() bool
}

type sqliteErrorCoder interface {
	Code() int
}

const (
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
	sqliteConstraintRowID      = 2579
)

// isDuplicateError detects duplicate key errors using a tiered strategy:
//
//  1. DuplicateError interface — most reliable, no string dependency.
//  2. Typed pgx error — SQLSTATE 23505 (unique_violation) is Postgres's
//     authoritative signal; any other PG code is authoritatively NOT a
//     duplicate (M3, SPEC §5.3 error-mapping acceptance).
//  3. SQLite extended result code — UNIQUE/PRIMARYKEY/ROWID only; other
//     constraint families are authoritatively not duplicates.
//  4. GORM's ErrDuplicatedKey (v1.25.0+) — covers drivers that translate
//     into GORM's sentinel via translate plugin.
//  5. Narrow string matching fallback — catches MySQL and unknown-driver
//     duplicate/unique messages without matching generic constraint failures.
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
	// glebarez/modernc SQLite exposes extended result codes. Treat those as
	// authoritative: generic CONSTRAINT (NOT NULL, CHECK, FK) is not a
	// duplicate, while PRIMARYKEY/UNIQUE/ROWID is.
	var sqliteErr sqliteErrorCoder
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case sqliteConstraintPrimaryKey, sqliteConstraintUnique, sqliteConstraintRowID:
			return true
		default:
			return false
		}
	}
	// Tier 4: GORM sentinel.
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	// Tier 5: string heuristic (backward compatibility).
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "unique_violation")
}

// pgUniqueViolation is SQLSTATE class 23, unique_violation.
const pgUniqueViolation = "23505"
