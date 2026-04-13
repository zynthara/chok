package swagger

import (
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Schema is a simplified OpenAPI 3.0 Schema Object.
type Schema struct {
	Type       string            `json:"type,omitempty"`
	Format     string            `json:"format,omitempty"`
	Properties map[string]*Schema `json:"properties,omitempty"`
	Items      *Schema           `json:"items,omitempty"`
	Required   []string          `json:"required,omitempty"`
	Enum       []string          `json:"enum,omitempty"`
	Minimum    *float64          `json:"minimum,omitempty"`
	Maximum    *float64          `json:"maximum,omitempty"`
	MinLength  *int              `json:"minLength,omitempty"`
	MaxLength  *int              `json:"maxLength,omitempty"`
	Nullable   bool              `json:"nullable,omitempty"`
}

var timeType = reflect.TypeOf(time.Time{})

// schemaFromType generates a JSON Schema from a Go type.
// source filters fields by tag: "json" for body, "" for all.
func schemaFromType(t reflect.Type, source string) *Schema {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == timeType {
		return &Schema{Type: "string", Format: "date-time"}
	}
	if t.Kind() != reflect.Struct {
		return typeSchema(t)
	}

	s := &Schema{Type: "object", Properties: make(map[string]*Schema)}
	collectFields(t, source, s)
	return s
}

func collectFields(t reflect.Type, source string, out *Schema) {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		// Recurse into embedded structs.
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectFields(ft, source, out)
			}
			continue
		}

		// Determine the source tag (uri, form, or json).
		name, fieldSource := fieldName(f)
		if name == "" || name == "-" {
			continue
		}
		// If a specific source is requested, skip fields from other sources.
		if source != "" && fieldSource != source {
			continue
		}

		ft := f.Type
		nullable := false
		if ft.Kind() == reflect.Ptr {
			nullable = true
			ft = ft.Elem()
		}

		prop := typeSchema(ft)
		if nullable {
			prop.Nullable = true
		}
		applyValidation(f, prop)
		out.Properties[name] = prop

		// Required: binding:"required" and not a pointer.
		if !nullable && hasBindingRule(f, "required") {
			out.Required = append(out.Required, name)
		}
	}
}

// fieldName returns the public name and source tag of a struct field.
func fieldName(f reflect.StructField) (string, string) {
	for _, tag := range []string{"uri", "form", "json"} {
		if v := f.Tag.Get(tag); v != "" && v != "-" {
			name, _, _ := strings.Cut(v, ",")
			if name != "" && name != "-" {
				return name, tag
			}
		}
	}
	return "", ""
}

// typeSchema returns the schema for a basic Go type.
func typeSchema(t reflect.Type) *Schema {
	if t == timeType {
		return &Schema{Type: "string", Format: "date-time"}
	}
	switch t.Kind() {
	case reflect.String:
		return &Schema{Type: "string"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return &Schema{Type: "integer"}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Slice:
		items := typeSchema(t.Elem())
		return &Schema{Type: "array", Items: items}
	case reflect.Struct:
		return schemaFromType(t, "json")
	default:
		return &Schema{Type: "string"}
	}
}

// applyValidation reads binding tags and sets schema constraints.
func applyValidation(f reflect.StructField, s *Schema) {
	binding := f.Tag.Get("binding")
	if binding == "" {
		return
	}
	for _, rule := range strings.Split(binding, ",") {
		rule = strings.TrimSpace(rule)
		switch {
		case rule == "email":
			s.Format = "email"
		case strings.HasPrefix(rule, "oneof="):
			s.Enum = strings.Fields(strings.TrimPrefix(rule, "oneof="))
		case strings.HasPrefix(rule, "min="):
			if v, err := strconv.ParseFloat(strings.TrimPrefix(rule, "min="), 64); err == nil {
				if s.Type == "string" {
					iv := int(v)
					s.MinLength = &iv
				} else {
					s.Minimum = &v
				}
			}
		case strings.HasPrefix(rule, "max="):
			if v, err := strconv.ParseFloat(strings.TrimPrefix(rule, "max="), 64); err == nil {
				if s.Type == "string" {
					iv := int(v)
					s.MaxLength = &iv
				} else {
					s.Maximum = &v
				}
			}
		}
	}
}

func hasBindingRule(f reflect.StructField, rule string) bool {
	binding := f.Tag.Get("binding")
	for _, r := range strings.Split(binding, ",") {
		if strings.TrimSpace(r) == rule {
			return true
		}
	}
	return false
}

// extractParams extracts path and query parameters from a request struct.
func extractParams(t reflect.Type) []Parameter {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var params []Parameter
	scanParams(t, &params)
	return params
}

func scanParams(t reflect.Type, out *[]Parameter) {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				scanParams(ft, out)
			}
			continue
		}

		var in, name string
		if v := f.Tag.Get("uri"); v != "" && v != "-" {
			in = "path"
			name, _, _ = strings.Cut(v, ",")
		} else if v := f.Tag.Get("form"); v != "" && v != "-" {
			in = "query"
			name, _, _ = strings.Cut(v, ",")
		}
		if in == "" {
			continue
		}

		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		p := Parameter{
			Name:     name,
			In:       in,
			Required: in == "path" || hasBindingRule(f, "required"),
			Schema:   typeSchema(ft),
		}
		applyValidation(f, p.Schema)
		*out = append(*out, p)
	}
}
