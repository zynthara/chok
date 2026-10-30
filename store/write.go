package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/internal/dbx"
	"github.com/zynthara/chok/store/internal/q"
	"github.com/zynthara/chok/store/where"
)

const batchSize = 100 // rows per INSERT of BatchCreate and BatchUpsert

// Create inserts obj and fills in its RID, version and timestamps. On an
// owned model the principal becomes the owner. A unique violation is a
// DuplicateError naming the field.
func (s *Store[T]) Create(ctx context.Context, obj *T) error {
	if obj == nil {
		return errors.New("store: Create: nil object")
	}
	if err := s.beforeCreate(ctx, obj); err != nil {
		return err
	}
	return s.mapError(s.d.Unsafe(ctx).Create(obj).Error)
}

// BatchCreate inserts objs in one transaction, all or none.
func (s *Store[T]) BatchCreate(ctx context.Context, objs []*T) error {
	if len(objs) == 0 {
		return nil
	}
	for i, obj := range objs {
		if obj == nil {
			return fmt.Errorf("store: BatchCreate: item %d is nil", i)
		}
		if err := s.beforeCreate(ctx, obj); err != nil {
			return fmt.Errorf("store: BatchCreate: item %d: %w", i, err)
		}
	}
	return s.d.RunInTx(ctx, func(txCtx context.Context) error {
		return s.mapError(s.d.Unsafe(txCtx).CreateInBatches(objs, batchSize).Error)
	})
}

func (s *Store[T]) beforeCreate(ctx context.Context, obj *T) error {
	if err := s.fillOwner(ctx, obj); err != nil {
		return err
	}
	if h := s.hooks.BeforeCreate; h != nil {
		return h(ctx, obj)
	}
	return nil
}

// write is the scoped query of an update or delete: the owner scope and
// the custom scopes, opts, and loc. Without a principal an owner-scoped
// write fails with NoPrincipalError.
func (s *Store[T]) write(ctx context.Context, loc Locator, opts []where.Option) (*gorm.DB, *where.Config, error) {
	if loc == nil {
		return nil, nil, errors.New("store: nil Locator")
	}
	tx, err := s.scoped(ctx, s.d.Unsafe(ctx).Model(new(T)))
	if err != nil {
		return nil, nil, err
	}
	cfg := &where.Config{FiltersOnly: true}
	if tx, err = s.apply(ctx, tx, cfg, opts); err != nil {
		return nil, nil, err
	}
	if tx, err = loc.apply(s, tx); err != nil {
		return nil, nil, s.codeError(err)
	}
	return tx, cfg, nil
}

// Update writes ch to the row loc names and advances its version. The
// lock is where.Version, else the version Patch(...).Onto carried: when
// the row has moved past it, Update fails with VersionConflictError, and
// on success the object given to Onto holds the new values, version and
// updated_at, ready to return. A row the caller may not see is a
// NotFoundError.
func (s *Store[T]) Update(ctx context.Context, loc Locator, ch *Changes, opts ...where.Option) error {
	b, err := ch.build(ctx, s.updateFields, s.columns, s.schema)
	if err != nil {
		return err
	}
	if h := s.hooks.BeforeUpdate; h != nil {
		if err := h(ctx, loc, maps.Clone(b.fields)); err != nil {
			return err
		}
	}
	tx, cfg, err := s.write(ctx, loc, opts)
	if err != nil {
		return err
	}
	lock := b.lock
	if cfg.Version > 0 {
		lock = cfg.Version
	}
	if lock > 0 {
		tx = tx.Where(s.table()+".version = ?", lock)
	}
	now := tx.NowFunc()
	b.payload["version"] = gorm.Expr("version + 1")
	b.payload["updated_at"] = now
	res := tx.Select(append(b.columns, "version", "updated_at")).Updates(b.payload)
	if res.Error != nil {
		return s.mapError(res.Error)
	}
	if res.RowsAffected == 0 {
		return s.missed(ctx, loc, lock)
	}
	if b.model != nil {
		b.model.UpdatedAt = now
		if lock > 0 {
			b.model.Version = lock + 1
		}
	}
	return nil
}

