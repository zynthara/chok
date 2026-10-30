package store_test

import (
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

type patchKind string

// patchable has a plain column, a nullable one, a defined-type one and one
// whose JSON name is its Go name.
type patchable struct {
	db.SoftDeleteModel
	Title string    `json:"title" store:"query,update" gorm:"size:200;not null;default:''"`
	Body  *string   `json:"body" store:"update" gorm:"size:200"`
	Kind  patchKind `json:"kind" store:"update" gorm:"size:20;not null;default:''"`
	Extra string    `json:"Extra" store:"update" gorm:"size:20;not null;default:''"`
	Enc   *patchEnc `json:"enc" store:"update" gorm:"size:100"`
}

// patchEnc is a Valuer on its pointer only.
type patchEnc struct{ raw string }

func (e *patchEnc) Value() (driver.Value, error) {
	if e == nil {
		return nil, nil
	}
	return "enc:" + e.raw, nil
}

func (e *patchEnc) Scan(src any) error {
	switch v := src.(type) {
	case string:
		e.raw = strings.TrimPrefix(v, "enc:")
	case []byte:
		e.raw = strings.TrimPrefix(string(v), "enc:")
	}
	return nil
}

type patchReq struct {
	ID    string     `path:"id"`
	Title *string    `json:"title"`
	Body  *string    `json:"body"`
	Kind  *patchKind `json:"kind"`
}

func patchStore(t *testing.T, opts ...store.Option) (*store.Store[patchable], *patchable) {
	t.Helper()
	d := dbtest.Open(t, db.Tables(db.Table(&patchable{})))
	s := store.New[patchable](d, opts...)
	p := &patchable{Title: "orig", Kind: "draft", Extra: "x"}
	mustCreate(t, t.Context(), s, p)
	return s, p
}

func TestPatch_PanicsOnNonStructs(t *testing.T) {
	var typedNil *patchReq
	inner := &patchReq{}
	for _, bad := range []any{nil, "str", 42, new(7), typedNil, &inner} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Patch(%T) does not panic", bad)
				}
			}()
			store.Patch(bad)
		}()
	}
}

func TestPatch_IsEmpty(t *testing.T) {
	if !store.Patch(&patchReq{}).Onto(nil).IsEmpty() || !store.Patch(patchReq{}).Onto(nil).IsEmpty() {
		t.Error("an all-nil patch is empty, by pointer or by value")
	}
	if store.Patch(&patchReq{Title: new("x")}).Onto(nil).IsEmpty() {
		t.Error("a set field is a change")
	}
	type noPointer struct {
		Title string `json:"title"`
	}
	if store.Patch(&noPointer{}).Onto(nil).IsEmpty() {
		t.Error("a DTO without patchable fields is a mistake, not an empty patch")
	}
	type unknownButEmpty struct {
		Bogus *string `json:"bogus"`
	}
	if !store.Patch(&unknownButEmpty{}).Onto(nil).IsEmpty() {
		t.Error("IsEmpty does not validate fields against the store")
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if store.Patch(&patchReq{Title: new("x")}).Onto(nil).IsEmpty() {
				t.Error("concurrent plans disagree")
			}
		})
	}
	wg.Wait()
}

func TestPatch_OntoAppliesAndAdvances(t *testing.T) {
	s, p := patchStore(t)
	ctx := t.Context()
	created := p.UpdatedAt
	time.Sleep(5 * time.Millisecond)
	ch := store.Patch(&patchReq{ID: p.RID, Title: new("changed"), Kind: new(patchKind("published"))}).Onto(p)
	if err := s.Update(ctx, store.RID(p.RID), ch); err != nil {
		t.Fatal(err)
	}
	if p.Title != "changed" || p.Kind != "published" || p.Version != 2 || !p.UpdatedAt.After(created) {
		t.Fatalf("Onto's object: %+v", p)
	}
	got, _ := s.Get(ctx, store.RID(p.RID))
	if got.Title != "changed" || got.Kind != "published" || got.Version != 2 || got.Extra != "x" {
		t.Fatalf("stored %+v", got)
	}
	// The object is what was stored, to the column's precision.
	if d := got.UpdatedAt.Sub(p.UpdatedAt); d < -time.Millisecond || d > time.Millisecond {
		t.Fatalf("updated_at %v in the object, %v stored", p.UpdatedAt, got.UpdatedAt)
	}
}

