package docgen

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ValidateYAMLTree checks a decoded yaml tree against the generated
// schema. It implements exactly the subset JSONSchema emits — type /
// properties / additionalProperties / enum / items — which keeps the
// checker dependency-free and impossible to drift ahead of the
// generator. Returns every violation, not just the first.
func ValidateYAMLTree(schemaJSON []byte, tree map[string]any) []error {
	var schema map[string]any
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return []error{fmt.Errorf("docgen: schema unmarshal: %w", err)}
	}
	var errs []error
	validateNode(schema, normalize(tree), "$", &errs)
	return errs
}

// normalize converts yaml.v3 map[string]any trees (and their nested
// values) into plain JSON-ish values.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[strings.ToLower(k)] = normalize(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalize(val)
		}
		return out
	default:
		return v
	}
}

var durationRe = regexp.MustCompile(`^-?([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`)

func validateNode(schema map[string]any, v any, path string, errs *[]error) {
	if v == nil {
		return // bare `section:` — presence only, nothing to check
	}
	if enum, ok := schema["enum"].([]any); ok {
		for _, e := range enum {
			if fmt.Sprintf("%v", e) == fmt.Sprintf("%v", v) {
				return
			}
		}
		*errs = append(*errs, fmt.Errorf("%s: %v is not one of %v", path, v, enum))
		return
	}
	types := typeList(schema["type"])
	if len(types) > 0 && !matchesAny(types, v) {
		*errs = append(*errs, fmt.Errorf("%s: %v (%T) does not match type %v", path, v, v, types))
		return
	}
	obj, isObj := v.(map[string]any)
	if !isObj {
		if arr, isArr := v.([]any); isArr {
			if items, ok := schema["items"].(map[string]any); ok {
				for i, el := range arr {
					validateNode(items, el, fmt.Sprintf("%s[%d]", path, i), errs)
				}
			}
		}
		return
	}
	props, _ := schema["properties"].(map[string]any)
	for key, val := range obj {
		if ps, ok := props[key].(map[string]any); ok {
			validateNode(ps, val, path+"."+key, errs)
			continue
		}
		switch ap := schema["additionalProperties"].(type) {
		case bool:
			if !ap {
				*errs = append(*errs, fmt.Errorf("%s.%s: unknown key", path, key))
			}
		case map[string]any:
			validateNode(ap, val, path+"."+key, errs)
		default:
			// absent = allowed (JSON Schema default)
		}
	}
}

func typeList(t any) []string {
	switch v := t.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func matchesAny(types []string, v any) bool {
	for _, t := range types {
		if matches(t, v) {
			return true
		}
	}
	return false
}

func matches(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		switch v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64: // JSON round-trip
			f := v.(float64)
			return f == float64(int64(f))
		}
		return false
	case "number":
		switch v.(type) {
		case int, int64, float32, float64:
			return true
		}
		return false
	case "string":
		s, ok := v.(string)
		if !ok {
			return false
		}
		// Durations are typed ["string","integer"]; when only string
		// is allowed the value is free-form, so no extra check — but a
		// string that *looks* like it should be a duration for a
		// duration-typed field is validated by the caller's type list.
		_ = durationRe // kept for potential pattern tightening
		_ = s
		return true
	}
	return false
}
