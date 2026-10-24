package openapi

import (
	"encoding"
	"encoding/json/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Schema is the subset of JSON Schema 2020-12 the document uses.
type Schema struct {
	Ref                  string             `json:"$ref,omitzero"`
	Type                 any                `json:"type,omitzero"` // a name, or [name, "null"]
	Format               string             `json:"format,omitzero"`
	Description          string             `json:"description,omitzero"`
	Properties           map[string]*Schema `json:"properties,omitzero"`
	Required             []string           `json:"required,omitzero"`
	Items                *Schema            `json:"items,omitzero"`
	AdditionalProperties *Schema            `json:"additionalProperties,omitzero"`
	Enum                 []any              `json:"enum,omitzero"`
	Minimum              *float64           `json:"minimum,omitzero"`
	Maximum              *float64           `json:"maximum,omitzero"`
	ExclusiveMinimum     *float64           `json:"exclusiveMinimum,omitzero"`
	ExclusiveMaximum     *float64           `json:"exclusiveMaximum,omitzero"`
	MinLength            *int               `json:"minLength,omitzero"`
	MaxLength            *int               `json:"maxLength,omitzero"`
	MinItems             *int               `json:"minItems,omitzero"`
	MaxItems             *int               `json:"maxItems,omitzero"`
	Deprecated           bool               `json:"deprecated,omitzero"`
}

var (
	timeType        = reflect.TypeFor[time.Time]()
	durationType    = reflect.TypeFor[time.Duration]()
	textMarshaler   = reflect.TypeFor[encoding.TextMarshaler]()
	jsonMarshaler   = reflect.TypeFor[json.Marshaler]()
	jsonMarshalerTo = reflect.TypeFor[json.MarshalerTo]()
	bytesType       = reflect.TypeFor[[]byte]()
	sourceTags      = []string{"path", "query", "header"}
)

// registry holds the named types that became components.
type registry struct {
	schemas map[string]*Schema
	names   map[reflect.Type]string
	taken   map[string]reflect.Type
}

func newRegistry() *registry {
	return &registry{schemas: map[string]*Schema{}, names: map[reflect.Type]string{}, taken: map[string]reflect.Type{}}
}

// schemaOf returns the schema of t as a response or nested value: named
// structs become components referenced by $ref, anonymous ones inline.
func (g *registry) schemaOf(t reflect.Type) *Schema {
	nullable := false
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
		nullable = true
	}
	s := g.plain(t)
	if nullable && s.Ref == "" {
		s.Type = orNull(s.Type)
	}
	return s
}

func (g *registry) plain(t reflect.Type) *Schema {
	switch {
	case t == timeType:
		return &Schema{Type: "string", Format: "date-time"}
	case t == durationType:
		return &Schema{Type: "string", Description: "a duration such as 1h30m"}
	case t == bytesType:
		return &Schema{Type: "string", Format: "byte"}
	case implements(t, jsonMarshaler) || implements(t, jsonMarshalerTo):
		return &Schema{}
	case implements(t, textMarshaler):
		return &Schema{Type: "string"}
	}
	switch t.Kind() {
	case reflect.String:
		return &Schema{Type: "string"}
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}
	case reflect.Slice, reflect.Array:
		return &Schema{Type: "array", Items: g.schemaOf(t.Elem())}
	case reflect.Map:
		return &Schema{Type: "object", AdditionalProperties: g.schemaOf(t.Elem())}
	case reflect.Struct:
		if t.Name() == "" {
			return g.object(t, false)
		}
		return &Schema{Ref: "#/components/schemas/" + g.component(t)}
	}
	return &Schema{}
}

// component registers named struct t and returns its component name.
func (g *registry) component(t reflect.Type) string {
	if name, ok := g.names[t]; ok {
		return name
	}
	name := typeName(t)
	if other, taken := g.taken[name]; taken && other != t {
		name = pkgBase(t.PkgPath()) + "." + name
	}
	g.names[t], g.taken[name] = name, t
	g.schemas[name] = &Schema{Type: "object"} // placeholder: breaks recursion
	g.schemas[name] = g.object(t, false)
	return name
}