func TestPatch_ZeroValuesAreWritten(t *testing.T) {
	s, p := patchStore(t)
	ctx := t.Context()
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&patchReq{Body: new("bio"), Title: new("")}).Onto(p)); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&patchReq{Body: new("")}).Onto(p)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, store.RID(p.RID))
	if got.Body == nil || *got.Body != "" || got.Title != "" {
		t.Fatalf("zero values persist: %+v", got)
	}
}

func TestPatch_VersionLock(t *testing.T) {
	s, p := patchStore(t)
	ctx := t.Context()
	other, _ := s.Get(ctx, store.RID(p.RID))
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&patchReq{Title: new("bumped")}).Onto(other)); err != nil {
		t.Fatal(err)
	}
	// p is stale now.
	err := s.Update(ctx, store.RID(p.RID), store.Patch(&patchReq{Title: new("mine")}).Onto(p))
	if !errors.As(err, new(*store.VersionConflictError)) {
		t.Fatalf("stale Onto: %v", err)
	}
	if p.Version != 1 {
		t.Fatalf("a failed write does not advance the object: %+v", p)
	}
	// where.Version overrides the object's version.
	p.Version = 999
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&patchReq{Title: new("v")}).Onto(p), where.Version(2)); err != nil {
		t.Fatalf("where.Version wins: %v", err)
	}
	if p.Version != 3 {
		t.Fatalf("advanced from the lock used: %d", p.Version)
	}
	// Onto(nil) locks only on where.Version or the request's version.
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&patchReq{Title: new("bare")}).Onto(nil)); err != nil {
		t.Fatalf("bare patch: %v", err)
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&patchReq{Title: new("x")}).Onto(nil), where.Version(50)); !errors.As(err, new(*store.VersionConflictError)) {
		t.Fatalf("bare patch, stale where.Version: %v", err)
	}
	if err := s.Update(ctx, store.RID("pst_nope"), store.Patch(&patchReq{Title: new("x")}).Onto(nil)); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("missing row: %v", err)
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&patchReq{Title: new("x")}).Onto((*patchable)(nil))); err == nil {
		t.Fatal("Onto of a typed nil is an error")
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&patchReq{Title: new("x")}).Onto(&post{})); err == nil {
		t.Fatal("Onto of another model is an error")
	}
}

// TestPatch_RequestVersion: a json "version" field is the lock, never a
// change, and coexists with path fields.
func TestPatch_RequestVersion(t *testing.T) {
	s, p := patchStore(t)
	ctx := t.Context()
	type req struct {
		ID      string  `path:"id"`
		Version *int    `json:"version"`
		Title   *string `json:"title"`
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&req{ID: p.RID, Version: new(1), Title: new("a")}).Onto(p)); err != nil {
		t.Fatal(err)
	}
	stale := &req{ID: p.RID, Version: new(1), Title: new("b")}
	// The request's version beats the object's.
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(stale).Onto(p)); !errors.As(err, new(*store.VersionConflictError)) {
		t.Fatalf("stale request version: %v", err)
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&req{Version: new(2), Title: new("c")}).Onto(nil)); err != nil {
		t.Fatalf("request version without Onto: %v", err)
	}
	type plain struct {
		Version int     `json:"version"`
		Title   *string `json:"title"`
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&plain{Version: 1, Title: new("d")}).Onto(nil)); !errors.As(err, new(*store.VersionConflictError)) {
		t.Fatalf("an int version locks when non-zero: %v", err)
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&plain{Title: new("d")}).Onto(nil)); err != nil {
		t.Fatalf("a zero int version does not lock: %v", err)
	}
}

