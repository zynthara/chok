package store

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode"

	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/store/internal/q"
)

// Changes is the set of columns an Update writes: from Patch(...).Onto, or
// from Set.
type Changes struct {
	set   map[string]any
	patch *PatchBuilder
}

// Set writes the given fields by public name: any column of the model
// but the managed ones (id, version, the timestamps, the owner, the
// soft-delete columns). It locks on no version unless the Update adds
// where.Version. It is the code's own write; a request DTO goes through
// Patch, which writes only store:"update" fields.
func Set(kv map[string]any) *Changes { return &Changes{set: kv} }

// PatchBuilder is a partial update read from a request: Onto turns it
// into Changes.
type PatchBuilder struct {
	req  any
	onto any
}

// Patch reads a request DTO for a partial update. Its non-nil pointer
// fields that come from the JSON body are the changes, under their JSON
// names; a nil pointer is a field the client did not send. Fields bound
// from the path, the query or a header never take part, and neither does
// a field tagged store:"-". A field with the JSON name "version", an int
// or *int, is not a change: when set it is the version the client last
// saw, and the update fails with VersionConflictError if the row has
// moved on.
//
// Patch panics if req is not a struct or a non-nil pointer to one.
func Patch(req any) *PatchBuilder {
	rv := reflect.ValueOf(req)
	if !rv.IsValid() || (rv.Kind() == reflect.Pointer && rv.IsNil()) {
		panic("store.Patch: needs a request struct, got nil")
	}
	t := rv.Type()
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("store.Patch: needs a request struct or a pointer to one, got %T", req))
	}
	return &PatchBuilder{req: req}
}

// Onto applies the patch to obj, the *T the handler loaded, and locks the
// update on obj's version (or on the request's version field). On success
// obj holds the new values and version. Onto(nil) writes without loading
// and locks only on the request's version field.
func (b *PatchBuilder) Onto(obj any) *Changes {
	b.onto = obj
	return &Changes{patch: b}
}

// IsEmpty reports whether the changes write nothing: a patch whose
// request set no field, or an empty Set.
func (c *Changes) IsEmpty() bool {
	if c == nil {
		return true
	}
	if c.patch == nil {
		return len(c.set) == 0
	}
	plan := patchPlanFor(reflect.TypeOf(c.patch.req))
	if plan.err != nil {
		return false
	}
	rv := derefValue(reflect.ValueOf(c.patch.req))
	for _, f := range plan.fields {
		if fv, ok := fieldByIndex(rv, f.index); ok && !fv.IsNil() {
			return false
		}
	}
	return true
}

// built is what an Update executes.
type built struct {
	columns []string
	payload map[string]any // column → value
	fields  map[string]any // field name → value, what BeforeUpdate sees
	lock    int            // the version to lock on, 0 for none
	model   *db.Model      // the object Onto named, whose Version advances
}

// build resolves the changes: a patch against the update fields, Set
// against every column.
func (c *Changes) build(ctx context.Context, update, columns map[string]string, sch *schema.Schema) (built, error) {
	if c == nil {
		return built{}, &EmptyChangesError{}
	}
	if c.patch == nil {
		return buildSet(c.set, columns, sch)
	}
	return c.patch.build(ctx, update, sch)
}

func buildSet(kv map[string]any, columns map[string]string, sch *schema.Schema) (built, error) {
	if len(kv) == 0 {
		return built{}, &EmptyChangesError{}
	}
	b := built{payload: map[string]any{}, fields: map[string]any{}}
	for name, v := range kv {
		col, err := q.Column(columns, name)
		if err != nil {
			return built{}, fmt.Errorf("store: Set: %s: %w", sch.Name, err)
		}
		if protectedColumns[col] {
			return built{}, fmt.Errorf("store: Set: %q writes the managed column %q", name, col)
		}
		b.columns = append(b.columns, col)
		b.payload[col] = v
		b.fields[name] = v
	}
	slices.Sort(b.columns)
	return b, nil
}

