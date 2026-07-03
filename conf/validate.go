package conf

import (
	"errors"
	"fmt"
	"reflect"
)

// validateTree validates a decoded section: the recursive Validatable
// walk over nested fields first, then the section root itself (root
// last, so cross-field checks see validated parts). All errors are
// collected so operators fix everything in one pass — same contract
// as v1 validateConfig/validateFields.
func validateTree(root reflect.Value, path string) error {
	var errs []error

	// A SelfValidating section root validates its own subtree — the
	// recursive walk must not descend (v1 only met discriminators as
	// fields; in v2 a discriminator can BE the section type).
	selfValidating := false
	if root.CanAddr() {
		if _, ok := root.Addr().Interface().(SelfValidating); ok {
			selfValidating = true
		}
	}

	if !selfValidating {
		if err := validateFields(root, path); err != nil {
			errs = append(errs, err)
		}
	}
	if root.CanAddr() {
		if v, ok := root.Addr().Interface().(Validatable); ok {
			if err := v.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("conf: validate section %q: %w", path, err))
			}
		}
	} else if v, ok := root.Interface().(Validatable); ok {
		if err := v.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("conf: validate section %q: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

// validateFields walks struct fields recursively, calling Validate()
// on each Validatable. SelfValidating fields validate themselves and
// stop the descent — a discriminator validates only the branch its
// selector picked, and recursing would trip unselected branches.
func validateFields(rv reflect.Value, prefix string) error {
	if rv.Kind() != reflect.Struct {
		return nil
	}
	var errs []error
	t := rv.Type()
	for i := range t.NumField() {
		fv := rv.Field(i)
		ft := t.Field(i)
		if !ft.IsExported() {
			continue
		}
		path := prefix + "." + mapKeyOf(ft)

		if fv.Kind() == reflect.Ptr {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
		}

		var skipRecurse bool
		if fv.CanAddr() {
			addr := fv.Addr().Interface()
			if v, ok := addr.(Validatable); ok {
				if err := v.Validate(); err != nil {
					errs = append(errs, fmt.Errorf("conf: validate field %s: %w", path, err))
				}
			}
			if _, ok := addr.(SelfValidating); ok {
				skipRecurse = true
			}
		}

		if !skipRecurse && fv.Kind() == reflect.Struct && !isAtomicStruct(fv.Type()) {
			if err := validateFields(fv, path); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
