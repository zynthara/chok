package db

import (
	"fmt"
	"reflect"

	"github.com/zynthara/chok/v2/rid"
)

// ValidateModel checks full-model metadata:
//   - Must embed db.Model or db.SoftDeleteModel
//   - If RIDPrefixer, prefix must be valid
//
// Called by db.Table and store.New at construction time. Append-only
// models (db.AppendOnlyModel) validate via ValidateAppendModel instead;
// db.Table dispatches between the two by marker interface.
func ValidateModel(model any) error {
	t := reflect.TypeOf(model)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return fmt.Errorf("db: model must be a struct, got %s", t.Kind())
	}

	// Check that it embeds Model (satisfies Modeler).
	ptr := reflect.New(t).Interface()
	if _, ok := ptr.(Modeler); !ok {
		return fmt.Errorf("db: %s must embed db.Model or db.SoftDeleteModel", t.Name())
	}

	// If it implements RIDPrefixer, validate the prefix.
	if p, ok := ptr.(RIDPrefixer); ok {
		prefix := p.RIDPrefix()
		if err := rid.ValidatePrefix(prefix, 12); err != nil {
			return fmt.Errorf("db: model %s: %w", t.Name(), err)
		}
	}

	return nil
}

// ValidateAppendModel checks append-only model metadata:
//   - Must embed db.AppendOnlyModel (satisfy AppendModeler)
//   - Must NOT also embed db.Model — carrying both markers makes the
//     model's identity ambiguous (which store family owns it?)
//   - Must NOT implement RIDPrefixer — append-only models have no RID
//     column for the prefix to apply to
//
// Called by db.Table and store.NewAppend at construction time.
func ValidateAppendModel(model any) error {
	t := reflect.TypeOf(model)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return fmt.Errorf("db: model must be a struct, got %s", t.Kind())
	}

	ptr := reflect.New(t).Interface()
	if _, ok := ptr.(AppendModeler); !ok {
		return fmt.Errorf("db: %s must embed db.AppendOnlyModel", t.Name())
	}
	if _, ok := ptr.(Modeler); ok {
		return fmt.Errorf("db: %s embeds both db.Model and db.AppendOnlyModel — pick one base (full model or append-only)", t.Name())
	}
	if _, ok := ptr.(RIDPrefixer); ok {
		return fmt.Errorf("db: %s implements RIDPrefixer but append-only models have no RID column — remove the RIDPrefix method or embed db.Model", t.Name())
	}
	return nil
}

// IsSoftDeleteModel returns true if the model embeds SoftDeleteModel,
// including through intermediate anonymous structs.
func IsSoftDeleteModel(model any) bool {
	t := reflect.TypeOf(model)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return hasSoftDeleteModel(t)
}

// IsOwnedModel returns true if the model implements OwnerAccessor
// (i.e. embeds the Owned mixin).
func IsOwnedModel(model any) bool {
	if _, ok := model.(OwnerAccessor); ok {
		return true
	}
	// model may be a non-pointer value; check the pointer form.
	v := reflect.ValueOf(model)
	if v.Kind() != reflect.Ptr {
		pv := reflect.New(v.Type())
		pv.Elem().Set(v)
		_, ok := pv.Interface().(OwnerAccessor)
		return ok
	}
	return false
}

func hasSoftDeleteModel(t reflect.Type) bool {
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Type == reflect.TypeOf(SoftDeleteModel{}) {
			return true
		}
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && hasSoftDeleteModel(ft) {
				return true
			}
		}
	}
	return false
}
