package conf

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/spf13/pflag"
	"go.yaml.in/yaml/v3"
)

// The raw tree every source is turned into before decoding: mappings are
// map[string]any, sequences []any and scalars their text as a string.
// A nil value means "not set here" and never overrides a lower source.

// parseYAML reads a YAML document into a raw tree, recording the line of
// every key so problems can point at the file.
func parseYAML(data []byte, lines map[string]int) (map[string]any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	v, err := nodeValue(&doc, "", lines)
	if err != nil {
		return nil, err
	}
	switch m := v.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return m, nil
	default:
		return nil, fmt.Errorf("the top level must be a mapping of sections")
	}
}

func nodeValue(n *yaml.Node, path string, lines map[string]int) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return nodeValue(n.Content[0], path, lines)
	case yaml.AliasNode:
		return nodeValue(n.Alias, path, lines)
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			return nil, nil
		}
		return n.Value, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for i, c := range n.Content {
			v, err := nodeValue(c, fmt.Sprintf("%s[%d]", path, i), lines)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		out := map[string]any{}
		var merged []map[string]any
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, val := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: a key must be a plain value", k.Line)
			}
			if k.Tag == "!!merge" {
				m, err := mergeSources(val, path, lines)
				if err != nil {
					return nil, err
				}
				merged = append(merged, m...)
				continue
			}
			if _, dup := out[k.Value]; dup {
				return nil, fmt.Errorf("line %d: duplicate key %s", k.Line, join(path, k.Value))
			}
			p := join(path, k.Value)
			lines[p] = k.Line
			v, err := nodeValue(val, p, lines)
			if err != nil {
				return nil, err
			}
			out[k.Value] = v
		}
		// Merge keys (<<: *anchor) fill only what the mapping does not set.
		for _, m := range merged {
			for k, v := range m {
				if _, set := out[k]; !set {
					out[k] = v
				}
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
}

func mergeSources(n *yaml.Node, path string, lines map[string]int) ([]map[string]any, error) {
	if n.Kind == yaml.SequenceNode {
		var out []map[string]any
		for _, c := range n.Content {
			m, err := mergeSources(c, path, lines)
			if err != nil {
				return nil, err
			}
			out = append(out, m...)
		}
		return out, nil
	}
	v, err := nodeValue(n, path, lines)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("line %d: a merge key (<<) needs a mapping", n.Line)
	}
	return []map[string]any{m}, nil
}

// merge lays src over dst: mappings merge key by key, anything else
// replaces, nil never overrides.
func merge(dst, src map[string]any) {
	for k, v := range src {
		if v == nil {
			continue
		}
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				merge(dm, sm)
				continue
			}
			cp := map[string]any{}
			merge(cp, sm)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

// setPath stores v at a dotted path, creating mappings on the way.
func setPath(tree map[string]any, path string, v any) {
	keys := strings.Split(path, ".")
	m := tree
	for _, k := range keys[:len(keys)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[keys[len(keys)-1]] = v
}

func subtree(tree map[string]any, key string) map[string]any {
	if tree == nil {
		return nil
	}
	m, _ := tree[key].(map[string]any)
	return m
}

// EnvName is the environment variable that overrides a key path.
func EnvName(prefix, path string) string {
	return prefix + "_" + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(path))
}

// leaves maps every settable leaf path of t to its env variable name.
// Map entries are known only through base (the file), so an environment
// variable cannot invent a map key the file does not mention.
func leaves(t reflect.Type, prefix, path string, base map[string]any, out map[string]string) {
	for _, f := range fieldsOf(t) {
		p := join(path, f.key)
		ft := deref(f.typ)
		switch {
		case isLeaf(ft):
			out[p] = EnvName(prefix, p)
		case isSection(ft):
			leaves(ft, prefix, p, subtree(base, f.key), out)
		case ft.Kind() == reflect.Map:
			entries := subtree(base, f.key)
			for _, k := range slices.Sorted(maps.Keys(entries)) {
				switch et := deref(ft.Elem()); {
				case isSection(et):
					leaves(et, prefix, join(p, k), subtree(entries, k), out)
				case isLeaf(et):
					out[join(p, k)] = EnvName(prefix, join(p, k))
				}
			}
		}
	}
}

// envLayer collects the environment overrides and warns about variables
// with the prefix that match no key.
func envLayer(paths map[string]string, prefix string, environ []string) (map[string]any, []string) {
	byName := make(map[string]string, len(paths))
	names := make([]string, 0, len(paths))
	for p, n := range paths {
		byName[n] = p
		names = append(names, n)
	}
	layer := map[string]any{}
	var warnings []string
	for _, kv := range environ {
		name, val, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, prefix+"_") || name == prefix+"_CONFIG" {
			continue
		}
		if p, known := byName[name]; known {
			setPath(layer, p, val)
			continue
		}
		warnings = append(warnings, fmt.Sprintf("environment variable %s matches no config key%s", name, suggest(name, names)))
	}
	slices.Sort(warnings)
	return layer, warnings
}

// flagLayer takes the changed flags whose names are key paths.
func flagLayer(fs *pflag.FlagSet, paths map[string]string) map[string]any {
	layer := map[string]any{}
	if fs == nil {
		return layer
	}
	fs.Visit(func(f *pflag.Flag) {
		if _, known := paths[f.Name]; !known {
			return
		}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			items := []any{}
			for _, s := range sv.GetSlice() {
				items = append(items, s)
			}
			setPath(layer, f.Name, items)
			return
		}
		setPath(layer, f.Name, f.Value.String())
	})
	return layer
}
