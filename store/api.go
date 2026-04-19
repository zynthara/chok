package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/rid"
)

// --- Options (shared by Update and Delete) ---------------------------------

// UpdateOption tunes Update behaviour beyond what Changes expresses.
type UpdateOption interface {
	applyUpdate(*updateConfig)
}

// DeleteOption tunes Delete behaviour.
type DeleteOption interface {
	applyDelete(*deleteConfig)
}

type updateConfig struct {
	version    int
	versionSet bool
}

type deleteConfig struct {
	version    int
	versionSet bool
}

// WithVersion enables optimistic locking by asserting the row's current
// version column equals v. The returned option satisfies both UpdateOption
// and DeleteOption.
//
// Use WithVersion when Changes is Set(map) — the map carries no version
// field, so the lock must be supplied separately. For Fields(&obj) the
// version is extracted from obj.Version automatically; WithVersion there
// overrides the implicit value (rare).
//
// A version <= 0 is treated as "no lock" (idempotent semantics).
func WithVersion(v int) versionOpt {
	return versionOpt(v)
}

type versionOpt int

func (v versionOpt) applyUpdate(c *updateConfig) {
	c.version = int(v)
	c.versionSet = true
}

func (v versionOpt) applyDelete(c *deleteConfig) {
	c.version = int(v)
	c.versionSet = true
}

// --- Query options (shared by Get and List) ---------------------------------

// QueryOption configures a Get or List query with additional clauses that
// sit outside the where DSL (preloads, joins, etc.).
type QueryOption func(*queryConfig)

type queryConfig struct {
	preloads    []string
	withTrashed bool // include soft-deleted records
	onlyTrashed bool // ONLY soft-deleted records
}

// WithPreload eagerly loads a named association using GORM's Preload. The
// relation name must match the Go struct field name (e.g. "Author", "Tags").
// Multiple WithPreload options can be combined.
//
// Scope propagation: the store's scopes (OwnerScope + any custom scope)
// are re-applied to the association query, so preloading never leaks
// rows owned by other principals. This is safer than s.DB().Preload()
// which would bypass scopes entirely. Custom scopes whose predicates
// don't make sense on the associated table must detect this via ctx
// and return q unchanged to opt out; the scope function itself is the
// single point of enforcement.
//
//	post, err := s.Get(ctx, store.RID(rid), store.WithPreload("Author"))
//	page, err := s.List(ctx, where.WithCount(), store.WithPreload("Tags"))
func WithPreload(relation string) QueryOption {
	return func(qc *queryConfig) {
		qc.preloads = append(qc.preloads, relation)
	}
}

// WithTrashed includes soft-deleted records in the query result. GORM
// normally adds `WHERE deleted_at IS NULL` automatically for models with
// a DeletedAt field; this option removes that filter. Scopes (OwnerScope
// etc.) are still applied — soft-deleted records are visible but not
// unprotected.
//
// Use this in admin/audit views that need to see deleted records.
func WithTrashed() QueryOption {
	return func(qc *queryConfig) { qc.withTrashed = true }
}

// WithOnlyTrashed returns ONLY soft-deleted records. Useful for "trash"
// views or data recovery workflows. Like WithTrashed, all scopes still
// apply.
func WithOnlyTrashed() QueryOption {
	return func(qc *queryConfig) { qc.onlyTrashed = true }
}

// applyQueryOpts applies QueryOptions to a GORM query. ctx carries the
// principal; the store's scopes are re-applied to each preload query so
// OwnerScope / custom scopes don't leak associated rows owned by other
// principals. If any scope returns an error when applied to a preload,
// that preload is rewritten to match zero rows (fail-closed).
func (s *Store[T]) applyQueryOpts(ctx context.Context, q *gorm.DB, opts []QueryOption) *gorm.DB {
	if len(opts) == 0 {
		return q
	}
	cfg := &queryConfig{}
	for _, o := range opts {
		o(cfg)
	}
	for _, p := range cfg.preloads {
		q = q.Preload(p, func(pq *gorm.DB) *gorm.DB {
			scoped, err := s.applyScopes(ctx, pq)
			if err != nil {
				// Fail-closed on scope errors (e.g. unauthenticated ctx
				// reaching OwnerScope): render no rows rather than
				// fall back to an unscoped preload.
				return pq.Where("1 = 0")
			}
			return scoped
		})
	}
	// Soft-delete control: Unscoped removes GORM's automatic
	// `WHERE deleted_at IS NULL`. OwnerScope and custom scopes are
	// applied separately via applyScopes and are NOT affected.
	if cfg.onlyTrashed {
		q = q.Unscoped().Where("deleted_at IS NOT NULL")
	} else if cfg.withTrashed {
		q = q.Unscoped()
	}
	return q
}

