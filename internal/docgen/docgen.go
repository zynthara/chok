// Package docgen renders the generated documentation surfaces —
// components table, configuration reference, JSON Schema — from the
// blessed inventory plus reflection over each module's Options type
// (axiom 5: docs are outputs, Descriptor and Options are the truth).
//
// The conf loader's walking rules are mirrored exactly (mapstructure
// keys, default tags, atomic structs, maps as dynamic leaves) so what
// the schema promises is what the loader does.
package docgen

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/zynthara/chok/v2/internal/blessed"
	"github.com/zynthara/chok/v2/kernel"
)

// Capability names follow the kernel behavior interfaces.
func capabilities(c kernel.Component) []string {
	var caps []string
	if _, ok := c.(kernel.Reloader); ok {
		caps = append(caps, "reload")
	}
	if _, ok := c.(kernel.Healther); ok {
		caps = append(caps, "health")
	}
	if _, ok := c.(kernel.Mounter); ok {
		caps = append(caps, "mount")
	}
	if _, ok := c.(kernel.Migrator); ok {
		caps = append(caps, "migrate")
	}
	if _, ok := c.(kernel.Readier); ok {
		caps = append(caps, "ready")
	}
	if _, ok := c.(kernel.Server); ok {
		caps = append(caps, "serve")
	}
	if _, ok := c.(kernel.Drainer); ok {
		caps = append(caps, "drain")
	}
	if _, ok := c.(kernel.RouterProvider); ok {
		caps = append(caps, "router")
	}
	return caps
}

