package conf

import (
	"reflect"
	"strings"
)

// This file is the §12.9 rebuild of v1's sensitive-config handling for
// the modular v2 world: v1 scanned the user mega-struct in one pass;
// v2 modules own their Options types, so the mechanism lives here and
// modules consume it by tagging fields `sensitive:"true"` and (for
// %#v/%v safety) implementing GoString via RedactedGoString.

const redactedPlaceholder = "***"

// Redact returns a deep copy of a config struct with every field
// tagged sensitive:"true" replaced by "***". Nested structs, pointers,
// slices and maps are walked; map keys that look secret-shaped
// (isSensitiveKey) are masked heuristically. The input is never
// mutated. Non-struct inputs pass through unchanged.
//
//	logger.Info("db config", "opts", conf.Redact(opts))
func Redact(v any) any {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return v
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return v
	}
	return redactStruct(rv).Interface()
}

// GoString safety pattern for Options types that carry credentials:
// declare a method-less twin type and format the redacted twin —
// struct tags survive the conversion, methods do not, so %#v cannot
// re-enter GoString:
//
//	type optionsRaw Options // no methods → %#v prints raw fields
//	func (o Options) GoString() string {
//	    return fmt.Sprintf("%#v", conf.Redact(optionsRaw(o)))
//	}
//
// (Same twin trick v1's config package used; documented here because
// conf.Redact is the v2 mechanism modules pair it with.)

// redactStruct copies a struct value, masking sensitive fields.
func redactStruct(v reflect.Value) reflect.Value {
	out := reflect.New(v.Type()).Elem()
	out.Set(v)
	t := v.Type()
	for i := range t.NumField() {
		ft := t.Field(i)
		if !ft.IsExported() {
			continue
		}
		fv := out.Field(i)
		if ft.Tag.Get("sensitive") == "true" && ft.Type.Kind() == reflect.String {
			if fv.String() != "" {
				fv.SetString(redactedPlaceholder)
			}
			continue
		}
		fv.Set(redactAny(fv))
	}
	return out
}

// redactAny dispatches on kind: structs recurse, pointers/interfaces
// unwrap, maps get heuristic key masking, container slices recurse
// element-wise; scalars pass through.
func redactAny(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return v
		}
		inner := redactAny(v.Elem())
		ptr := reflect.New(v.Type().Elem())
		ptr.Elem().Set(inner)
		return ptr
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		inner := redactAny(v.Elem())
		out := reflect.New(v.Type()).Elem()
		out.Set(inner)
		return out
	case reflect.Struct:
		return redactStruct(v)
	case reflect.Map:
		return redactMap(v)
	case reflect.Slice, reflect.Array:
		ek := v.Type().Elem().Kind()
		if ek != reflect.Struct && ek != reflect.Ptr && ek != reflect.Interface && ek != reflect.Map {
			return v // []byte, []string etc: no tags inside, pass through
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(redactAny(v.Index(i)))
		}
		return out
	default:
		return v
	}
}

// redactMap returns a fresh map with secret-shaped keys masked and
// container values recursed. Maps are reference types — the caller's
// map is never touched.
func redactMap(v reflect.Value) reflect.Value {
	if v.IsNil() {
		return v
	}
	out := reflect.MakeMapWithSize(v.Type(), v.Len())
	iter := v.MapRange()
	for iter.Next() {
		k, val := iter.Key(), iter.Value()
		if k.Kind() == reflect.String && isSensitiveKey(k.String()) {
			masked := reflect.New(val.Type()).Elem()
			placeholder := reflect.ValueOf(redactedPlaceholder)
			if placeholder.Type().AssignableTo(val.Type()) || val.Kind() == reflect.Interface {
				masked.Set(placeholder)
			}
			out.SetMapIndex(k, masked)
			continue
		}
		out.SetMapIndex(k, redactAny(val))
	}
	return out
}

// isSensitiveKey reports whether a config key name looks like it holds
// a secret. Errs toward redaction: a false positive blanks a value in
// a diagnostic dump, a false negative leaks credentials.
func isSensitiveKey(name string) bool {
	lower := strings.ToLower(name)
	for _, tok := range []string{
		"secret", "password", "passwd", "private_key", "privatekey",
		"api_key", "apikey", "token", "signing_key", "client_secret",
		"dsn",
	} {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

// --- per-section sensitive paths (snapshot-level redaction) ----------

// sensitivePathsOf walks a registered section type and returns the
// dotted paths (relative to the section root) of every field tagged
// sensitive:"true". Registered at Register time so RedactedSettings
// can mask by exact path in addition to the key heuristic.
func sensitivePathsOf(t reflect.Type) []string {
	var out []string
	collectSensitivePaths(t, "", &out)
	return out
}

func collectSensitivePaths(t reflect.Type, prefix string, out *[]string) {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		key := mapKeyOf(f)
		if prefix != "" {
			key = prefix + "." + key
		}
		if f.Tag.Get("sensitive") == "true" {
			*out = append(*out, key)
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && !isAtomicStruct(ft) {
			collectSensitivePaths(ft, key, out)
		}
	}
}

// RedactedSettings returns a deep copy of the frozen tree with (a)
// every registered section's sensitive-tagged paths and (b) any
// secret-shaped key masked. This is the sink future config dumps
// (snapshot logs, /configz-style surfaces) must go through.
func (s *Snapshot) RedactedSettings() map[string]any {
	paths := make(map[string]bool)
	for key, t := range s.loader.sections {
		for _, p := range sensitivePathsOf(t) {
			paths[key+"."+p] = true
		}
	}
	out, _ := redactTree(s.settings, "", paths).(map[string]any)
	return out
}

// redactTree deep-copies a settings subtree, masking exact sensitive
// paths and heuristic keys. Non-empty scalar values at masked
// positions become "***"; empty strings/nils stay as-is (masking an
// absent secret would fake its presence).
func redactTree(v any, path string, sensitive map[string]bool) any {
	switch tv := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(tv))
		for k, val := range tv {
			childPath := k
			if path != "" {
				childPath = path + "." + k
			}
			if sensitive[childPath] || isSensitiveKey(k) {
				out[k] = maskScalar(val)
				continue
			}
			out[k] = redactTree(val, childPath, sensitive)
		}
		return out
	case []any:
		out := make([]any, len(tv))
		for i, e := range tv {
			out[i] = redactTree(e, path, sensitive)
		}
		return out
	default:
		return v
	}
}

// maskScalar masks a value that sits at a sensitive position. Empty
// values pass through so dumps still show "not configured".
func maskScalar(v any) any {
	switch tv := v.(type) {
	case nil:
		return nil
	case string:
		if tv == "" {
			return ""
		}
		return redactedPlaceholder
	default:
		return redactedPlaceholder
	}
}
