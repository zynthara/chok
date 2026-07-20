package store

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"gorm.io/gorm/schema"
)

// Patch returns a Changes that derives its update set from the non-nil
// pointer fields of a request DTO — the "cast" half of an Ecto changeset,
// adapted to Go's pointer-as-optional idiom. It is the third Changes
// constructor alongside Set and Fields, and the intended front door for
// partial updates (HTTP PATCH):
//
//	type updatePostReq struct {
//	    Title  *string `json:"title"`
//	    Status *string `json:"status"`
//	}
//	// nil field = client omitted it (skipped); non-nil = write it, zero
//	// values included (*"" clears the column).
//	p, _ := posts.Get(ctx, store.RID(rid))
//	pc := store.Patch(req).Onto(p)   // apply onto p, carry p.Version as the lock
//	if pc.IsEmpty() {                // client sent {} → nothing to do
//	    return p, nil
//	}
//	err := posts.Update(ctx, store.RID(rid), pc)
//
// Participation rules (mirroring encoding/json field visibility):
//
//   - Only pointer fields participate. Non-pointer fields (uri params,
//     an int version, control flags) are silently ignored — they cannot
//     express "absent", and ignoring them fails safe (a forgotten `*`
//     means "field never updates", visible in dev; the alternative,
//     "non-pointer always written", would silently clobber columns to
//     their zero value).
//   - The public field name is the json tag's first segment; json:"-"
//     excludes the field; a field with no json tag participates under its
//     Go field name (so a DTO relying on default naming is not silently
//     dropped — a mismatch surfaces loudly as ErrUnknownUpdateField).
//   - A DTO field may opt out with `store:"-"` (anonymous embeds included,
//     so an embedded control container is not promoted). Any other store
//     tag value on a request DTO is a construction-time error (the
//     model-side query/update tag vocabulary does not apply to DTOs).
//   - Embedded structs promote their fields per json rules: an embed with
//     no json name promotes its fields (shallower shadows deeper), while a
//     named embed (json:"meta") is a normal nested field, not promoted.
//     Two fields resolving to the same public name at the same depth are
//     ambiguous and rejected (stricter than encoding/json's tag-dominance
//     tie-break — a DTO name clash is a build-time error, not a silent
//     pick).
//
// Values are validated against the Store's update whitelist and model
// schema on every build (the FULL declared shape, even fields nil this
// call), so a DTO that names an unknown, protected or type-incompatible
// column fails on the first request that reaches Update rather than lying
// in wait until a client first sends that field. (IsEmpty short-circuits a
// no-op PATCH before build, so an all-nil first request does not trip that
// validation — the guarantee is "the first request that builds".)
// Whitelist/protected/type failures are programming errors → 500; an
// all-nil call is ErrEmptyPatch → 400.
//
// Patch panics if req is not a struct or a non-nil pointer-to-struct — a
// wiring error caught at the call site, like WithRowsAffected(nil).
func Patch(req any) *PatchChanges {
	rv := reflect.ValueOf(req)
	if !rv.IsValid() {
		panic("store: Patch requires a struct or pointer-to-struct request, got nil")
	}
	// Reject a typed nil pointer at construction rather than panicking deep
	// in reflection at build time.
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		panic(fmt.Sprintf("store: Patch requires a non-nil request, got nil %T", req))
	}
	rt := rv.Type()
	for rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct {
		panic(fmt.Sprintf("store: Patch requires a struct or pointer-to-struct request, got %T", req))
	}
	return &PatchChanges{req: req}
}

// PatchChanges is the concrete return of Patch so that Onto / NoLock /
// IsEmpty remain available. It satisfies the Changes interface.
type PatchChanges struct {
	req     any
	onto    any  // the model passed to Onto (may be a nil interface)
	ontoSet bool // whether Onto was called at all — distinct from onto == nil
	noLock  bool // set by NoLock; only meaningful together with Onto
}

var _ Changes = (*PatchChanges)(nil)

// Onto applies the patch onto an already-loaded model object (the Store's
// concrete *T) and, unless NoLock is called, carries obj.Version as the
// optimistic-lock guard — exactly like Fields(&obj). The non-nil patch
// values are written onto obj before the update, and on success obj.Version
// advances. On error, obj holds the applied values with an un-advanced
// Version: discard or reload it rather than treating it as the truth.
//
// Calling Onto with a nil model is an error surfaced at build (a mistyped
// intent, not a silent downgrade to a bare write). Without Onto at all,
// Patch is a bare change set (no implicit lock, obj not updated) — pair
// with WithVersion when locking matters, mirroring Set.
func (p *PatchChanges) Onto(obj any) *PatchChanges {
	p.onto = obj
	p.ontoSet = true
	return p
}

