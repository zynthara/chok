package conf

import (
	"encoding"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// decoder assigns a raw tree to a typed value and collects every problem
// instead of stopping at the first.
type decoder struct {
	lines    map[string]int // key path → line in the file, for messages
	problems []string
}

func (d *decoder) problemf(path, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if path != "" {
		msg = path + ": " + msg
	}
	d.problems = append(d.problems, msg+d.at(path))
}

// at names the file line of path, or of its nearest parent.
func (d *decoder) at(path string) string {
	for p := path; p != ""; {
		if line, ok := d.lines[p]; ok {
			return fmt.Sprintf(" (line %d)", line)
		}
		i := strings.LastIndexAny(p, ".[")
		if i < 0 {
			break
		}
		p = p[:i]
	}
	return ""
}

func (d *decoder) decode(path string, raw any, v reflect.Value) {
	t := v.Type()
	if t.Kind() == reflect.Pointer {
		if raw == nil {
			return
		}
		if v.IsNil() {
			v.Set(reflect.New(t.Elem()))
		}
		d.decode(path, raw, v.Elem())
		return
	}
	if raw == nil {
		return
	}
	if t != durationType && reflect.PointerTo(t).Implements(textUnmarshalerType) {
		s, ok := raw.(string)
		if !ok {
			d.problemf(path, "expected a single value, got %s", describe(raw))
			return
		}
		if err := v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s)); err != nil {
			d.problemf(path, "%v", err)
		}
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		d.decodeStruct(path, raw, v)
	case reflect.Map:
		d.decodeMap(path, raw, v)
	case reflect.Slice:
		d.decodeSlice(path, raw, v)
	case reflect.Interface:
		if t.NumMethod() != 0 {
			d.problemf(path, "cannot decode into %s", t)
			return
		}
		v.Set(reflect.ValueOf(raw))
	default:
		d.decodeScalar(path, raw, v)
	}
}

func (d *decoder) decodeStruct(path string, raw any, v reflect.Value) {
	m, ok := raw.(map[string]any)
	if !ok {
		d.problemf(path, "expected a mapping of keys, got %s", describe(raw))
		return
	}
	fields := fieldsOf(v.Type())
	known := make([]string, 0, len(fields))
	for _, f := range fields {
		known = append(known, f.key)
		p := join(path, f.key)
		fv := v.FieldByIndex(f.index)
		if r := m[f.key]; r != nil {
			d.decode(p, r, fv)
			continue
		}
		switch {
		case f.hasDef:
			d.decode(p, f.def, fv)
		case f.typ.Kind() == reflect.Struct && isSection(f.typ):
			// An absent section still gets the defaults of its keys.
			d.decode(p, map[string]any{}, fv)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if slices.Contains(known, k) {
			continue
		}
		if path == "" {
			d.problems = append(d.problems, fmt.Sprintf("unknown section %s%s%s", k, suggest(k, known), d.at(k)))
		} else {
			d.problems = append(d.problems, fmt.Sprintf("unknown key %s%s%s", join(path, k), suggest(k, known), d.at(join(path, k))))
		}
	}
}

func (d *decoder) decodeMap(path string, raw any, v reflect.Value) {
	t := v.Type()
	if t.Key().Kind() != reflect.String {
		d.problemf(path, "maps need string keys, %s has %s", t, t.Key())
		return
	}
	m, ok := raw.(map[string]any)
	if !ok {
		d.problemf(path, "expected a mapping, got %s", describe(raw))
		return
	}
	out := reflect.MakeMapWithSize(t, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		r := m[k]
		if r == nil && isSection(t.Elem()) {
			r = map[string]any{}
		}
		ev := reflect.New(t.Elem()).Elem()
		d.decode(join(path, k), r, ev)
		out.SetMapIndex(reflect.ValueOf(k).Convert(t.Key()), ev)
	}
	v.Set(out)
}

func (d *decoder) decodeSlice(path string, raw any, v reflect.Value) {
	var items []any
	switch r := raw.(type) {
	case []any:
		items = r
	case string:
		// A scalar for a list of scalars is a comma-separated list: that
		// is how defaults, environment variables and flags spell lists.
		if !isScalar(deref(v.Type().Elem())) {
			d.problemf(path, "expected a list, got %s", describe(raw))
			return
		}
		for s := range strings.SplitSeq(r, ",") {
			if s = strings.TrimSpace(s); s != "" {
				items = append(items, s)
			}
		}
	default:
		d.problemf(path, "expected a list, got %s", describe(raw))
		return
	}
	out := reflect.MakeSlice(v.Type(), len(items), len(items))
	for i, it := range items {
		if it == nil && isSection(v.Type().Elem()) {
			it = map[string]any{}
		}
		d.decode(fmt.Sprintf("%s[%d]", path, i), it, out.Index(i))
	}
	v.Set(out)
}

func (d *decoder) decodeScalar(path string, raw any, v reflect.Value) {
	s, ok := raw.(string)
	if !ok {
		d.problemf(path, "expected a single value, got %s", describe(raw))
		return
	}
	t := v.Type()
	if t == durationType {
		dur, err := time.ParseDuration(s)
		if err != nil {
			d.problemf(path, "%q is not a duration (write it like 30s, 5m or 1h30m)", s)
			return
		}
		v.SetInt(int64(dur))
		return
	}
	switch t.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			d.problemf(path, "%q is not true or false", s)
			return
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 0, t.Bits())
		if err != nil {
			d.problemf(path, "%q is not an integer that fits %s", s, t)
			return
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 0, t.Bits())
		if err != nil {
			d.problemf(path, "%q is not a non-negative integer that fits %s", s, t)
			return
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, t.Bits())
		if err != nil {
			d.problemf(path, "%q is not a number", s)
			return
		}
		v.SetFloat(f)
	default:
		d.problemf(path, "cannot decode into %s", t)
	}
}

func describe(raw any) string {
	switch raw.(type) {
	case map[string]any:
		return "a mapping"
	case []any:
		return "a list"
	case string:
		return "a single value"
	}
	return fmt.Sprintf("%T", raw)
}