// --- New unified CRUD methods ---------------------------------------------

// Get retrieves a single record matched by the locator. Returns ErrNotFound
// if no record matches. Scopes (OwnerScope, custom) are applied before the
// locator's WHERE. Optional QueryOption (e.g. WithPreload) can be appended.
//
// Examples:
//
//	store.Get(ctx, store.RID("usr_abc"))
//	store.Get(ctx, store.ID(42), store.WithPreload("Author"))
func (s *Store[T]) Get(ctx context.Context, by Locator, opts ...QueryOption) (*T, error) {
	q, err := s.applyScopes(ctx, s.effectiveDB(ctx))
	if err != nil {
		return nil, err
	}
	q = s.applyQueryOpts(ctx, q, opts)
	q, err = by.apply(q, s.queryFieldMap)
	if err != nil {
		return nil, mapQueryError(err)
	}
	var obj T
	if err := q.First(&obj).Error; err != nil {
		mapped := mapError(err)
		if errors.Is(mapped, ErrNotFound) {
			return nil, newNotFoundError(by)
		}
		return nil, mapped
	}
	return &obj, nil
}

// Update modifies the record(s) matched by the locator using the described
// changes. Optimistic locking is automatic when Changes is Fields(&obj) and
// obj embeds db.Model; use WithVersion for explicit locking with Set(map),
// or .NoLock() on Fields to skip the lock.
//
// Returns:
//   - ErrNotFound        when no row matches the locator
//   - ErrStaleVersion    when the lock version is stale (row exists but
//     version mismatch)
//   - ErrUnknownUpdateField / ErrMissingColumns  for invalid Changes
//
// Zero values in Fields(&obj) ARE persisted — the Store uses Select() to
// bypass GORM's default "skip zero values" behaviour.
func (s *Store[T]) Update(ctx context.Context, by Locator, changes Changes, opts ...UpdateOption) error {
	if changes == nil {
		return ErrMissingColumns
	}

	for _, h := range s.hooks.beforeUpdate {
		if err := h(ctx, by, changes); err != nil {
			return err
		}
	}

	cols, payload, implicitVer, err := changes.build(s.updateFieldMap)
	if err != nil {
		return err
	}

	// Resolve the effective lock version: explicit WithVersion wins over the
	// implicit one extracted from Fields(&obj).
	cfg := &updateConfig{}
	for _, o := range opts {
		o.applyUpdate(cfg)
	}
	lockVer := implicitVer
	if cfg.versionSet {
		lockVer = cfg.version
	}

	// Build the query: scope + locator + optional version guard.
	q, err := s.applyScopes(ctx, s.effectiveDB(ctx))
	if err != nil {
		return err
	}
	q, err = by.apply(q, s.queryFieldMap)
	if err != nil {
		return mapQueryError(err)
	}
	if lockVer > 0 {
		q = q.Where("version = ?", lockVer)
		cols = append(cols, "version")
	}

	// Branch on payload shape.
	rv := reflect.ValueOf(payload)
	if rv.Kind() == reflect.Map {
		m := payload.(map[string]any)
		if lockVer > 0 {
			// Clone before injecting "version" so we don't mutate the
			// caller's map. Re-using the same map across retries would
			// double-apply version + 1 and break optimistic locking.
			cloned := make(map[string]any, len(m)+1)
			for k, v := range m {
				cloned[k] = v
			}
			cloned["version"] = gorm.Expr("version + 1")
			m = cloned
		}
		result := q.Model(new(T)).Select(cols).Updates(m)
		return s.finalizeUpdate(ctx, by, result, lockVer, changes)
	}

	// Struct payload path (Fields).
	//
	// GORM Updates(struct) reads the Version field off the passed struct
	// to write the new value, which means we have to bump modelPtr.Version
	// BEFORE executing the UPDATE. If the UPDATE fails (conflict or DB
	// error) we restore the old value. Callers that share `obj` across
	// goroutines would briefly observe the bumped value during the
	// UPDATE; Update is single-caller-per-object by contract and tests
	// assume that invariant — this note exists to make the contract
	// explicit rather than implicit.
	modelPtr := extractModelSafe(payload)
	if lockVer > 0 {
		if modelPtr == nil {
			return fmt.Errorf("store: optimistic lock requested but payload does not embed db.Model")
		}
		modelPtr.Version = lockVer + 1
	}
	result := q.Model(new(T)).Select(cols).Updates(payload)
	if lockVer > 0 && (result.Error != nil || result.RowsAffected == 0) {
		modelPtr.Version = lockVer
	}
	return s.finalizeUpdate(ctx, by, result, lockVer, changes)
}

