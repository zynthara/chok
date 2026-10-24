package conf

import (
	"encoding"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	durationType        = reflect.TypeFor[time.Duration]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	textMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
)

// field is one yaml key of a struct type. Embedded structs without a
// yaml tag (or tagged ",inline") are flattened into their parent, so
// their keys sit at the parent's level — chok.Base inside an app's
// Config contributes app, http and log as top-level sections.
type field struct {
	key    string
	name   string // Go field name, used to map validator namespaces
	index  []int
	typ    reflect.Type
	def    string
	hasDef bool
	secret bool
}

var fieldCache sync.Map // reflect.Type → []field

// fieldsOf returns the yaml keys of struct type t in declaration order.
func fieldsOf(t reflect.Type) []field {
	if f, ok := fieldCache.Load(t); ok {
		return f.([]field)
	}
	var out []field
	collectFields(t, nil, &out)
	fieldCache.Store(t, out)
	return out
}

func collectFields(t reflect.Type, index []int, out *[]field) {
	for i := range t.NumField() {
		sf := t.Field(i)
		name, inline := parseYAMLTag(sf.Tag.Get("yaml"))
		if name == "-" {
			continue
		}
		idx := append(slices.Clone(index), i)
		if (sf.Anonymous && name == "") || inline {
			if sf.Type.Kind() == reflect.Struct {
				collectFields(sf.Type, idx, out)
				continue
			}
		}
		if !sf.IsExported() {
			continue
		}
		if name == "" {
			name = strings.ToLower(sf.Name)
		}
		def, hasDef := sf.Tag.Lookup("default")
		*out = append(*out, field{
			key:    name,
			name:   sf.Name,
			index:  idx,
			typ:    sf.Type,
			def:    def,
			hasDef: hasDef,
			secret: sf.Tag.Get("secret") == "true",
		})
	}
}

func parseYAMLTag(tag string) (name string, inline bool) {
	name, opts, _ := strings.Cut(tag, ",")
	for opt := range strings.SplitSeq(opts, ",") {
		if opt == "inline" {
			inline = true
		}
	}
	return name, inline
}

// isScalar reports whether t is decoded from a single YAML scalar.
func isScalar(t reflect.Type) bool {
	if t == durationType {
		return true
	}
	if reflect.PointerTo(t).Implements(textUnmarshalerType) {
		return true
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// isLeaf reports whether a value of type t is set as a whole from one
// environment variable or flag: a scalar or a list of scalars.
func isLeaf(t reflect.Type) bool {
	t = deref(t)
	if isScalar(t) {
		return true
	}
	return t.Kind() == reflect.Slice && isScalar(deref(t.Elem()))
}

// isSection reports whether t is a struct that holds keys of its own.
func isSection(t reflect.Type) bool {
	t = deref(t)
	return t.Kind() == reflect.Struct && !isScalar(t)
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// Key describes one leaf of a configuration type, for documentation and
// for the configkeys budget. Map entries appear once with "<name>" for
// the key and list elements with "[]".
type Key struct {
	Path    string
	Type    string
	Default string
	Secret  bool
}

// Keys lists the leaf keys of struct type t, sorted by path.
func Keys(t reflect.Type) []Key {
	var out []Key
	collectKeys(deref(t), "", &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func collectKeys(t reflect.Type, path string, out *[]Key) {
	for _, f := range fieldsOf(t) {
		p := join(path, f.key)
		ft := deref(f.typ)
		switch {
		case isLeaf(ft):
			*out = append(*out, Key{Path: p, Type: typeName(f.typ), Default: f.def, Secret: f.secret})
		case isSection(ft):
			collectKeys(ft, p, out)
		case ft.Kind() == reflect.Map && isSection(ft.Elem()):
			collectKeys(deref(ft.Elem()), p+".<name>", out)
		case ft.Kind() == reflect.Slice && isSection(ft.Elem()):
			collectKeys(deref(ft.Elem()), p+"[]", out)
		default:
			*out = append(*out, Key{Path: p, Type: typeName(f.typ), Default: f.def, Secret: f.secret})
		}
	}
}

func typeName(t reflect.Type) string {
	t = deref(t)
	switch {
	case t == durationType:
		return "duration"
	case t.Kind() == reflect.Slice:
		return "list of " + typeName(t.Elem())
	case t.Kind() == reflect.Map:
		return "map of " + typeName(t.Elem())
	}
	return t.Kind().String()
}

// suggest returns " (did you mean x?)" for the closest candidate within
// edit distance 2, or "".
func suggest(word string, candidates []string) string {
	best, bestDist := "", 3
	for _, c := range candidates {
		if d := editDistance(word, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	if best == "" {
		return ""
	}
	return " (did you mean " + best + "?)"
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
