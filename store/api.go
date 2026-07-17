package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/internal/txctx"
	"github.com/zynthara/chok/v2/rid"
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
	affected   *int64
}

type deleteConfig struct {
	version    int
	versionSet bool
	affected   *int64
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

// WithRowsAffected reports how many rows the statement touched into
// *dst. Like WithVersion it satisfies both UpdateOption and
// DeleteOption — the observability story for bulk writes through the
// Where locator, where "how many" is the caller's business outcome:
//
//	var n int64
//	err := s.Delete(ctx, store.Where(
//	    where.WithFilterOp("updated_at", where.Lt, cutoff),
//	), store.WithRowsAffected(&n))
//
// *dst is written on every execution: the affected count when the SQL
// ran (0 together with ErrNotFound / ErrStaleVersion when nothing
// matched), 0 when the statement itself failed. The option observes —
// it never changes Update/Delete semantics.
//
// Panics if dst is nil (configuration error, caught at construction).
func WithRowsAffected(dst *int64) rowsAffectedOpt {
	if dst == nil {
		panic("store: WithRowsAffected dst must not be nil")
	}
	return rowsAffectedOpt{dst: dst}
}

type rowsAffectedOpt struct{ dst *int64 }

func (o rowsAffectedOpt) applyUpdate(c *updateConfig) { c.affected = o.dst }
func (o rowsAffectedOpt) applyDelete(c *deleteConfig) { c.affected = o.dst }

// recordAffected writes result's row count to dst (0 on statement
// error) — shared by the Update payload branches and Delete.
func recordAffected(dst *int64, result *gorm.DB) {
	if dst == nil {
		return
	}
	if result.Error != nil {
		*dst = 0
		return
	}
	*dst = result.RowsAffected
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
	return s.getInternal(ctx, by, opts, false)
}

// GetForUpdate retrieves a single record like Get and locks it against
// concurrent lockers and writers until the enclosing transaction ends
// (SELECT ... FOR UPDATE). It is the pessimistic counterpart to Update's
// automatic optimistic locking: reach for it when a read-modify-write
// sequence must win against concurrency instead of retrying on
// ErrStaleVersion.
//
// It must run inside a transaction on the store's own handle — the tx
// Store handed to Store.Tx's callback, or a context from db.RunInTx on
// the same *db.DB. Outside one it returns ErrLockRequiresTx: a row lock
// under autocommit is released before the caller can act on the row, so
// the entry point enforces what the guarantee needs. Read-only stores
// return db.ErrReadOnly — a lock is write intent.
//
// Dialects: PostgreSQL and MySQL render FOR UPDATE and block concurrent
// lockers/writers of the row until commit. SQLite has no row locks and
// its driver drops the clause — but chok's SQLite shape routes every
// transaction onto the single write connection opened with
// _txlock=immediate, so the enclosing transaction already holds the
// database write lock: strictly stronger than a row lock. The observable
// guarantee — no concurrent writer between the locked read and commit —
// holds on all three dialects.
//
// WithPreload is rejected with ErrLockPreload: association rows load
// through separate queries the lock does not cover. Lock the row first,
// then load associations with a plain Get if needed.
func (s *Store[T]) GetForUpdate(ctx context.Context, by Locator, opts ...QueryOption) (*T, error) {
	if err := s.rejectWrite("GetForUpdate"); err != nil {
		return nil, err
	}
	if txctx.DB(ctx, s.h) == nil && s.txDB == nil {
		return nil, ErrLockRequiresTx
	}
	qc := &queryConfig{}
	for _, o := range opts {
		o(qc)
	}
	if len(qc.preloads) > 0 {
		return nil, ErrLockPreload
	}
	return s.getInternal(ctx, by, opts, true)
}

// getInternal is the single-row read shared by Get and GetForUpdate;
// lock appends FOR UPDATE (GetForUpdate has already verified the
// transactional context it needs).
func (s *Store[T]) getInternal(ctx context.Context, by Locator, opts []QueryOption, lock bool) (*T, error) {
	q, err := s.applyScopes(ctx, s.effectiveDB(ctx))
	if err != nil {
		return nil, err
	}
	q = s.applyQueryOpts(ctx, q, opts)
	if lock {
		q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
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
// The Changes are built — update whitelist and protected-column validation
// included — before any before-update hook runs, so static validation
// precedes user logic exactly as it does on the batch paths, and hooks
// receive the resolved ChangeSnapshot (public field names → the values
// about to be written) rather than an opaque payload.
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
	if err := s.rejectWrite("Update"); err != nil {
		return err
	}
	if changes == nil {
		return ErrMissingColumns
	}
	built, err := changes.build(ctx, s.updateFieldMap, s.modelSchema)
	if err != nil {
		return err
	}

	for _, h := range s.hooks.beforeUpdate {
		if err := h(ctx, by, built.event); err != nil {
			return err
		}
	}

	cfg := &updateConfig{}
	for _, o := range opts {
		o.applyUpdate(cfg)
	}
	return s.updateBuilt(ctx, by, built, cfg)
}

// updateBuilt is Update's hook-free write kernel over pre-built Changes.
// Callers run rejectWrite, changes.build and any before-update hooks first.
// The built payload is consumed exactly once: the version column is
// appended in place below.
func (s *Store[T]) updateBuilt(ctx context.Context, by Locator, built builtChanges, cfg *updateConfig) error {
	// Resolve the effective lock version: explicit WithVersion wins over the
	// implicit one extracted from Fields(&obj).
	lockVer := built.implicitVersion
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
	}

	// Version is a row revision, independent of whether this call asks for an
	// optimistic-lock guard. Always advance it in the same UPDATE statement.
	built.columns = append(built.columns, "version")
	built.payload["version"] = gorm.Expr("version + 1")
	result := q.Model(new(T)).Select(built.columns).Updates(built.payload)
	recordAffected(cfg.affected, result)
	if result.Error == nil && result.RowsAffected > 0 && lockVer > 0 && built.model != nil {
		built.model.Version = lockVer + 1
	}
	return s.finalizeUpdate(ctx, by, result, lockVer, built.event)
}

// Delete removes the record(s) matched by the locator. Soft-delete models get
// deleted_at + a fresh delete_token and advance the row revision in the same
// statement; regular models are physically deleted.
//
// Without WithVersion, Delete is idempotent — zero matches returns nil.
// With WithVersion, a zero-match row that exists returns ErrStaleVersion;
// a truly absent row returns ErrNotFound.
func (s *Store[T]) Delete(ctx context.Context, by Locator, opts ...DeleteOption) error {
	if err := s.rejectWrite("Delete"); err != nil {
		return err
	}
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
			"version":      gorm.Expr("version + 1"),
		})
	} else {
		result = q.Delete(new(T))
	}
	recordAffected(cfg.affected, result)

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
	// Only publish when a row was actually deleted. A no-op idempotent
	// delete (RowsAffected==0 without version) must not emit an event —
	// audit/cache subscribers would record a deletion that never
	// happened (v1's after-hook gate, carried over).
	if result.RowsAffected > 0 {
		s.publishChanged(ctx, EntityChanged[T]{Op: OpDelete, Locator: snapshotLocator(by)})
	}
	return nil
}