// NoLock disables the implicit optimistic lock that Onto would otherwise
// derive from obj.Version. It has no effect on a bare Patch (which never
// locks). Semantics match FieldChanges.NoLock.
func (p *PatchChanges) NoLock() *PatchChanges {
	p.noLock = true
	return p
}

// IsEmpty reports whether this call carries no field to write — i.e. every
// participating pointer field is nil. It touches neither the database nor
// the Store's schema, so handlers can short-circuit a no-op PATCH ({}) to
// return the current object without a write.
//
// A DTO with a structural problem (illegal store tag, ambiguous promotion)
// or no patchable field at all reports NOT empty: those are surfaced as
// errors by the subsequent Update, and IsEmpty must not swallow them into
// a silent no-op.
func (p *PatchChanges) IsEmpty() bool {
	plan := patchPlanFor(reflect.TypeOf(p.req))
	if plan.err != nil || len(plan.fields) == 0 {
		return false
	}
	rv := derefStruct(reflect.ValueOf(p.req))
	for _, f := range plan.fields {
		fv, ok := fieldByIndexSafe(rv, f.index)
		if ok && !fv.IsNil() {
			return false
		}
	}
	return true
}

func (p *PatchChanges) build(ctx context.Context, fm map[string]string, modelSchema *schema.Schema) (builtChanges, error) {
	plan := patchPlanFor(reflect.TypeOf(p.req))
	if plan.err != nil {
		return builtChanges{}, plan.err
	}
	if len(plan.fields) == 0 {
		return builtChanges{}, fmt.Errorf("%w: %T", ErrNoPatchableFields, p.req)
	}
	if fm == nil {
		return builtChanges{}, ErrUpdateFieldsNotConfigured
	}
	if modelSchema == nil {
		return builtChanges{}, fmt.Errorf("store: Patch: model schema is unavailable")
	}

	// Validate the full declared shape first, including fields nil this
	// call: name in whitelist, not a protected column, type assignable to
	// the model field. A bad DTO fails on the first request that builds.
	for _, f := range plan.fields {
		col, ok := fm[f.public]
		if !ok {
			return builtChanges{}, fmt.Errorf("%w: %q", ErrUnknownUpdateField, f.public)
		}
		if isProtectedUpdateColumn(modelSchema, col) {
			return builtChanges{}, fmt.Errorf("%w: %q resolves to %q", ErrProtectedUpdateField, f.public, col)
		}
		mf := modelSchema.LookUpField(col)
		if mf == nil {
			return builtChanges{}, fmt.Errorf("store: Patch: field %q resolves to unknown model column %q", f.public, col)
		}
		if !patchAssignable(f.elemType, mf.FieldType) {
			return builtChanges{}, fmt.Errorf("store: Patch: field %q of type %s is not assignable to model column %q of type %s", f.public, f.elemType, col, mf.FieldType)
		}
	}

	// Resolve the model instance the values are applied onto: the caller's
	// object (Onto) or a throwaway of the Store's model type (bare Patch).
	// Both then delegate to Fields, so payload building, value coercion (a
	// driver.Valuer implemented on *E, serializers, GORM's zero handling),
	// the event snapshot and — for Onto — the optimistic lock and version
	// write-back all run through exactly one code path. Building the bare
	// payload by hand instead would skip that coercion and, for a nullable
	// *E column, persist a differently-encoded value than Onto.
	var target reflect.Value
	if p.ontoSet {
		ov := reflect.ValueOf(p.onto)
		if ov.Kind() != reflect.Pointer || ov.IsNil() || ov.Elem().Type() != modelSchema.ModelType {
			return builtChanges{}, fmt.Errorf("store: Patch.Onto object type %T does not match Store model %s", p.onto, modelSchema.ModelType)
		}
		target = ov
	} else {
		target = reflect.New(modelSchema.ModelType)
	}

	reqVal := derefStruct(reflect.ValueOf(p.req))
	names := make([]string, 0, len(plan.fields))
	for _, f := range plan.fields {
		fv, ok := fieldByIndexSafe(reqVal, f.index)
		if !ok || fv.IsNil() {
			continue
		}
		dst := modelSchema.LookUpField(fm[f.public]).ReflectValueOf(ctx, target)
		if err := patchApply(dst, fv.Elem()); err != nil {
			return builtChanges{}, fmt.Errorf("store: Patch: field %q: %w", f.public, err)
		}
		names = append(names, f.public)
	}
	if len(names) == 0 {
		return builtChanges{}, ErrEmptyPatch
	}

	fc := Fields(target.Interface(), names...)
	if !p.ontoSet || p.noLock {
		fc.NoLock()
	}
	built, err := fc.build(ctx, fm, modelSchema)
	if err != nil {
		return builtChanges{}, err
	}
	if !p.ontoSet {
		// Throwaway instance: never write a version back onto it, and carry
		// no implicit lock (a bare Patch locks only via WithVersion).
		built.model = nil
		built.implicitVersion = 0
	}
	return built, nil
}

