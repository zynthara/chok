package conf

import (
	"reflect"
)

// SectionChange is the tag-classified difference of one registered
// section between two snapshots (SPEC §3.4).
type SectionChange struct {
	Key string

	// Hot lists changed field paths tagged reload:"hot" — the owning
	// component gets a Reload call.
	Hot []string

	// Restart lists changed field paths that are restart-only (tagged
	// "restart" or untagged — the conservative default). The framework
	// emits one unified warn per field; no component dispatch.
	Restart []string

	// EnabledFlipped records a change of the section-root `enabled`
	// switch. Always restart-only (framework rule, tags cannot
	// override): the registry warns and does NOT hot-toggle the
	// component (SPEC §3.1 definition 4).
	EnabledFlipped bool
}

// Changed reports whether the section differs at all.
func (c SectionChange) Changed() bool {
	return len(c.Hot) > 0 || len(c.Restart) > 0 || c.EnabledFlipped
}

// Diff is the outcome of one Store.Reload against the previous
// snapshot.
type Diff struct {
	// Changed is the whole-tree verdict (registered sections plus any
	// dynamic/unregistered keys). Drives the "was there a config
	// change at all" contract — no-ConfigKey Reloaders dispatch on it.
	Changed bool

	// Sections maps each registered section key to its classified
	// change (zero-value entry when the section is untouched).
	Sections map[string]SectionChange
}

// diffSnapshots decodes every registered section from both snapshots
// and classifies field-level changes by reload tag.
func diffSnapshots(old, fresh *Snapshot, l *Loader) *Diff {
	d := &Diff{
		Changed:  !reflect.DeepEqual(old.settings, fresh.settings),
		Sections: make(map[string]SectionChange, len(l.sections)),
	}
	for _, key := range l.SectionKeys() {
		ch := SectionChange{Key: key}
		oldVal, oerr := old.decodeRegistered(key)
		newVal, nerr := fresh.decodeRegistered(key)
		if oerr == nil && nerr == nil {
			diffStructs(oldVal, newVal, key, "", true, &ch)
		}
		// Decode errors cannot happen for fresh (validated at build);
		// a stale old decode error degrades to "no per-field info".
		d.Sections[key] = ch
	}
	return d
}

// diffStructs walks two equally-typed struct values, classifying each
// changed leaf by its effective reload tag.
//
// Tag rules (SPEC §3.4): a field's own reload tag wins; absent tags
// inherit the nearest ancestor field's tag; a tree with no tag at all
// is restart (conservative). Maps, slices and atomic structs compare
// as whole values under a single tag — no element-level diff.
// SelfValidating types participate normally (that interface only
// bounds validation recursion). The section-root `enabled` field is
// forced restart regardless of tags.
func diffStructs(old, fresh reflect.Value, path, inherited string, isRoot bool, out *SectionChange) {
	t := old.Type()
	for i := range t.NumField() {
		ft := t.Field(i)
		if !ft.IsExported() {
			continue
		}
		fieldPath := path + "." + mapKeyOf(ft)

		effective := ft.Tag.Get("reload")
		if effective == "" {
			effective = inherited
		}

		ov, nv := old.Field(i), fresh.Field(i)

		// Section-root enabled: framework-owned restart-only switch.
		if isRoot && mapKeyOf(ft) == "enabled" {
			if !reflect.DeepEqual(ov.Interface(), nv.Interface()) {
				out.EnabledFlipped = true
				out.Restart = append(out.Restart, fieldPath)
			}
			continue
		}

		// Pointer fields: nil-ness change is a leaf change; both
		// non-nil dereferences and structs recurse below.
		if ov.Kind() == reflect.Ptr {
			if ov.IsNil() != nv.IsNil() {
				classify(out, fieldPath, effective)
				continue
			}
			if ov.IsNil() {
				continue
			}
			ov, nv = ov.Elem(), nv.Elem()
		}

		if ov.Kind() == reflect.Struct && !isAtomicStruct(ov.Type()) {
			diffStructs(ov, nv, fieldPath, effective, false, out)
			continue
		}

		if !reflect.DeepEqual(ov.Interface(), nv.Interface()) {
			classify(out, fieldPath, effective)
		}
	}
}

func classify(out *SectionChange, path, tag string) {
	if tag == "hot" {
		out.Hot = append(out.Hot, path)
		return
	}
	out.Restart = append(out.Restart, path)
}