// Restore un-deletes the soft-deleted record(s) matched by the locator:
// deleted_at is cleared, delete_token returns to the empty-string live
// sentinel, and the row revision advances, so the row re-enters every
// SoftUnique slot — when a new live row has taken the slot in the meantime,
// the write maps to
// ErrDuplicate and the record stays deleted. Only soft-delete models
// (db.SoftDeleteModel embedders) can restore; calling Restore on a
// hard-delete model is a programming error and returns an error.
//
// Scopes apply: a principal can only restore rows its scope can see,
// and a foreign row reports ErrNotFound rather than confirming its
// existence. Idempotence mirrors Delete: a locator matching only live
// rows is a no-op nil, a locator matching nothing at all returns
// ErrNotFound.
func (s *Store[T]) Restore(ctx context.Context, by Locator) error {
	if err := s.rejectWrite("Restore"); err != nil {
		return err
	}
	if !s.soft {
		return fmt.Errorf("store: Restore: %s is not a soft-delete model (embed db.SoftDeleteModel to restore)", reflect.TypeFor[T]().Name())
	}

	q, err := s.applyScopes(ctx, s.effectiveDB(ctx).Unscoped())
	if err != nil {
		return err
	}
	q, err = by.apply(q, s.queryFieldMap)
	if err != nil {
		return mapQueryError(err)
	}

	result := q.Where("deleted_at IS NOT NULL").Model(new(T)).Updates(map[string]any{
		"deleted_at":   nil,
		"delete_token": "",
		"version":      gorm.Expr("version + 1"),
	})
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		// Distinguish "row is alive" (idempotent nil) from "no such
		// row under this scope" (ErrNotFound). The scoped probe keeps
		// foreign rows indistinguishable from absent ones.
		exists, err := s.existsByLocatorUnscoped(ctx, by)
		if err != nil {
			return err
		}
		if !exists {
			return newNotFoundError(by)
		}
		return nil
	}
	s.publishChanged(ctx, EntityChanged[T]{Op: OpRestore, Locator: snapshotLocator(by)})
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
// Before-create hooks fire after static argument validation and before the
// SQL. Owner auto-fill applies.
//
// The input object is not a persisted-row snapshot on the conflict path:
// create hooks may generate a new RID while the database keeps the existing
// row's RID. Re-read by a known business key when the persisted row is needed.
//
// Upsert is forbidden on scoped Stores AND on Stores whose model embeds
// db.Owned (even when OwnerScope is disabled via WithoutOwnerScope). SQL
// ON CONFLICT UPDATE does not apply the owner_id WHERE filter to the
// update path, so an attacker providing a conflicting key that belongs
// to another user could mutate the victim's row. Use Create + detect
// ErrDuplicate + Update as an explicit alternative.
func (s *Store[T]) Upsert(ctx context.Context, obj *T, conflictColumns []string, updateColumns ...string) error {
	if err := s.rejectWrite("Upsert"); err != nil {
		return err
	}
	if obj == nil {
		return fmt.Errorf("store: Upsert: obj is nil")
	}
	if len(s.scopes) > 0 {
		return ErrUpsertScoped
	}
	if db.IsOwnedModel(new(T)) {
		return ErrUpsertScoped
	}
	gormCols, doUpdateCols, err := s.resolveUpsertColumns(conflictColumns, updateColumns)
	if err != nil {
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

	if err := s.effectiveDB(ctx).Clauses(clause.OnConflict{
		Columns:   gormCols,
		DoUpdates: clause.AssignmentColumns(doUpdateCols),
	}).Create(obj).Error; err != nil {
		return mapError(err)
	}
	// The SQL doesn't report a portable insert-vs-update branch or a truthful
	// persisted object on every supported dialect. Publish a payload-free
	// OpUpsert so subscribers can invalidate type-wide without caching the
	// synthetic create object.
	s.publishChanged(ctx, upsertEvent[T]())
	return nil
}

// BatchUpsert inserts or conflict-updates multiple objects. It uses the same
// conflict and update whitelist semantics as Upsert and executes GORM's
// chunked statements atomically in one transaction.
//
// Every object must be non-nil, and the declared conflict-key tuple must be
// unique across the batch under the target database's equality rules. Exact
// duplicate Go values are rejected before hooks or SQL with
// ErrDuplicateBatchConflict; database collation and type rules remain the
// final authority. Empty input is a no-op.
//
// BatchUpsert is forbidden for scoped Stores and db.Owned models. Before-create
// hooks run for every item before this method opens its transaction. Input
// objects are not persisted-row snapshots on conflict; re-read when the stored
// RID, version, or database-generated values are needed. A bus-enabled Store
// publishes one payload-free OpUpsert per successful non-empty BatchUpsert
// call, not per input object, because the event represents type-wide
// invalidation.
func (s *Store[T]) BatchUpsert(ctx context.Context, objs []*T, conflictColumns []string, updateColumns ...string) error {
	if err := s.rejectWrite("BatchUpsert"); err != nil {
		return err
	}
	if len(objs) == 0 {
		return nil
	}
	if len(s.scopes) > 0 || db.IsOwnedModel(new(T)) {
		return ErrUpsertScoped
	}

	gormCols, doUpdateCols, err := s.resolveUpsertColumns(conflictColumns, updateColumns)
	if err != nil {
		return err
	}
	for i, obj := range objs {
		if obj == nil {
			return fmt.Errorf("store: BatchUpsert item %d: obj is nil", i)
		}
	}
	if err := s.rejectDuplicateConflictTuples(ctx, objs, gormCols); err != nil {
		return err
	}

	for i, obj := range objs {
		if err := fillOwner(ctx, obj, s.requirePrincipal, s.adminRoles); err != nil {
			return fmt.Errorf("store: BatchUpsert item %d: %w", i, err)
		}
	}
	for i, obj := range objs {
		for _, h := range s.hooks.beforeCreate {
			if err := h(ctx, obj); err != nil {
				return fmt.Errorf("store: BatchUpsert item %d: %w", i, err)
			}
		}
	}
	// Hooks are allowed to normalize conflict fields, so re-check before SQL.
	if err := s.rejectDuplicateConflictTuples(ctx, objs, gormCols); err != nil {
		return err
	}

	write := func(txCtx context.Context) error {
		return s.effectiveDB(txCtx).Clauses(clause.OnConflict{
			Columns:   gormCols,
			DoUpdates: clause.AssignmentColumns(doUpdateCols),
		}).CreateInBatches(objs, createBatchSize).Error
	}
	if txctx.DB(ctx, s.h) != nil || s.txDB != nil {
		err = write(ctx)
	} else {
		err = s.h.RunInTx(ctx, write)
	}
	if err != nil {
		return mapError(err)
	}
	// OpUpsert carries no row identity, so one event per call has the same
	// information as one per input without amplifying type-wide invalidation.
	s.publishChanged(ctx, upsertEvent[T]())
	return nil
}

func (s *Store[T]) resolveUpsertColumns(conflictColumns, updateColumns []string) ([]clause.Column, []string, error) {
	if len(conflictColumns) == 0 {
		return nil, nil, ErrMissingConflictColumns
	}

	gormCols := make([]clause.Column, 0, len(conflictColumns))
	seenConflict := make(map[string]struct{}, len(conflictColumns))
	for _, name := range conflictColumns {
		col, ok := s.queryFieldMap[name]
		if !ok {
			return nil, nil, fmt.Errorf("store: unknown conflict column %q: not in query whitelist", name)
		}
		// Repeating an arbiter column produces an invalid/ambiguous conflict
		// target, so treat it as a caller error instead of emitting SQL.
		if _, duplicate := seenConflict[col]; duplicate {
			return nil, nil, fmt.Errorf("store: duplicate conflict column %q", name)
		}
		seenConflict[col] = struct{}{}
		gormCols = append(gormCols, clause.Column{Name: col})
	}

	if s.updateFieldMap == nil {
		return nil, nil, ErrUpdateFieldsNotConfigured
	}
	var doUpdateCols []string
	if len(updateColumns) > 0 {
		doUpdateCols = make([]string, 0, len(updateColumns))
		seenUpdate := make(map[string]struct{}, len(updateColumns))
		for _, name := range updateColumns {
			col, ok := s.updateFieldMap[name]
			if !ok {
				return nil, nil, fmt.Errorf("%w: %q", ErrUnknownUpdateField, name)
			}
			// Repeating an assignment column is semantically redundant: every
			// occurrence resolves to the same excluded column. Collapse it while
			// preserving the caller's first-occurrence order.
			if _, duplicate := seenUpdate[col]; duplicate {
				continue
			}
			seenUpdate[col] = struct{}{}
			doUpdateCols = append(doUpdateCols, col)
		}
	} else {
		doUpdateCols = make([]string, 0, len(s.updateFieldMap))
		for _, col := range s.updateFieldMap {
			doUpdateCols = append(doUpdateCols, col)
		}
		sort.Strings(doUpdateCols)
	}
	if len(doUpdateCols) == 0 {
		return nil, nil, ErrMissingColumns
	}
	return gormCols, doUpdateCols, nil
}

type batchConflictTuple struct {
	index  int
	values []any
}

// rejectDuplicateConflictTuples catches exact duplicate Go values before
// GORM splits a batch into statements. Database equality remains authoritative
// for collation/type-specific equivalence, so the public contract still
// requires conflict tuples to be unique under the target database's rules.
func (s *Store[T]) rejectDuplicateConflictTuples(ctx context.Context, objs []*T, cols []clause.Column) error {
	stmt := &gorm.Statement{DB: s.effectiveDB(ctx)}
	if err := stmt.Parse(new(T)); err != nil {
		return fmt.Errorf("store: BatchUpsert parse model: %w", err)
	}

	seen := make(map[string][]batchConflictTuple, len(objs))
	for i, obj := range objs {
		rv := reflect.ValueOf(obj)
		values := make([]any, len(cols))
		var fingerprint strings.Builder
		for j, col := range cols {
			field := stmt.Schema.LookUpField(col.Name)
			if field == nil {
				return fmt.Errorf("store: BatchUpsert conflict column %q not found in model", col.Name)
			}
			value, _ := field.ValueOf(ctx, rv)
			value, err := normalizeConflictValue(value)
			if err != nil {
				return fmt.Errorf("store: BatchUpsert conflict column %q value: %w", col.Name, err)
			}
			values[j] = value
			fmt.Fprintf(&fingerprint, "%T:%#v\x00", value, value)
		}

		key := fingerprint.String()
		for _, prior := range seen[key] {
			if reflect.DeepEqual(prior.values, values) {
				return fmt.Errorf("%w: items %d and %d share the declared conflict key", ErrDuplicateBatchConflict, prior.index, i)
			}
		}
		seen[key] = append(seen[key], batchConflictTuple{index: i, values: values})
	}
	return nil
}

const maxConflictValueNormalizeDepth = 32

func normalizeConflictValue(value any) (any, error) {
	for depth := 0; value != nil; depth++ {
		if depth >= maxConflictValueNormalizeDepth {
			return nil, fmt.Errorf("exceeded maximum normalization depth %d", maxConflictValueNormalizeDepth)
		}
		if valuer, ok := value.(driver.Valuer); ok {
			resolved, err := valuer.Value()
			if err != nil {
				return nil, err
			}
			value = resolved
			continue
		}
		rv := reflect.ValueOf(value)
		if rv.Kind() != reflect.Pointer && rv.Kind() != reflect.Interface {
			break
		}
		if rv.IsNil() {
			return nil, nil
		}
		value = rv.Elem().Interface()
	}
	return value, nil
}

// --- helpers ---------------------------------------------------------------

func (s *Store[T]) finalizeUpdate(ctx context.Context, by Locator, result *gorm.DB, lockVer int, changes ChangeSnapshot) error {
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
	s.publishChanged(ctx, EntityChanged[T]{Op: OpUpdate, Locator: snapshotLocator(by), Changes: changes})
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