func TestPatch_FieldParticipation(t *testing.T) {
	s, p := patchStore(t)
	ctx := t.Context()
	type ctrl struct {
		Force *bool `json:"force"`
	}
	type inner struct {
		X *string `json:"x"`
	}
	type myScalar int
	type mixed struct {
		ctrl      `store:"-"` // an opted-out embed does not promote
		inner     `json:"meta"`
		*myScalar        // unexported scalar embed: invisible to JSON
		Ignored   string `json:"ignored"` // not a pointer
		hidden    *string
		Excluded  *string `json:"-"`
		Opted     *bool   `json:"opted" store:"-"`
		Query     *string `query:"q"`
		Header    *string `header:"X-Thing"`
		Title     *string `json:"title"`
		Extra     *string // no tag: the Go name
	}
	m := &mixed{Force: new(true), X: new("no"), myScalar: new(myScalar(1)), Ignored: "no",
		hidden: new("no"), Excluded: new("no"), Opted: new(true), Query: new("no"), Header: new("no"), Title: new("t"), Extra: new("y")}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(m).Onto(p)); err != nil {
		t.Fatalf("only title and Extra take part: %v", err)
	}
	if p.Title != "t" || p.Extra != "y" {
		t.Fatalf("applied %+v", p)
	}
	type embedBase struct {
		Body *string `json:"body"`
	}
	type promoted struct {
		embedBase
		Title *string `json:"title"`
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&promoted{embedBase{new("deep")}, new("t2")}).Onto(p)); err != nil {
		t.Fatal(err)
	}
	if p.Body == nil || *p.Body != "deep" {
		t.Fatalf("an embedded field is promoted: %+v", p)
	}
	type meta struct {
		Title *string `json:"title"`
	}
	type named struct {
		*meta `json:"meta"`
		Body  *string `json:"body"`
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&named{&meta{new("nested")}, new("b")}).Onto(p)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, store.RID(p.RID)); got.Title != "t2" || *got.Body != "b" {
		t.Fatalf("a named embed is a nested object, not fields: %+v", got)
	}
	// Two untagged fields of one name at one depth are invisible to
	// encoding/json; the name, not an update field, must not fail the
	// update either.
	type ghostA struct{ Ghost *string }
	type ghostB struct {
		Ghost *string `store:"-"`
	}
	type ambiguous struct {
		ghostA
		ghostB
		Body *string `json:"body"`
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&ambiguous{ghostA{new("x")}, ghostB{new("y")}, new("c")}).Onto(p)); err != nil {
		t.Fatalf("an ambiguous name drops out of the patch: %v", err)
	}
	if *p.Body != "c" {
		t.Fatalf("the rest applies: %+v", p)
	}
}

func TestPatch_RefusesBadDTOs(t *testing.T) {
	s, p := patchStore(t)
	ctx := t.Context()
	for name, req := range map[string]any{
		"unknown field": &struct {
			Bogus *string
		}{new("z")},
		"store tag on a DTO": &struct {
			Title *string `json:"title" store:"update"`
		}{new("x")},
		"defined type mismatch": &struct {
			Kind *string `json:"kind"`
		}{new("x")},
		"convertible, not assignable": &struct {
			Title *int `json:"title"`
		}{new(3)},
		"no patchable field": &struct {
			Title string `json:"title"`
		}{"x"},
	} {
		err := s.Update(ctx, store.RID(p.RID), store.Patch(req).Onto(p))
		var coder chok.Coder
		if err == nil || errors.As(err, &coder) {
			t.Errorf("%s: want a plain (500) error, got %v", name, err)
		}
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&patchReq{}).Onto(p)); !errors.Is(err, chok.ErrBadRequest) {
		t.Fatalf("an empty patch is a 400: %v", err)
	}
}

// TestPatch_ValuerOnPointerEncodes: a bare patch writes through the
// column's Valuer like Onto does.
func TestPatch_ValuerOnPointerEncodes(t *testing.T) {
	s, p := patchStore(t)
	ctx := t.Context()
	type encReq struct {
		Enc *patchEnc `json:"enc"`
	}
	if err := s.Update(ctx, store.RID(p.RID), store.Patch(&encReq{&patchEnc{raw: "bare"}}).Onto(nil)); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.Unsafe(ctx).Select("enc").Where("rid = ?", p.RID).Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored != "enc:bare" {
		t.Fatalf("stored %q", stored)
	}
	if got, _ := s.Get(ctx, store.RID(p.RID)); got.Enc == nil || got.Enc.raw != "bare" {
		t.Fatalf("round trip %+v", got.Enc)
	}
}

func TestPatch_WhereLocatorWritesEveryMatch(t *testing.T) {
	s, p := patchStore(t)
	ctx := t.Context()
	q := &patchable{Title: "second"}
	mustCreate(t, ctx, s, q)
	if err := s.Update(ctx, store.Where(where.In("id", []string{p.RID, q.RID})), store.Patch(&patchReq{Kind: new(patchKind("archived"))}).Onto(nil)); err != nil {
		t.Fatal(err)
	}
	page, _ := s.List(ctx)
	for _, it := range page.Items {
		if it.Kind != "archived" {
			t.Fatalf("not written: %+v", it)
		}
	}
}
