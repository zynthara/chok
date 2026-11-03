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
	"strings"
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
		return nil, nil, s.codeError(ctx, err)
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

	tx, cfg, err := s.write(ctx, loc, opts)
	if err != nil {
		return err
	}
	if h := s.hooks.BeforeUpdate; h != nil {
		if err := h(ctx, loc, maps.Clone(b.fields)); err != nil {
			return err
		}
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
		found, err := s.exists(ctx, tx, loc)
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

	tx, cfg, err := s.write(ctx, loc, opts)
	if err != nil {
		return err
	}
	if h := s.hooks.BeforeDelete; h != nil {
		if err := h(ctx, loc); err != nil {
			return err
		}
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
	found, err := s.exists(ctx, live, loc)
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
// names the fields of a unique index, the RID when empty. On a soft-delete
// model a SoftUnique index counts only live rows; any other index, a
// Unique or the RID's, counts deleted rows too, and a deleted row holding
// obj's values is the DuplicateError Create gets, with nothing written.
// The check runs before the write: on PostgreSQL and MySQL a row deleted
// in between is still the one updated, and stays deleted; SQLite's single
// writer leaves no such gap. MySQL updates on any unique key whatever
// conflict says. obj is not reloaded: after an update it does not hold
// the stored row's RID or version. An owner-scoped or custom-scoped store
// refuses Upsert, whose update cannot be fenced: use Create, and Update
// on a DuplicateError.
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
	target, live, err := s.conflictTarget(ctx, conflict)
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
		if s.soft && !live {
			if err := s.deletedConflict(txCtx, objs, target); err != nil {
				return err
			}
		}
		return s.mapError(s.d.Unsafe(txCtx).Clauses(onConflict).CreateInBatches(objs, batchSize).Error)
	})
}

// conflictTarget resolves the conflict fields to the columns of ON
// CONFLICT. live reports a SoftUnique index's columns, to which it adds
// delete_token so that only live rows conflict. Any other target, a
// Unique index, the RID or a gorm unique tag, is the columns as they are:
// on a soft-delete model a deleted row holding the values conflicts too,
// which BatchUpsert checks before it writes.
func (s *Store[T]) conflictTarget(ctx context.Context, fields []string) ([]string, bool, error) {
	if len(fields) == 0 {
		fields = []string{"id"}
	}
	cols := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		col, err := q.Column(s.columns, f)
		if err != nil {
			logUnknownField(ctx, s.schema.Name, err)
			return nil, false, fmt.Errorf("store: Upsert: %s: conflict %w", s.schema.Name, err)
		}
		if slices.Contains(cols, col) {
			return nil, false, fmt.Errorf("store: Upsert: %s: conflict field %q repeats column %q", s.schema.Name, f, col)
		}
		cols = append(cols, col)
	}
	if s.soft {
		for _, idx := range dbx.Indexes(s.d, s.schema.ModelType) {
			if idx.Soft && len(idx.Columns) == len(cols) && sameSet(idx.Columns, cols) {
				return append(slices.Clone(idx.Columns), "delete_token"), true, nil
			}
		}
	}
	return cols, false, nil
}

// deletedConflict returns the DuplicateError of a batch in which a
// deleted row holds an object's values in the target columns, which ON
// CONFLICT would update and leave deleted; nil when no deleted row does.
// It asks once per batchSize objects, as CreateInBatches inserts them:
// the deleted rows equal to any object's values, one parenthesized group
// of equalities per object. An object with a value assigned at insert
// (an empty RID, a NULL, a zero value the database's default replaces)
// matches no stored row and is left out. A target column that is no
// field of the model, an Alias gorm does not map, cannot be read off the
// objects; such a target is not checked.
func (s *Store[T]) deletedConflict(ctx context.Context, objs []*T, target []string) error {
	fields := make([]*schema.Field, len(target))
	for i, col := range target {
		if fields[i] = s.schema.LookUpField(col); fields[i] == nil {
			return nil
		}
	}
	tx := s.d.Unsafe(ctx)
	eqs := make([]string, len(fields))
	for i, f := range fields {
		eqs[i] = tx.Statement.Quote(clause.Column{Table: s.table(), Name: f.DBName}) + " = ?"
	}
	group := "(" + strings.Join(eqs, " AND ") + ")"
	for chunk := range slices.Chunk(objs, batchSize) {
		groups := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*len(fields))
		for _, obj := range chunk {
			values, ok := insertValues(ctx, fields, reflect.ValueOf(obj))
			if !ok {
				continue
			}
			groups = append(groups, group)
			args = append(args, values...)
		}
		if len(groups) == 0 {
			continue
		}
		var one int
		res := tx.Model(new(T)).Unscoped().
			Where(s.table()+".deleted_at IS NOT NULL AND ("+strings.Join(groups, " OR ")+")", args...).
			Select("1").Limit(1).Scan(&one)
		if res.Error != nil {
			return s.mapError(res.Error)
		}
		if res.RowsAffected > 0 {
			return s.deletedDuplicate(target)
		}
	}
	return nil
}

// insertValues reads off obj the values an insert writes to fields, as
// gorm writes them: a zero value whose default gorm knows is that
// default. ok is false when one is assigned at insert, so that it matches
// no stored row: an empty RID, a NULL, a zero value the database's own
// default replaces.
func insertValues(ctx context.Context, fields []*schema.Field, obj reflect.Value) ([]any, bool) {
	values := make([]any, len(fields))
	for i, f := range fields {
		v, zero := f.ValueOf(ctx, obj)
		if zero {
			switch {
			case f.DBName == "rid", f.HasDefaultValue && f.DefaultValueInterface == nil:
				return nil, false
			case f.DefaultValueInterface != nil:
				v = f.DefaultValueInterface
			}
		}
		if n, err := normalizeValue(v); err != nil || n == nil {
			return nil, false
		}
		values[i] = v
	}
	return values, true
}

// deletedDuplicate is the DuplicateError of target values a deleted row
// holds, named as PostgreSQL and MySQL name Create's: by the unique index
// over the target, else by its columns.
func (s *Store[T]) deletedDuplicate(target []string) *DuplicateError {
	dup := &DuplicateError{Constraint: s.uniqueIndexOf(target)}
	if dup.Constraint != "" {
		dup.Field = s.constraintField(dup.Constraint)
	}
	if dup.Field == "" {
		dup.Field = s.columnsField(target)
	}
	return dup
}

// uniqueIndexOf names the unique index over exactly cols that counts
// deleted rows: a Unique, a gorm uniqueIndex such as the RID's, or the
// constraint of a gorm unique tag. It is empty when the model declares
// none.
func (s *Store[T]) uniqueIndexOf(cols []string) string {
	for _, idx := range dbx.Indexes(s.d, s.schema.ModelType) {
		if !idx.Soft && len(idx.Columns) == len(cols) && sameSet(idx.Columns, cols) {
			return idx.Name
		}
	}
	for _, idx := range s.schema.ParseIndexes() {
		if idx.Class != "UNIQUE" || len(idx.Fields) != len(cols) {
			continue
		}
		idxCols := make([]string, 0, len(idx.Fields))
		for _, f := range idx.Fields {
			if f.Field != nil {
				idxCols = append(idxCols, f.DBName)
			}
		}
		if len(idxCols) == len(cols) && sameSet(idxCols, cols) {
			return idx.Name
		}
	}
	if len(cols) == 1 {
		if f := s.schema.LookUpField(cols[0]); f != nil && f.Unique {
			// gorm's name for the constraint of a unique tag, as in New.
			return "uni_" + s.schema.Table + "_" + f.DBName
		}
	}
	return ""
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