// missed explains a locked write that matched no row: a row that exists
// at another version is a conflict, anything else is not found.
func (s *Store[T]) missed(ctx context.Context, loc Locator, lock int) error {
	if lock > 0 {
		tx, err := s.scoped(ctx, s.d.Unsafe(ctx).Model(new(T)).Unscoped())
		if err != nil {
			return err
		}
		found, err := s.exists(tx, loc)
		if err != nil {
			return err
		}
		if found {
			return &VersionConflictError{Locator: loc.String(), Version: lock}
		}
	}
	return &NotFoundError{Locator: loc.String()}
}

// Delete removes the row loc names. A soft-delete model keeps the row
// with deleted_at set and a random delete_token, which frees its
// SoftUnique values; other models lose it. Deleting what is not there is
// not an error, except under where.Version: then a moved row is a
// VersionConflictError and a missing one a NotFoundError.
func (s *Store[T]) Delete(ctx context.Context, loc Locator, opts ...where.Option) error {
	if h := s.hooks.BeforeDelete; h != nil {
		if err := h(ctx, loc); err != nil {
			return err
		}
	}
	tx, cfg, err := s.write(ctx, loc, opts)
	if err != nil {
		return err
	}
	if cfg.Version > 0 {
		tx = tx.Where(s.table()+".version = ?", cfg.Version)
	}
	var res *gorm.DB
	if s.soft {
		res = tx.Updates(map[string]any{
			"deleted_at":   time.Now().UTC(),
			"delete_token": deleteToken(),
			"version":      gorm.Expr("version + 1"),
		})
	} else {
		res = tx.Delete(new(T))
	}
	if res.Error != nil {
		return s.mapError(res.Error)
	}
	if res.RowsAffected == 0 && cfg.Version > 0 {
		return s.missed(ctx, loc, cfg.Version)
	}
	return nil
}

