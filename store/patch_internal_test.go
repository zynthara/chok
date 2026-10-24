package store

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
)

func planNames(t *testing.T, v any) []string {
	t.Helper()
	plan := buildPatchPlan(reflect.TypeOf(v))
	if plan.err != nil {
		t.Fatalf("plan of %T: %v", v, plan.err)
	}
	var names []string
	for _, f := range plan.fields {
		names = append(names, f.public)
	}
	slices.Sort(names)
	return names
}

func planErr(v any) error { return buildPatchPlan(reflect.TypeOf(v)).err }

// The plan follows encoding/json's visibility rules, so a request decodes
// and patches the same fields.
func TestPatchPlan_Visibility(t *testing.T) {
	type deepTitle struct {
		Title *string `json:"Title"`
	}
	type shallowShadows struct {
		deepTitle
		Title *string
	}
	plan := buildPatchPlan(reflect.TypeFor[shallowShadows]())
	if plan.err != nil || len(plan.fields) != 1 || !slices.Equal(plan.fields[0].index, []int{1}) {
		t.Fatalf("the shallow field shadows the deep one: %+v", plan)
	}

	type left struct {
		Name *string `json:"Name"`
	}
	type right struct{ Name *string }
	type ambiguous struct {
		left
		right
	}
	if planErr(ambiguous{}) == nil {
		t.Error("two patchable fields of one name at one depth are ambiguous")
	}
	type leaf struct{ X *string }
	type midA struct{ leaf }
	type midB struct{ leaf }
	type diamond struct {
		midA
		midB
	}
	if planErr(diamond{}) == nil {
		t.Error("a diamond is ambiguous: the cycle guard is per path")
	}
	type cyc struct {
		*cyc
		Title *string `json:"title"`
	}
	if got := planNames(t, cyc{cyc: &cyc{}}); !slices.Equal(got, []string{"title"}) {
		t.Errorf("a self embed terminates: %v", got)
	}
}

func TestPatchPlan_ShadowingAndOptOut(t *testing.T) {
	type deepMeta struct {
		Meta *string `json:"meta"`
	}
	type nested struct {
		X *string `json:"x"`
	}
	type namedShadows struct {
		deepMeta
		nested `json:"meta"`
		Body   *string `json:"body"`
	}
	type deepName struct {
		Name *string `json:"name"`
	}
	type leafOptOut struct {
		deepName
		Name *string `json:"name" store:"-"`
		Body *string `json:"body"`
	}
	type box struct{ deepName }
	type shallow struct {
		Name *string `json:"name"`
	}
	type containerOptOut struct {
		box
		shallow `store:"-"`
		Body    *string `json:"body"`
	}
	type a struct{ X *string }
	type b struct {
		X *string `store:"-"`
	}
	type untaggedTie struct {
		a
		b
		Body *string `json:"body"`
	}
	type taggedOptOut struct {
		X *string `json:"X" store:"-"`
	}
	type taggedOptOutWins struct {
		a
		taggedOptOut
		Body *string `json:"body"`
	}
	type taggedValue struct {
		X string `json:"X"`
	}
	type taggedValueWins struct {
		a
		taggedValue
		Body *string `json:"body"`
	}
	type deepInt struct {
		Title *int `json:"Title"`
	}
	type valueShadows struct {
		deepInt
		Title string
		Body  *string `json:"body"`
	}
	for _, v := range []any{namedShadows{}, leafOptOut{}, containerOptOut{}, untaggedTie{}, taggedOptOutWins{}, taggedValueWins{}, valueShadows{}} {
		if got := planNames(t, v); !slices.Equal(got, []string{"body"}) {
			t.Errorf("%T: plan %v, want [body]", v, got)
		}
	}
	type modelish struct {
		Secret *string `json:"secret" store:"update"`
	}
	type optedContainer struct {
		modelish `store:"-"`
		Title    *string `json:"title"`
	}
	if got := planNames(t, optedContainer{}); !slices.Equal(got, []string{"title"}) {
		t.Errorf("an opted-out container ignores the store tags inside: %v", got)
	}
	type ctrlA struct{ Flag *string }
	type ctrlB struct{ Flag *string }
	type twoOpted struct {
		ctrlA `store:"-"`
		ctrlB `store:"-"`
		Title *string `json:"title"`
	}
	if got := planNames(t, twoOpted{}); !slices.Equal(got, []string{"title"}) {
		t.Errorf("a name only opted-out fields share drops quietly: %v", got)
	}
	// Tagged and untagged contenders for one name: the tagged one wins,
	// patchable or not.
	type taggedPatchable struct {
		X *string `json:"X"`
	}
	type untaggedOpted struct {
		X *string `store:"-"`
	}
	type taggedWins struct {
		taggedPatchable
		untaggedOpted
	}
	if got := planNames(t, taggedWins{}); !slices.Equal(got, []string{"X"}) {
		t.Errorf("the tagged patchable field wins: %v", got)
	}
	// json:"-,omitempty" names a field "-" rather than excluding it, so
	// an embed so tagged is a nested object, as a named field so tagged
	// is. The types are built at run time: vet rightly flags the tag in
	// source.
	meta := reflect.TypeFor[struct {
		Title *string `json:"title"`
	}]()
	body := reflect.StructField{Name: "Body", Type: reflect.TypeFor[*string](), Tag: `json:"body"`}
	for _, anonymous := range []bool{false, true} {
		if anonymous {
			meta = reflect.TypeFor[Meta]()
		}
		rt := reflect.StructOf([]reflect.StructField{{Name: "Meta", Type: meta, Tag: `json:"-,omitempty"`, Anonymous: anonymous}, body})
		plan := buildPatchPlan(rt)
		if plan.err != nil || len(plan.fields) != 1 || plan.fields[0].public != "body" {
			t.Errorf("anonymous %v: plan %+v", anonymous, plan)
		}
	}
}