func needs(d kernel.Descriptor) string {
	if len(d.Needs) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(d.Needs))
	for _, dep := range d.Needs {
		s := dep.Kind
		if dep.Optional {
			s += "?"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// enabledDefault reads the Options `enabled` field's default tag.
// Modules without an enabled field are always-on once assembled.
func enabledDefault(d kernel.Descriptor) string {
	if d.Options == nil {
		return "always"
	}
	t := reflect.TypeOf(d.Options)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return "always"
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if mapKeyOf(f) == "enabled" {
			if f.Tag.Get("default") == "false" {
				return "false"
			}
			return "true"
		}
	}
	return "always"
}

// ComponentsTable renders the generated markdown table. lang is "en"
// or "zh" (descriptions and headers switch, structure does not).
func ComponentsTable(lang string) string {
	var b strings.Builder
	if lang == "zh" {
		b.WriteString("| 模块 | 配置段 | 依赖（`?` = 软依赖） | 能力 | 默认启用 | 说明 |\n")
	} else {
		b.WriteString("| Module | Section | Needs (`?` = optional) | Capabilities | Enabled by default | What it does |\n")
	}
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, m := range blessed.Modules() {
		inst := m.New()
		d := inst.Describe()
		desc := m.DescEN
		if lang == "zh" {
			desc = m.DescZH
		}
		caps := strings.Join(capabilities(inst), ", ")
		if caps == "" {
			caps = "—"
		}
		fmt.Fprintf(&b, "| `%s.Module()` | `%s` | %s | %s | %s | %s |\n",
			m.Pkg, kernel.SectionKeyOf(d), needs(d), caps, enabledDefault(d), desc)
	}
	return b.String()
}

// --- struct walking (mirrors conf/conf.go rules) ----------------------------

func mapKeyOf(f reflect.StructField) string {
	key := f.Tag.Get("mapstructure")
	if comma := strings.IndexByte(key, ','); comma >= 0 {
		key = key[:comma]
	}
	if key == "" {
		key = strings.ToLower(f.Name)
	}
	return key
}

func isAtomicStruct(t reflect.Type) bool {
	if t.String() == "time.Time" {
		return true
	}
	for i := range t.NumField() {
		if t.Field(i).IsExported() {
			return false
		}
	}
	return true
}

// field is one flattened leaf (or dynamic-map node) of a section.
type field struct {
	Path      string // dot-joined mapstructure path within the section
	Type      string // human type ("string", "bool", "duration", ...)
	Default   string
	Reload    string // "hot" | "restart"
	Sensitive bool
	IsMap     bool
}

// walkOptions flattens an Options struct into leaves, inheriting
// reload tags exactly like conf's diff walker (untagged = restart,
// nested fields inherit the enclosing tag unless they override).
func walkOptions(t reflect.Type) []field {
	var out []field
	var walk func(t reflect.Type, prefix, inheritedReload string)
	walk = func(t reflect.Type, prefix, inheritedReload string) {
		for t.Kind() == reflect.Ptr {
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
			rel := f.Tag.Get("reload")
			if rel == "" {
				rel = inheritedReload
			}
			eff := rel
			if eff == "" {
				eff = "restart"
			}
			ft := f.Type
			for ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			switch {
			case ft.Kind() == reflect.Struct && !isAtomicStruct(ft):
				walk(ft, key, rel)
			case ft.Kind() == reflect.Map:
				out = append(out, field{Path: key, Type: "map", Reload: eff,
					Sensitive: f.Tag.Get("sensitive") == "true", IsMap: true})
			case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct && !isAtomicStruct(ft.Elem()):
				// e.g. log files: list the element fields under path[].
				base := field{Path: key + "[]", Type: "list(object)", Reload: eff,
					Sensitive: f.Tag.Get("sensitive") == "true"}
				out = append(out, base)
				elemFields := walkOptions(ft.Elem())
				for _, ef := range elemFields {
					ef.Path = key + "[]." + ef.Path
					if ef.Reload == "restart" && eff == "hot" {
						// element fields inherit the slice's tag unless overridden
						ef.Reload = eff
					}
					out = append(out, ef)
				}
			default:
				out = append(out, field{
					Path:      key,
					Type:      typeName(ft),
					Default:   f.Tag.Get("default"),
					Reload:    eff,
					Sensitive: f.Tag.Get("sensitive") == "true",
				})
			}
		}
	}
	walk(t, "", "")
	return out
}

func typeName(t reflect.Type) string {
	switch {
	case t.String() == "time.Duration":
		return "duration"
	case t.Kind() == reflect.Slice:
		return "list(" + typeName(t.Elem()) + ")"
	case t.Kind() == reflect.Bool:
		return "bool"
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Uint64:
		return "int"
	case t.Kind() == reflect.Float32 || t.Kind() == reflect.Float64:
		return "float"
	default:
		return t.Kind().String()
	}
}

// ConfigReference renders docs/config.md: one table per section, in
// canonical order.
func ConfigReference() string {
	var b strings.Builder
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("<!-- Code generated by chok docs gen; DO NOT EDIT. -->\n\n")
	b.WriteString("Generated from each module's Options type. `reload: hot` fields\n")
	b.WriteString("apply on SIGHUP / config-watch reload; everything else needs a\n")
	b.WriteString("restart (the reload diff warns instead of dispatching). `enabled`\n")
	b.WriteString("is always restart-only by framework rule. Sensitive values are\n")
	b.WriteString("redacted in logs and should ride environment variables in\n")
	b.WriteString("production (`<APP>_<SECTION>_<KEY>`).\n\n")
	b.WriteString("Named instances: sections marked *multi-instance* accept an\n")
	b.WriteString("`instances.<name>` subtree with the same fields (env:\n")
	b.WriteString("`<APP>_DB_INSTANCES_<NAME>_...`).\n")

	for _, m := range blessed.Modules() {
		d := m.New().Describe()
		section := kernel.SectionKeyOf(d)
		fmt.Fprintf(&b, "\n## `%s` — %s\n\n", section, m.DescEN)
		if m.MultiInstance {
			b.WriteString("*Multi-instance.*\n\n")
		}
		if d.Options == nil {
			b.WriteString("No configuration.\n")
			continue
		}
		b.WriteString("| Key | Type | Default | Reload | Notes |\n|---|---|---|---|---|\n")
		t := reflect.TypeOf(d.Options)
		for _, f := range walkOptions(t) {
			var notes []string
			if f.Sensitive {
				notes = append(notes, "**sensitive**")
			}
			if enum, ok := m.Enums[f.Path]; ok {
				notes = append(notes, "one of: "+strings.Join(enum, " \\| "))
			}
			if f.IsMap {
				notes = append(notes, "dynamic keys")
			}
			def := f.Default
			if def == "" {
				def = "—"
			} else {
				def = "`" + def + "`"
			}
			note := strings.Join(notes, "; ")
			if note == "" {
				note = "—"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", f.Path, f.Type, def, f.Reload, note)
		}
	}
	return b.String()
}

// InjectBlock replaces the region between the gen markers in doc,
// returning the new content. Missing markers is an error — the file
// must opt in to generation explicitly.
func InjectBlock(doc, name, content string) (string, error) {
	open := fmt.Sprintf("<!-- gen:%s -->", name)
	close := fmt.Sprintf("<!-- /gen:%s -->", name)
	i := strings.Index(doc, open)
	j := strings.Index(doc, close)
	if i < 0 || j < 0 || j < i {
		return "", fmt.Errorf("docgen: markers %s / %s not found (or reversed)", open, close)
	}
	return doc[:i+len(open)] + "\n" + strings.TrimRight(content, "\n") + "\n" + doc[j:], nil
}

// SortedSections returns the canonical section list (docs + schema
// iterate it so output is deterministic).
func SortedSections() []string {
	var keys []string
	for _, m := range blessed.Modules() {
		keys = append(keys, kernel.SectionKeyOf(m.New().Describe()))
	}
	sort.Strings(keys)
	return keys
}