// deleteToken is a soft-deleted row's random delete_token.
func deleteToken() string {
	var b [9]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// Restore brings back the soft-deleted row loc names. If a live row took
// one of its SoftUnique values meanwhile, Restore is a DuplicateError and
// the row stays deleted. Restoring a live row does nothing; a row the
// caller may not see is a NotFoundError.
func (s *Store[T]) Restore(ctx context.Context, loc Locator) error {
	if !s.soft {
		return fmt.Errorf("store: Restore: %s has no soft delete (embed db.SoftDeleteModel)", s.schema.Name)
	}
	tx, _, err := s.write(ctx, loc, []where.Option{where.OnlyTrashed()})
	if err != nil {
		return err
	}
	res := tx.Updates(map[string]any{
		"deleted_at":   nil,
		"delete_token": "",
		"version":      gorm.Expr("version + 1"),
	})
	if res.Error != nil {
		return s.mapError(res.Error)
	}
	if res.RowsAffected > 0 {
		return nil
	}
	live, err := s.scoped(ctx, s.d.Unsafe(ctx).Model(new(T)))
	if err != nil {
		return err
	}
	found, err := s.exists(live, loc)
	if err != nil {
		return err
	}
	if !found {
		return &NotFoundError{Locator: loc.String()}
	}
	return nil
}

// Upsert inserts obj or, when a row with the same conflict fields exists,
// overwrites that row's update fields and advances its version. conflict
// names the fields of a unique index, the RID when empty; on a soft-delete
// model a SoftUnique index counts only live rows. MySQL updates on any
// unique key whatever conflict says. obj is not reloaded: after an update
// it does not hold the stored row's RID or version. An owner-scoped or
// custom-scoped store refuses Upsert, whose update cannot be fenced: use
// Create, and Update on a DuplicateError.
func (s *Store[T]) Upsert(ctx context.Context, obj *T, conflict ...string) error {
	if obj == nil {
		return errors.New("store: Upsert: nil object")
	}
	return s.BatchUpsert(ctx, []*T{obj}, conflict...)
}

// BatchUpsert is Upsert of objs in one transaction. Two objects with the
// same conflict values are an error, as the database would refuse them.
func (s *Store[T]) BatchUpsert(ctx context.Context, objs []*T, conflict ...string) error {
	if len(s.scopes) > 0 {
		return errors.New("store: Upsert cannot be fenced by an owner or custom scope; use Create and Update")
	}
	if len(objs) == 0 {
		return nil
	}
	target, err := s.conflictTarget(conflict)
	if err != nil {
		return err
	}
	set := make([]string, 0, len(s.updateFields)+1)
	for _, col := range s.updateFields {
		set = append(set, col)
	}
	slices.Sort(set)
	set = append(set, "updated_at")
	assignments := clause.AssignmentColumns(set)
	// Postgres needs the table: version alone could be excluded.version.
	assignments = append(assignments, clause.Assignment{Column: clause.Column{Name: "version"}, Value: gorm.Expr(s.table() + ".version + 1")})
	for i, obj := range objs {
		if obj == nil {
			return fmt.Errorf("store: BatchUpsert: item %d is nil", i)
		}
		if err := s.beforeCreate(ctx, obj); err != nil {
			return fmt.Errorf("store: BatchUpsert: item %d: %w", i, err)
		}
	}
	if err := s.distinctConflicts(ctx, objs, target); err != nil {
		return err
	}
	cols := make([]clause.Column, len(target))
	for i, c := range target {
		cols[i] = clause.Column{Name: c}
	}
	onConflict := clause.OnConflict{Columns: cols, DoUpdates: assignments}
	return s.d.RunInTx(ctx, func(txCtx context.Context) error {
		return s.mapError(s.d.Unsafe(txCtx).Clauses(onConflict).CreateInBatches(objs, batchSize).Error)
	})
}

// conflictTarget resolves the conflict fields to columns, adding
// delete_token when they are a SoftUnique index's.
func (s *Store[T]) conflictTarget(fields []string) ([]string, error) {
	if len(fields) == 0 {
		fields = []string{"id"}
	}
	cols := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		col, err := q.Column(s.columns, f)
		if err != nil {
			return nil, fmt.Errorf("store: Upsert: %s: conflict %w", s.schema.Name, err)
		}
		if slices.Contains(cols, col) {
			return nil, fmt.Errorf("store: Upsert: %s: conflict field %q repeats column %q", s.schema.Name, f, col)
		}
		cols = append(cols, col)
	}
	if s.soft {
		for _, idx := range dbx.Indexes(s.schema.ModelType) {
			if len(idx.Columns) == len(cols) && sameSet(idx.Columns, cols) {
				return append(slices.Clone(idx.Columns), "delete_token"), nil
			}
		}
	}
	return cols, nil
}

func sameSet(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

// distinctConflicts refuses a batch in which two objects share their
// conflict values. A RID still empty is assigned at insert and never
// collides.
func (s *Store[T]) distinctConflicts(ctx context.Context, objs []*T, target []string) error {
	fields := make([]reflectField, 0, len(target))
	for _, col := range target {
		if f := s.schema.LookUpField(col); f != nil {
			fields = append(fields, reflectField{col: col, f: f})
		}
	}
	seen := make(map[string]int, len(objs))
	for i, obj := range objs {
		key, skip := "", false
		rv := reflect.ValueOf(obj)
		for _, rf := range fields {
			v, zero := rf.f.ValueOf(ctx, rv)
			if rf.col == "rid" && zero {
				skip = true
				break
			}
			if v, err := normalizeValue(v); err == nil {
				key += fmt.Sprintf("%T:%v\x00", v, v)
			}
		}
		if skip {
			continue
		}
		if j, dup := seen[key]; dup {
			return fmt.Errorf("store: BatchUpsert: items %d and %d have the same conflict values", j, i)
		}
		seen[key] = i
	}
	return nil
}

type reflectField struct {
	col string
	f   *schema.Field
}
