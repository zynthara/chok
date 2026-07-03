package handler

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// decodeForm maps string values (query params, path values) onto
// tag-addressed struct fields — the stdlib replacement for gin's
// binding.MapFormWithTag (SPEC §4.2 item 2).
//
// Supported field kinds: string, bool, all int/uint widths, floats,
// time.Duration, time.Time (RFC3339), pointers to those, slices of
// those, and embedded structs (recursed with the same value map).
// Scalars take the first value; slices take all. Untagged fields and
// `tag:"-"` are skipped; tag options after a comma are ignored. Maps
// are not supported (declare a `json` body field instead).
func decodeForm(values map[string][]string, target any, tag string) error {
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("handler: form decode needs a non-nil pointer, got %T", target)
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return nil // non-struct targets have no tagged fields to fill
	}
	return decodeFormStruct(values, rv, tag)
}

func decodeFormStruct(values map[string][]string, rv reflect.Value, tag string) error {
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := rv.Field(i)

		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				if fv.IsNil() {
					if ft.Elem().Kind() != reflect.Struct {
						continue
					}
					fv.Set(reflect.New(ft.Elem()))
				}
				fv = fv.Elem()
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				if err := decodeFormStruct(values, fv, tag); err != nil {
					return err
				}
			}
			continue
		}

		name := f.Tag.Get(tag)
		if comma := strings.IndexByte(name, ','); comma >= 0 {
			name = name[:comma]
		}
		if name == "" || name == "-" {
			continue
		}
		vs, ok := values[name]
		if !ok || len(vs) == 0 {
			continue
		}
		if err := setFormField(fv, f, vs); err != nil {
			return fmt.Errorf("field %q: %w", name, err)
		}
	}
	return nil
}

func setFormField(fv reflect.Value, f reflect.StructField, vs []string) error {
	ft := fv.Type()

	if ft.Kind() == reflect.Pointer {
		elem := reflect.New(ft.Elem())
		tmp := f
		tmp.Type = ft.Elem()
		if err := setFormField(elem.Elem(), tmp, vs); err != nil {
			return err
		}
		fv.Set(elem)
		return nil
	}

	if ft.Kind() == reflect.Slice && ft.Elem().Kind() != reflect.Uint8 {
		out := reflect.MakeSlice(ft, len(vs), len(vs))
		tmp := f
		tmp.Type = ft.Elem()
		for i, s := range vs {
			if err := setFormField(out.Index(i), tmp, []string{s}); err != nil {
				return err
			}
		}
		fv.Set(out)
		return nil
	}

	return setFormScalar(fv, vs[0])
}

func setFormScalar(fv reflect.Value, s string) error {
	switch fv.Type() {
	case reflect.TypeOf(time.Duration(0)):
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q", s)
		}
		fv.SetInt(int64(d))
		return nil
	case reflect.TypeOf(time.Time{}):
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return fmt.Errorf("invalid RFC3339 time %q", s)
		}
		fv.Set(reflect.ValueOf(t))
		return nil
	}

	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("invalid bool %q", s)
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid integer %q", s)
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid unsigned integer %q", s)
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(s, fv.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid number %q", s)
		}
		fv.SetFloat(n)
	default:
		return fmt.Errorf("unsupported field kind %s", fv.Kind())
	}
	return nil
}