// Delete removes the record(s) matched by the locator. Soft-delete models get
// deleted_at + a fresh delete_token; regular models are physically deleted.
//
// Without WithVersion, Delete is idempotent — zero matches returns nil.
// With WithVersion, a zero-match row that exists returns ErrStaleVersion;
// a truly absent row returns ErrNotFound.
func (s *Store[T]) Delete(ctx context.Context, by Locator, opts ...DeleteOption) error {
	for _, h := range s.hooks.beforeDelete {
		if err := h(ctx, by); err != nil {
			return err
		}
	}

	cfg := &deleteConfig{}
	for _, o := range opts {
		o.applyDelete(cfg)
	}

	q, err := s.applyScopes(ctx, s.effectiveDB(ctx))
	if err != nil {
		return err
	}
	q, err = by.apply(q, s.queryFieldMap)
	if err != nil {
		return mapQueryError(err)
	}
	if cfg.versionSet && cfg.version > 0 {
		q = q.Where("version = ?", cfg.version)
	}

	var result *gorm.DB
	if s.soft {
		result = q.Model(new(T)).Updates(map[string]any{
			"deleted_at":   gorm.Expr("CURRENT_TIMESTAMP"),
			"delete_token": rid.NewRaw(),
		})
	} else {
		result = q.Delete(new(T))
	}

	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 && cfg.versionSet && cfg.version > 0 {
		// Unscoped: distinguish "row was soft-deleted by another txn"
		// (→ StaleVersion) from "locator never matched" (→ NotFound).
		exists, err := s.existsByLocatorUnscoped(ctx, by)
		if err != nil {
			return err
		}
		if !exists {
			return newNotFoundError(by)
		}
		return newVersionConflictError(by, cfg.version)
	}
	// Only fire after-hooks when a row was actually deleted. A no-op
	// idempotent delete (RowsAffected==0 without version) should not
	// trigger audit/cache hooks.
	if result.RowsAffected > 0 {
		for _, h := range s.hooks.afterDelete {
			h := h
			s.safeAfterHook(func() { h(ctx, by) })
		}
	}
	return nil
}

// Exists checks whether any record matches the locator under the Store's
// scopes. More efficient than Get when you only need presence, not the data.
func (s *Store[T]) Exists(ctx context.Context, by Locator) (bool, error) {
	return s.existsByLocator(ctx, by)
}