// patchField is one participating pointer field of a request DTO, resolved
// once per DTO type and cached.
type patchField struct {
	index    []int        // field index path (FieldByIndex), embeds included
	public   string       // public field name the update whitelist keys on
	elemType reflect.Type // the pointer's element type (the value written)
}

// patchPlan is the cached, schema-independent analysis of a DTO type:
// which pointer fields participate and under what public names. err holds
// a structural defect (illegal store tag, ambiguous promotion) surfaced at
// build time as a 500.
type patchPlan struct {
	fields []patchField
	err    error
}

var patchPlanCache sync.Map // reflect.Type (struct) -> *patchPlan

func patchPlanFor(rt reflect.Type) *patchPlan {
	for rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	if cached, ok := patchPlanCache.Load(rt); ok {
		return cached.(*patchPlan)
	}
	plan := buildPatchPlan(rt)
	actual, _ := patchPlanCache.LoadOrStore(rt, plan)
	return actual.(*patchPlan)
}

// buildPatchPlan analyses a DTO struct type. It walks the struct breadth-
// first so shallower embeds shadow deeper ones (Go/json promotion rules);
// two participating fields resolving to the same public name at the same
// depth are ambiguous and rejected. Cycle detection is path-based (a type
// already on the current embed path is not re-entered), so a genuine
// diamond — the same field reachable by two equal-depth paths — is reported
// as ambiguous rather than silently deduplicated to the first path.
func buildPatchPlan(rt reflect.Type) *patchPlan {
	type candidate struct {
		field patchField
		depth int
	}
	chosen := map[string]*candidate{}
	ambiguous := map[string]struct{}{}

	type frame struct {
		t      reflect.Type
		prefix []int
		depth  int
		path   []reflect.Type // types from root to here, for cycle detection
	}
	queue := []frame{{t: rt, depth: 0, path: []reflect.Type{rt}}}

	onPath := func(path []reflect.Type, t reflect.Type) bool {
		for _, p := range path {
			if p == t {
				return true
			}
		}
		return false
	}

	for len(queue) > 0 {
		fr := queue[0]
		queue = queue[1:]
		for i := 0; i < fr.t.NumField(); i++ {
			sf := fr.t.Field(i)
			idx := make([]int, 0, len(fr.prefix)+1)
			idx = append(idx, fr.prefix...)
			idx = append(idx, i)

			// Unexported non-anonymous fields never participate. An
			// unexported ANONYMOUS field can still promote its exported
			// fields (Go/encoding/json rule), so it is not skipped here.
			if !sf.Anonymous && !sf.IsExported() {
				continue
			}

			// store:"-" opt-out and illegal store tags apply to every field
			// — anonymous containers included, checked BEFORE the embed is
			// walked so a tagged-out container is not promoted.
			if stag, ok := sf.Tag.Lookup("store"); ok {
				if strings.TrimSpace(stag) == "-" {
					continue
				}
				return &patchPlan{err: fmt.Errorf("store: Patch: field %q of %s has store tag %q; only %q is allowed on a request DTO", sf.Name, rt, stag, "-")}
			}

			if sf.Anonymous {
				// json:"-" excludes the whole embed.
				if isJSONExcluded(sf) {
					continue
				}
				et := sf.Type
				for et.Kind() == reflect.Pointer {
					et = et.Elem()
				}
				// A struct embed with no explicit json name promotes its
				// fields. A named embed (json:"meta") or a non-struct embed
				// falls through to be treated as a normal field.
				if _, named := explicitJSONName(sf); et.Kind() == reflect.Struct && !named {
					if !onPath(fr.path, et) {
						next := append(append([]reflect.Type(nil), fr.path...), et)
						queue = append(queue, frame{t: et, prefix: idx, depth: fr.depth + 1, path: next})
					}
					continue
				}
			}

			if sf.Type.Kind() != reflect.Pointer {
				continue
			}
			public, skip := patchPublicName(sf)
			if skip {
				continue
			}
			cand := &candidate{
				field: patchField{index: idx, public: public, elemType: sf.Type.Elem()},
				depth: fr.depth,
			}
			switch existing, ok := chosen[public]; {
			case !ok:
				chosen[public] = cand
			case fr.depth < existing.depth:
				chosen[public] = cand
				delete(ambiguous, public)
			case fr.depth == existing.depth:
				ambiguous[public] = struct{}{}
			}
		}
	}

	if len(ambiguous) > 0 {
		names := make([]string, 0, len(ambiguous))
		for n := range ambiguous {
			names = append(names, n)
		}
		sort.Strings(names)
		return &patchPlan{err: fmt.Errorf("store: Patch: %s promotes multiple fields to the same public name %q at the same depth", rt, names[0])}
	}

	fields := make([]patchField, 0, len(chosen))
	for _, c := range chosen {
		fields = append(fields, c.field)
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].public < fields[j].public })
	return &patchPlan{fields: fields}
}