func (p *PatchBuilder) build(ctx context.Context, fields map[string]string, sch *schema.Schema) (built, error) {
	plan := patchPlanFor(reflect.TypeOf(p.req))
	if plan.err != nil {
		return built{}, plan.err
	}
	// Validate the request's whole shape on every build, so a DTO naming a
	// field the store does not update fails on its first request.
	for _, f := range plan.fields {
		col, ok := fields[f.public]
		if !ok {
			return built{}, fmt.Errorf("store: Patch: %q is not an update field of this store (declare it with store:\"update\", or tag the DTO field store:\"-\")", f.public)
		}
		mf := sch.LookUpField(col)
		if mf == nil {
			return built{}, fmt.Errorf("store: Patch: %q maps to %q, which the model lacks", f.public, col)
		}
		if protectedColumns[col] {
			return built{}, fmt.Errorf("store: Patch: %q writes the managed column %q", f.public, col)
		}
		if !assignable(f.elem, mf.FieldType) {
			return built{}, fmt.Errorf("store: Patch: %q is %s, but the model column is %s", f.public, f.elem, mf.FieldType)
		}
	}
	var target reflect.Value
	if p.onto != nil {
		ov := reflect.ValueOf(p.onto)
		if ov.Kind() != reflect.Pointer || ov.IsNil() || ov.Elem().Type() != sch.ModelType {
			return built{}, fmt.Errorf("store: Patch.Onto needs a *%s, got %T", sch.ModelType.Name(), p.onto)
		}
		target = ov
	} else {
		target = reflect.New(sch.ModelType)
	}
	req := derefValue(reflect.ValueOf(p.req))
	b := built{payload: map[string]any{}, fields: map[string]any{}}
	for _, f := range plan.fields {
		fv, ok := fieldByIndex(req, f.index)
		if !ok || fv.IsNil() {
			continue
		}
		col := fields[f.public]
		mf := sch.LookUpField(col)
		dst := mf.ReflectValueOf(ctx, target)
		if err := assign(dst, fv.Elem()); err != nil {
			return built{}, fmt.Errorf("store: Patch: %q: %w", f.public, err)
		}
		value, _ := mf.ValueOf(ctx, target)
		b.columns = append(b.columns, col)
		b.payload[col] = value
		b.fields[f.public] = value
	}
	slices.Sort(b.columns)
	if p.onto != nil {
		b.model = modelOf(target)
		if b.model != nil {
			b.lock = b.model.Version
		}
	}
	if plan.version != nil {
		if v, ok := fieldByIndex(req, plan.version); ok {
			if v.Kind() == reflect.Pointer {
				if !v.IsNil() {
					b.lock = int(v.Elem().Int())
				}
			} else if v.Int() != 0 {
				b.lock = int(v.Int())
			}
		}
	}
	if len(b.columns) == 0 {
		return built{}, &EmptyChangesError{}
	}
	return b, nil
}

// modelOf finds the db.Model inside a model value.
func modelOf(ptr reflect.Value) *db.Model {
	v := ptr.Elem()
	if f := v.FieldByName("Model"); f.IsValid() && f.CanAddr() {
		if m, ok := f.Addr().Interface().(*db.Model); ok {
			return m
		}
	}
	return nil
}

type patchField struct {
	index  []int
	public string
	elem   reflect.Type
}

type patchPlan struct {
	fields  []patchField
	version []int // index of the request's version field, if any
	err     error
}

var patchPlans sync.Map // reflect.Type → *patchPlan

func patchPlanFor(t reflect.Type) *patchPlan {
	t = derefType(t)
	if p, ok := patchPlans.Load(t); ok {
		return p.(*patchPlan)
	}
	p, _ := patchPlans.LoadOrStore(t, buildPatchPlan(t))
	return p.(*patchPlan)
}