// Upsert inserts obj or updates it on conflict. conflictColumns are the
// unique constraint columns (resolved via the query field whitelist) that
// trigger the "update" path. updateColumns are the columns to update on
// conflict (resolved via the update field whitelist). When updateColumns
// is empty, all update-whitelisted columns are updated.
//
// Before-create hooks fire before the SQL. Owner auto-fill applies.
//
// Upsert is forbidden on scoped Stores AND on Stores whose model embeds
// db.Owned (even when OwnerScope is disabled via WithoutOwnerScope). SQL
// ON CONFLICT UPDATE does not apply the owner_id WHERE filter to the
// update path, so an attacker providing a conflicting key that belongs
// to another user could mutate the victim's row. Use Create + detect
// ErrDuplicate + Update as an explicit alternative.
func (s *Store[T]) Upsert(ctx context.Context, obj *T, conflictColumns []string, updateColumns ...string) error {
	if len(s.scopes) > 0 {
		return ErrUpsertScoped
	}
	if db.IsOwnedModel(new(T)) {
		return ErrUpsertScoped
	}
	if err := fillOwner(ctx, obj, s.requirePrincipal); err != nil {
		return err
	}
	for _, h := range s.hooks.beforeCreate {
		if err := h(ctx, obj); err != nil {
			return err
		}
	}

	// Resolve conflict columns to DB column names via the query whitelist.
	gormCols := make([]clause.Column, len(conflictColumns))
	for i, name := range conflictColumns {
		col, ok := s.queryFieldMap[name]
		if !ok {
			return fmt.Errorf("store: unknown conflict column %q: not in query whitelist", name)
		}
		gormCols[i] = clause.Column{Name: col}
	}

	// Resolve update columns.
	var doUpdateCols []string
	if len(updateColumns) > 0 {
		doUpdateCols = make([]string, 0, len(updateColumns))
		for _, name := range updateColumns {
			col, ok := s.updateFieldMap[name]
			if !ok {
				return fmt.Errorf("%w: %q", ErrUnknownUpdateField, name)
			}
			doUpdateCols = append(doUpdateCols, col)
		}
	} else {
		doUpdateCols = make([]string, 0, len(s.updateFieldMap))
		for _, col := range s.updateFieldMap {
			doUpdateCols = append(doUpdateCols, col)
		}
	}

	if err := s.effectiveDB(ctx).Clauses(clause.OnConflict{
		Columns:   gormCols,
		DoUpdates: clause.AssignmentColumns(doUpdateCols),
	}).Create(obj).Error; err != nil {
		return mapError(err)
	}
	for _, h := range s.hooks.afterCreate {
		h := h
		s.safeAfterHook(func() { h(ctx, obj) })
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func (s *Store[T]) finalizeUpdate(ctx context.Context, by Locator, result *gorm.DB, lockVer int, changes Changes) error {
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		if lockVer > 0 {
			// Use the unscoped variant so a concurrently soft-deleted
			// row still reports "exists but stale version" rather than
			// collapsing to NotFound (which would erase the version
			// drift signal from the caller's perspective).
			exists, err := s.existsByLocatorUnscoped(ctx, by)
			if err != nil {
				return err
			}
			if !exists {
				return newNotFoundError(by)
			}
			return newVersionConflictError(by, lockVer)
		}
		return newNotFoundError(by)
	}
	for _, h := range s.hooks.afterUpdate {
		h := h
		s.safeAfterHook(func() { h(ctx, by, changes) })
	}
	return nil
}

// existsByLocator checks whether any row currently matches the locator
// under the Store's scopes and the active soft-delete rules (i.e.
// excludes soft-deleted rows). Callers: the public Exists method.
//
// Uses SELECT 1 ... LIMIT 1 instead of COUNT(*) for efficiency — only needs
// to find one matching row, not scan the entire result set.
func (s *Store[T]) existsByLocator(ctx context.Context, by Locator) (bool, error) {
	return s.existsByLocatorInternal(ctx, by, false)
}

// existsByLocatorUnscoped is like existsByLocator but includes soft-
// deleted rows. Used to disambiguate "row was never there" from "row
// exists but is stale/soft-deleted" when an optimistic update affects
// zero rows. Without .Unscoped(), a row soft-deleted concurrently with
// the version-mismatched update would surface as ErrNotFound — the
// caller loses the information that the write was blocked by version
// drift rather than a bad locator.
func (s *Store[T]) existsByLocatorUnscoped(ctx context.Context, by Locator) (bool, error) {
	return s.existsByLocatorInternal(ctx, by, true)
}

func (s *Store[T]) existsByLocatorInternal(ctx context.Context, by Locator, includeSoftDeleted bool) (bool, error) {
	base := s.effectiveDB(ctx)
	if includeSoftDeleted {
		base = base.Unscoped()
	}
	q, err := s.applyScopes(ctx, base)
	if err != nil {
		return false, err
	}
	q, err = by.apply(q, s.queryFieldMap)
	if err != nil {
		return false, mapQueryError(err)
	}
	var dummy int
	result := q.Model(new(T)).Select("1").Limit(1).Scan(&dummy)
	if result.Error != nil {
		return false, mapError(result.Error)
	}
	return result.RowsAffected > 0, nil
}