// explicitJSONName returns the json tag's first segment when it names the
// field (non-empty, not "-"). json:",omitempty" and json:"-" both return
// ("", false): the former keeps the Go name, the latter is handled as an
// exclusion by the caller.
func explicitJSONName(sf reflect.StructField) (string, bool) {
	tag, ok := sf.Tag.Lookup("json")
	if !ok {
		return "", false
	}
	first := tag
	if c := strings.IndexByte(tag, ','); c >= 0 {
		first = tag[:c]
	}
	if first == "" || first == "-" {
		return "", false
	}
	return first, true
}

// isJSONExcluded reports whether the field is excluded by json:"-" (the
// exact tag "-"; json:"-," is the literal name "-", not an exclusion).
func isJSONExcluded(sf reflect.StructField) bool {
	tag, ok := sf.Tag.Lookup("json")
	return ok && tag == "-"
}

// patchPublicName resolves a struct field's public name the way
// encoding/json would. skip is true when the field is excluded (json:"-").
func patchPublicName(sf reflect.StructField) (name string, skip bool) {
	tag, ok := sf.Tag.Lookup("json")
	if !ok {
		return sf.Name, false
	}
	first := tag
	if comma := strings.IndexByte(tag, ','); comma >= 0 {
		first = tag[:comma]
	}
	switch {
	case tag == "-":
		return "", true // json:"-" excludes
	case first == "-":
		return "-", false // json:"-,..." is the literal name "-"
	case first == "":
		return sf.Name, false // json:",omitempty" keeps the Go field name
	default:
		return first, false
	}
}

// patchAssignable reports whether a DTO pointer element of type f can drive
// a model field of type m: directly assignable, or (nullable column) m is
// *E and f assignable to E. Deliberately not ConvertibleTo — an int→string
// "conversion" is a semantic trap, not a patch.
func patchAssignable(f, m reflect.Type) bool {
	if f.AssignableTo(m) {
		return true
	}
	return m.Kind() == reflect.Pointer && f.AssignableTo(m.Elem())
}

// patchApply writes elem onto the model field dst under the same rule
// patchAssignable gates: direct set, or take the address for a nullable
// (*E) column. reflect (not gorm's Field.Set) so the strict assignability
// rule holds rather than gorm's lenient coercion.
func patchApply(dst, elem reflect.Value) error {
	mt := dst.Type()
	if elem.Type().AssignableTo(mt) {
		dst.Set(elem)
		return nil
	}
	if mt.Kind() == reflect.Pointer && elem.Type().AssignableTo(mt.Elem()) {
		ptr := reflect.New(mt.Elem())
		ptr.Elem().Set(elem)
		dst.Set(ptr)
		return nil
	}
	return fmt.Errorf("value of type %s not assignable to %s", elem.Type(), mt)
}

// derefStruct follows pointers to the underlying struct value (read-only
// use: the returned value need not be addressable).
func derefStruct(v reflect.Value) reflect.Value {
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	return v
}

// fieldByIndexSafe is reflect.Value.FieldByIndex without the panic on a nil
// embedded pointer along the path: an unreachable field reads as absent
// (ok=false), which the caller treats as a nil patch field (skipped).
func fieldByIndexSafe(v reflect.Value, index []int) (reflect.Value, bool) {
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