// object builds the properties of struct t. In request mode the fields
// that bind from the path, the query or a header are left out: they are
// parameters, not body members.
func (g *registry) object(t reflect.Type, request bool) *Schema {
	s := &Schema{Type: "object", Properties: map[string]*Schema{}}
	g.fields(t, request, s)
	if len(s.Properties) == 0 {
		s.Properties = nil
	}
	return s
}

func (g *registry) fields(t reflect.Type, request bool, out *Schema) {
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if tag == "-" || (request && hasSourceTag(f)) {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if (f.Anonymous && name == "") || hasOpt(opts, "inline") {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				g.fields(ft, request, out)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		prop := g.schemaOf(f.Type)
		if hasOpt(opts, "string") {
			prop = &Schema{Type: "string"}
		}
		required := applyRules(f.Tag.Get("validate"), prop)
		out.Properties[name] = prop
		if required {
			out.Required = append(out.Required, name)
		}
	}
	slices.Sort(out.Required)
}

func hasOpt(opts, opt string) bool {
	for o := range strings.SplitSeq(opts, ",") {
		if o == opt {
			return true
		}
	}
	return false
}

func hasSourceTag(f reflect.StructField) bool {
	for _, tag := range sourceTags {
		if v, ok := f.Tag.Lookup(tag); ok && v != "-" {
			return true
		}
	}
	return false
}

// applyRules maps validate rules onto s and reports whether the field is
// required.
func applyRules(rules string, s *Schema) bool {
	required := false
	target := s
	for rule := range strings.SplitSeq(rules, ",") {
		key, param, _ := strings.Cut(strings.TrimSpace(rule), "=")
		switch key {
		case "required":
			required = true
		case "dive":
			if s.Items != nil {
				target = s.Items
			}
		case "email":
			target.Format = "email"
		case "url", "http_url", "uri":
			target.Format = "uri"
		case "uuid", "uuid4":
			target.Format = "uuid"
		case "oneof":
			for v := range strings.FieldsSeq(param) {
				target.Enum = append(target.Enum, enumValue(target, v))
			}
		case "min", "gte":
			bound(target, param, false, false)
		case "max", "lte":
			bound(target, param, true, false)
		case "gt":
			bound(target, param, false, true)
		case "lt":
			bound(target, param, true, true)
		case "len":
			bound(target, param, false, false)
			bound(target, param, true, false)
		}
	}
	return required
}

func enumValue(s *Schema, v string) any {
	switch baseType(s.Type) {
	case "integer":
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	case "number":
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return v
}

// bound applies a length, size or range limit depending on the type.
func bound(s *Schema, param string, upper, exclusive bool) {
	f, err := strconv.ParseFloat(param, 64)
	if err != nil {
		return
	}
	n := int(f)
	switch baseType(s.Type) {
	case "string":
		if upper {
			s.MaxLength = &n
		} else {
			s.MinLength = &n
		}
	case "array":
		if upper {
			s.MaxItems = &n
		} else {
			s.MinItems = &n
		}
	case "integer", "number":
		switch {
		case upper && exclusive:
			s.ExclusiveMaximum = &f
		case upper:
			s.Maximum = &f
		case exclusive:
			s.ExclusiveMinimum = &f
		default:
			s.Minimum = &f
		}
	}
}

func baseType(t any) string {
	switch v := t.(type) {
	case string:
		return v
	case []string:
		return v[0]
	}
	return ""
}

func orNull(t any) any {
	if s, ok := t.(string); ok && s != "" {
		return []string{s, "null"}
	}
	return t
}

func implements(t, iface reflect.Type) bool {
	return t.Implements(iface) || reflect.PointerTo(t).Implements(iface)
}

// typeName turns a Go type name into a component name: Post stays Post,
// Page[example.com/blog.Post] becomes Page_Post.
func typeName(t reflect.Type) string {
	name := t.Name()
	open := strings.IndexByte(name, '[')
	if open < 0 {
		return name
	}
	var parts []string
	for arg := range strings.SplitSeq(name[open+1:len(name)-1], ",") {
		arg = strings.TrimSpace(arg)
		if i := strings.LastIndexAny(arg, "./"); i >= 0 {
			arg = arg[i+1:]
		}
		parts = append(parts, strings.Trim(arg, "*[]"))
	}
	return name[:open] + "_" + strings.Join(parts, "_")
}

func pkgBase(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}