// buildPatchPlan follows encoding/json's visibility: embedded structs
// without a JSON name promote their fields, a shallower field shadows a
// deeper one of the same name, and two patchable fields of one name at
// one depth are ambiguous, which is an error.
func buildPatchPlan(rt reflect.Type) *patchPlan {
	type candidate struct {
		field     patchField
		depth     int
		patchable bool
		tagged    bool
	}
	candidates := map[string][]candidate{}
	type frame struct {
		t       reflect.Type
		prefix  []int
		depth   int
		path    []reflect.Type
		tainted bool
	}
	plan := &patchPlan{}
	queue := []frame{{t: rt, path: []reflect.Type{rt}}}
	for len(queue) > 0 {
		fr := queue[0]
		queue = queue[1:]
		for i := range fr.t.NumField() {
			sf := fr.t.Field(i)
			idx := append(slices.Clone(fr.prefix), i)
			if !sf.Anonymous && !sf.IsExported() {
				continue
			}
			var et reflect.Type
			if sf.Anonymous {
				et = derefType(sf.Type)
				if !sf.IsExported() && et.Kind() != reflect.Struct {
					continue
				}
			}
			optedOut := false
			if tag, ok := sf.Tag.Lookup("store"); ok && !fr.tainted {
				if strings.TrimSpace(tag) != "-" {
					return &patchPlan{err: fmt.Errorf("store: Patch: %s.%s has store:%q; on a request only store:\"-\" is allowed", rt.Name(), sf.Name, tag)}
				}
				optedOut = true
			}
			// Path, query and header fields are not body members.
			sourced := false
			for _, tag := range []string{"path", "query", "header"} {
				if v, ok := sf.Tag.Lookup(tag); ok && v != "-" {
					sourced = true
				}
			}
			nonPatchable := fr.tainted || optedOut || sourced
			public, excluded, explicit := jsonName(sf)
			if excluded {
				continue
			}
			if sf.Anonymous && et.Kind() == reflect.Struct {
				if explicit {
					candidates[public] = append(candidates[public], candidate{field: patchField{index: idx, public: public}, depth: fr.depth, tagged: true})
					continue
				}
				if !slices.Contains(fr.path, et) {
					queue = append(queue, frame{t: et, prefix: idx, depth: fr.depth + 1, path: append(slices.Clone(fr.path), et), tainted: nonPatchable})
				}
				continue
			}
			if !sf.IsExported() {
				continue
			}
			if public == "version" && !nonPatchable && isIntOrIntPtr(sf.Type) {
				if plan.version == nil || len(idx) < len(plan.version) {
					plan.version = idx
				}
				continue
			}
			var elem reflect.Type
			isPtr := sf.Type.Kind() == reflect.Pointer
			if isPtr {
				elem = sf.Type.Elem()
			}
			candidates[public] = append(candidates[public], candidate{
				field:     patchField{index: idx, public: public, elem: elem},
				depth:     fr.depth,
				patchable: isPtr && !nonPatchable,
				tagged:    explicit,
			})
		}
	}
	var ambiguous []string
	for name, cs := range candidates {
		minDepth := cs[0].depth
		for _, c := range cs[1:] {
			minDepth = min(minDepth, c.depth)
		}
		var atMin []candidate
		patchable := 0
		for _, c := range cs {
			if c.depth == minDepth {
				atMin = append(atMin, c)
				if c.patchable {
					patchable++
				}
			}
		}
		if patchable >= 2 {
			ambiguous = append(ambiguous, name)
			continue
		}
		var winner candidate
		found, tagged := false, 0
		for _, c := range atMin {
			if c.tagged {
				tagged++
				winner, found = c, true
			}
		}
		if tagged != 1 {
			winner, found = atMin[0], len(atMin) == 1
		}
		if found && winner.patchable {
			plan.fields = append(plan.fields, winner.field)
		}
	}
	if len(ambiguous) > 0 {
		slices.Sort(ambiguous)
		return &patchPlan{err: fmt.Errorf("store: Patch: %s has several patchable fields named %q at the same depth", rt.Name(), ambiguous[0])}
	}
	if len(plan.fields) == 0 {
		// Reporting it as empty would make the update a silent no-op.
		return &patchPlan{err: fmt.Errorf("store: Patch: %s has no patchable field: a pointer field with a JSON name, from the body", rt.Name())}
	}
	slices.SortFunc(plan.fields, func(a, b patchField) int { return strings.Compare(a.public, b.public) })
	return plan
}

func isIntOrIntPtr(t reflect.Type) bool {
	t = derefType(t)
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

// jsonName is the field's JSON name, as encoding/json reads the tag.
func jsonName(sf reflect.StructField) (name string, excluded, explicit bool) {
	tag, ok := sf.Tag.Lookup("json")
	if !ok {
		return sf.Name, false, false
	}
	if tag == "-" {
		return "", true, false
	}
	first, _, _ := strings.Cut(tag, ",")
	if first == "" || !validJSONName(first) {
		return sf.Name, false, false
	}
	return first, false, true
}

func validJSONName(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c) && !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			return false
		}
	}
	return true
}

func assignable(f, m reflect.Type) bool {
	return f != nil && (f.AssignableTo(m) || (m.Kind() == reflect.Pointer && f.AssignableTo(m.Elem())))
}

func assign(dst, v reflect.Value) error {
	mt := dst.Type()
	if v.Type().AssignableTo(mt) {
		dst.Set(v)
		return nil
	}
	if mt.Kind() == reflect.Pointer && v.Type().AssignableTo(mt.Elem()) {
		p := reflect.New(mt.Elem())
		p.Elem().Set(v)
		dst.Set(p)
		return nil
	}
	return fmt.Errorf("a %s cannot be written to a %s column", v.Type(), mt)
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func derefValue(v reflect.Value) reflect.Value {
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	return v
}

func fieldByIndex(v reflect.Value, index []int) (reflect.Value, bool) {
	for i, x := range index {
		if i > 0 {
			for v.Kind() == reflect.Pointer {
				if v.IsNil() {
					return reflect.Value{}, false
				}
				v = v.Elem()
			}
		}
		v = v.Field(x)
	}
	return v, true
}