// Meta is the embed of the run-time dash-name case; two fields keep it
// from being pointer-shaped, which reflect.StructOf cannot embed.
type Meta struct {
	Title *string `json:"title"`
	Note  *string `json:"note"`
}

func TestJSONName_MirrorsEncodingJSON(t *testing.T) {
	for _, c := range []struct {
		tag              string
		name             string
		excluded, tagged bool
	}{
		{`json:"a\"b"`, "Extra", false, false},
		{`json:"-,omitempty"`, "-", false, true},
		{`json:"-"`, "", true, false},
		{`json:",omitempty"`, "Extra", false, false},
		{`json:"x_y"`, "x_y", false, true},
	} {
		name, excluded, tagged := jsonName(reflect.StructField{Name: "Extra", Tag: reflect.StructTag(c.tag)})
		if name != c.name || excluded != c.excluded || tagged != c.tagged {
			t.Errorf("%s → (%q, %v, %v)", c.tag, name, excluded, tagged)
		}
	}
}

// TestModelOf_LivePointer: the model base Onto advances is the object's
// own, through every base.
func TestModelOf_LivePointer(t *testing.T) {
	type plain struct{ db.Model }
	type soft struct{ db.SoftDeleteModel }
	type owned struct{ db.OwnedSoftDeleteModel }
	for _, v := range []any{&plain{}, &soft{}, &owned{}} {
		m := modelOf(reflect.ValueOf(v))
		if m == nil {
			t.Fatalf("%T: no model", v)
		}
		m.Version = 42
		if got := reflect.ValueOf(v).Elem().FieldByName("Version").Int(); got != 42 {
			t.Fatalf("%T: a copy, not the object's base", v)
		}
	}
	if modelOf(reflect.ValueOf(&struct{ Name string }{})) != nil {
		t.Fatal("a struct without a base has no model")
	}
}

// TestChanges_DefendManagedColumns: New refuses an update field on a
// managed column, and building the changes refuses it again, so a store
// whose field map went wrong still cannot write the version or the owner.
func TestChanges_DefendManagedColumns(t *testing.T) {
	type row struct {
		db.OwnedModel
		Title string `json:"title" store:"query,update" gorm:"size:20;not null"`
	}
	d := dbtest.Open(t, db.Tables(db.Table(&row{})))
	s := New[row](d)
	s.updateFields["title"] = "version" // as a regression might
	if _, err := Set(map[string]any{"title": 9}).build(t.Context(), s.updateFields, s.schema); err == nil || !strings.Contains(err.Error(), "managed column") {
		t.Fatalf("Set: %v", err)
	}
	s.updateFields["title"] = "owner_id"
	req := &struct {
		Title *string `json:"title"`
	}{new("mallory")}
	if _, err := Patch(req).Onto(nil).build(t.Context(), s.updateFields, s.schema); err == nil || !strings.Contains(err.Error(), "managed column") {
		t.Fatalf("Patch: %v", err)
	}
}
