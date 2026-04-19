package store

import (
	"fmt"
	"reflect"

	"github.com/zynthara/chok/db"
)

// Changes describes what to update on matched rows. It is the "what" axis of
// the CRUD matrix, orthogonal to Locator ("who") and UpdateOption ("how").
//
// Two constructors cover all common cases:
//
//   - Set(map) — explicit map of public field names to values. No implicit
//     optimistic lock; use WithVersion to enable.
//   - Fields(obj, fields...) — struct-backed update. Automatically extracts
//     the optimistic-lock version from obj.Version unless .NoLock() is called.
//     Omitting fields updates every field in the Store's update whitelist.
type Changes interface {
	// build resolves public field names to DB columns and returns:
	//   cols        — columns the SQL UPDATE will touch (excluding any
	//                 version-bump column the Store adds later)
	//   payload     — the second argument to GORM's Updates(); either a
	//                 map[string]any or a pointer to the caller's struct
	//   implicitVer — the optimistic-lock version to enforce (>0 enables
	//                 the lock; 0 leaves the decision to UpdateOption)
	build(updateFieldMap map[string]string) (cols []string, payload any, implicitVer int, err error)
}

// Set returns a Changes that applies literal map values to the matched row.
// Keys are public field names resolved via the Store's update whitelist;
// unknown keys return ErrUnknownUpdateField. Empty map returns ErrMissingColumns.
//
// Set does NOT enable optimistic locking on its own — pairing with WithVersion
// is explicit:
//
//	store.Update(ctx, store.RID(x), store.Set(cols), store.WithVersion(v))
func Set(kv map[string]any) Changes {
	return setChanges(kv)
}

// Fields returns a FieldChanges that updates selected fields of obj.
//
// When fields is empty, every column declared via WithUpdateFields is written
// (whole-whitelist update). Zero values in obj ARE persisted — the Store
// internally uses Select(cols...).Updates(obj) to bypass GORM's default
// "skip zero values" behaviour, which would otherwise silently drop clears
// (e.g. setting a bio back to "").
//
// Fields auto-detects optimistic locking: if obj embeds db.Model (directly
// or via db.SoftDeleteModel), the current obj.Version is used as the WHERE
// version guard and incremented on success. Call .NoLock() to opt out for
// admin overrides or concurrent-safe fields.
func Fields(obj any, fields ...string) *FieldChanges {
	return &FieldChanges{obj: obj, fields: fields}
}

// FieldChanges is the concrete return of Fields so that .NoLock() remains
// available. It still satisfies the Changes interface.
type FieldChanges struct {
	obj    any
	fields []string // empty → every whitelisted field
	noLock bool
}

// NoLock disables the automatic optimistic-lock behaviour of Fields.
// Use this for admin-level overrides ("force save, ignore version") or when
// the caller already decided a first-write-wins policy is acceptable.
func (f *FieldChanges) NoLock() *FieldChanges {
	f.noLock = true
	return f
}

type setChanges map[string]any

func (s setChanges) build(fm map[string]string) ([]string, any, int, error) {
	if len(s) == 0 {
		return nil, nil, 0, ErrMissingColumns
	}
	if fm == nil {
		return nil, nil, 0, ErrUpdateFieldsNotConfigured
	}
	payload := make(map[string]any, len(s))
	cols := make([]string, 0, len(s))
	for field, val := range s {
		col, ok := fm[field]
		if !ok {
			return nil, nil, 0, fmt.Errorf("%w: %q", ErrUnknownUpdateField, field)
		}
		payload[col] = val
		cols = append(cols, col)
	}
	return cols, payload, 0, nil
}

func (f *FieldChanges) build(fm map[string]string) ([]string, any, int, error) {
	if f.obj == nil {
		return nil, nil, 0, fmt.Errorf("%w: Fields obj is nil", ErrMissingColumns)
	}
	if fm == nil {
		return nil, nil, 0, ErrUpdateFieldsNotConfigured
	}

	// Determine the field set: explicit list or full whitelist.
	fields := f.fields
	if len(fields) == 0 {
		if len(fm) == 0 {
			return nil, nil, 0, ErrMissingColumns
		}
		fields = make([]string, 0, len(fm))
		for k := range fm {
			fields = append(fields, k)
		}
	}

	cols := make([]string, 0, len(fields))
	for _, name := range fields {
		col, ok := fm[name]
		if !ok {
			return nil, nil, 0, fmt.Errorf("%w: %q", ErrUnknownUpdateField, name)
		}
		cols = append(cols, col)
	}

	// Auto-extract the current version for optimistic locking when the
	// object carries a db.Model (directly or via SoftDeleteModel).
	var implicitVer int
	if !f.noLock {
		if m := extractModelSafe(f.obj); m != nil {
			implicitVer = m.Version
		}
	}

	return cols, f.obj, implicitVer, nil
}

// extractModelSafe is the non-panicking sibling of extractModel. Returns nil
// when obj is nil or doesn't embed db.Model.
func extractModelSafe(obj any) *db.Model {
	v := reflect.ValueOf(obj)
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	if fv := v.FieldByName("Model"); fv.IsValid() && fv.CanAddr() {
		if m, ok := fv.Addr().Interface().(*db.Model); ok {
			return m
		}
	}
	if fv := v.FieldByName("SoftDeleteModel"); fv.IsValid() {
		if mf := fv.FieldByName("Model"); mf.IsValid() && mf.CanAddr() {
			if m, ok := mf.Addr().Interface().(*db.Model); ok {
				return m
			}
		}
	}
	return nil
}
