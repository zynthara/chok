package conf

import (
	"encoding"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Masked replaces a non-empty secret in Dump output.
const Masked = "***"

// Dump renders v as YAML under its yaml key names, with every field
// tagged secret:"true" masked. It is what --check-config prints.
func Dump(v any) ([]byte, error) {
	return yaml.Marshal(dumpValue(reflect.ValueOf(v), false))
}

func dumpValue(v reflect.Value, secret bool) *yaml.Node {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
		}
		v = v.Elem()
	}
	t := v.Type()
	scalar := func(tag, s string) *yaml.Node {
		if secret && s != "" {
			s, tag = Masked, "!!str"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: s}
	}
	switch {
	case t == durationType:
		return scalar("!!str", time.Duration(v.Int()).String())
	case t.Implements(textMarshalerType):
		b, err := v.Interface().(encoding.TextMarshaler).MarshalText()
		if err != nil {
			return scalar("!!str", err.Error())
		}
		return scalar("!!str", string(b))
	}
	switch t.Kind() {
	case reflect.Struct:
		n := &yaml.Node{Kind: yaml.MappingNode}
		for _, f := range fieldsOf(t) {
			n.Content = append(n.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: f.key},
				dumpValue(v.FieldByIndex(f.index), f.secret))
		}
		return n
	case reflect.Map:
		n := &yaml.Node{Kind: yaml.MappingNode}
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
		for _, k := range keys {
			n.Content = append(n.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k.String()},
				dumpValue(v.MapIndex(k), secret))
		}
		return n
	case reflect.Slice, reflect.Array:
		n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for i := range v.Len() {
			item := dumpValue(v.Index(i), secret)
			if item.Kind != yaml.ScalarNode {
				n.Style = 0
			}
			n.Content = append(n.Content, item)
		}
		return n
	case reflect.String:
		return scalar("!!str", v.String())
	case reflect.Bool:
		return scalar("!!bool", strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return scalar("!!int", strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return scalar("!!int", strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		return scalar("!!float", strconv.FormatFloat(v.Float(), 'g', -1, 64))
	}
	return scalar("!!str", v.String())
}
