package store

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// --- patch fixtures ---------------------------------------------------------

type patchKind string

// Patchable exercises the participation and type rules: a plain updatable
// column (Title), a nullable one (*Body), a defined-type one (Kind), and a
// field whose public name equals its Go name (Extra, via json:"Extra") so a
// no-json-tag DTO field can match it.
type Patchable struct {
	db.SoftDeleteModel
	Title string    `json:"title" store:"query,update" gorm:"size:200"`
	Body  *string   `json:"body"  store:"update"       gorm:"size:200"`
	Kind  patchKind `json:"kind"  store:"update"       gorm:"size:20"`
	Extra string    `json:"Extra" store:"update"       gorm:"size:20"`
}

func (Patchable) RIDPrefix() string { return "pa" }

func setupPatchStore(t *testing.T) *Store[Patchable] {
	t.Helper()
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&Patchable{})); err != nil {
		t.Fatal(err)
	}
	return New[Patchable](gdb, log.Empty()) // store tags drive the whitelist
}

func seedPatchable(t *testing.T, s *Store[Patchable]) *Patchable {
	t.Helper()
	p := &Patchable{Title: "orig", Kind: "draft", Extra: "x"}
	if err := s.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func ptr[T any](v T) *T { return &v }

// The canonical partial-update DTO: pointer fields optional, a non-pointer
// control field that must never participate.
type patchReq struct {
	RID   string     `json:"-"`
	Title *string    `json:"title"`
	Body  *string    `json:"body"`
	Kind  *patchKind `json:"kind"`
}

// --- participation & construction -------------------------------------------

func TestPatch_NonStructPanics(t *testing.T) {
	for _, bad := range []any{nil, "str", 42, ptr(7)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Patch(%#v) should panic", bad)
				}
			}()
			_ = Patch(bad)
		}()
	}
}

func TestPatch_IsEmpty(t *testing.T) {
	if !Patch(&patchReq{}).IsEmpty() {
		t.Error("all-nil patch should be empty")
	}
	if Patch(&patchReq{Title: ptr("x")}).IsEmpty() {
		t.Error("patch with one non-nil field should not be empty")
	}
	// A struct value (not pointer) is accepted too.
	if !Patch(patchReq{}).IsEmpty() {
		t.Error("all-nil patch (by value) should be empty")
	}
}

func TestPatch_NoPatchableFields(t *testing.T) {
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	// A type with no eligible pointer field.
	type noPtr struct {
		Title string `json:"title"`
	}
	// IsEmpty must not hide it as a no-op.
	if Patch(&noPtr{}).IsEmpty() {
		t.Error("no-patchable type must report NOT empty")
	}
	err := s.Update(context.Background(), RID(p.RID), Patch(&noPtr{Title: "x"}))
	if !errors.Is(err, ErrNoPatchableFields) {
		t.Fatalf("want ErrNoPatchableFields, got %v", err)
	}
	if MapError(err) != nil {
		t.Error("ErrNoPatchableFields must not be mapped (500, programming error)")
	}
}

func TestPatch_IllegalStoreTag(t *testing.T) {
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	type badTag struct {
		Title *string `json:"title" store:"update"` // only "-" allowed on a DTO
	}
	err := s.Update(context.Background(), RID(p.RID), Patch(&badTag{Title: ptr("x")}))
	if err == nil || MapError(err) != nil {
		t.Fatalf("illegal DTO store tag must be a 500 error, got %v (mapped=%v)", err, MapError(err))
	}
}

func TestPatch_StoreDashExempts(t *testing.T) {
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	type withControl struct {
		Force *bool   `json:"force" store:"-"` // opt out
		Title *string `json:"title"`
	}
	// Force is exempt; only Title participates.
	if err := s.Update(context.Background(), RID(p.RID),
		Patch(&withControl{Force: ptr(true), Title: ptr("new")}).Onto(p)); err != nil {
		t.Fatal(err)
	}
	if p.Title != "new" {
		t.Errorf("Title = %q, want new", p.Title)
	}
}

func TestPatch_NonPointerAndUnexportedIgnored(t *testing.T) {
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	type mixed struct {
		Title    *string `json:"title"`
		Ignored  string  `json:"ignored"` // non-pointer → skipped, no unknown-field error
		hidden   *string //nolint:unused // unexported → skipped
		HiddenNo *string `json:"-"` // json:"-" → skipped
	}
	m := &mixed{Title: ptr("t2"), Ignored: "should-not-matter", HiddenNo: ptr("nope")}
	if err := s.Update(context.Background(), RID(p.RID), Patch(m).Onto(p)); err != nil {
		t.Fatalf("non-pointer/unexported/json-excluded fields must be ignored, got %v", err)
	}
	if p.Title != "t2" {
		t.Errorf("Title = %q, want t2", p.Title)
	}
}

func TestPatch_NoJSONTagUsesGoName(t *testing.T) {
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	// No json tag → public name is the Go field name "Extra", which the
	// model exposes via json:"Extra".
	type extraReq struct {
		Extra *string
	}
	if err := s.Update(context.Background(), RID(p.RID), Patch(&extraReq{Extra: ptr("y")}).Onto(p)); err != nil {
		t.Fatalf("no-json-tag field should map by Go name, got %v", err)
	}
	if p.Extra != "y" {
		t.Errorf("Extra = %q, want y", p.Extra)
	}
	// And when the Go name does not match any public name, it surfaces loudly.
	type mismatch struct {
		Bogus *string
	}
	err := s.Update(context.Background(), RID(p.RID), Patch(&mismatch{Bogus: ptr("z")}))
	if !errors.Is(err, ErrUnknownUpdateField) {
		t.Fatalf("want ErrUnknownUpdateField, got %v", err)
	}
	if MapError(err) != nil {
		t.Error("ErrUnknownUpdateField must not be mapped (500)")
	}
}

func TestPatch_EmbeddedPromotion(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	type embedBase struct {
		Body *string `json:"body"` // depth 1 — promoted to the outer patch face
	}
	type embedReq struct {
		embedBase
		Title *string `json:"title"` // depth 0
	}
	req := &embedReq{embedBase: embedBase{Body: ptr("promoted-body")}, Title: ptr("t")}
	if err := s.Update(ctx, RID(p.RID), Patch(req).Onto(p)); err != nil {
		t.Fatal(err)
	}
	if p.Title != "t" {
		t.Errorf("Title = %q, want t", p.Title)
	}
	if p.Body == nil || *p.Body != "promoted-body" {
		t.Errorf("Body = %v, want promoted-body (promoted from embed)", p.Body)
	}
}

func TestPatch_ShallowShadowsDeep(t *testing.T) {
	// The shallow field carries no json tag (public name = Go name "Title"),
	// the embedded one carries json:"Title" — same public name, so shallow
	// shadows deep. Only one side has a json tag, so go vet's structtag check
	// (which flags source-level embedded tag collisions) stays quiet while we
	// still exercise the real shadowing path. Whitebox on the plan builder.
	type shadowDeep struct {
		Title *string `json:"Title"` // depth 1
	}
	type shadowOuter struct {
		shadowDeep
		Title *string // depth 0, no tag → Go name "Title" shadows the embed
	}
	plan := buildPatchPlan(reflect.TypeOf(shadowOuter{}))
	if plan.err != nil {
		t.Fatalf("unexpected structural error: %v", plan.err)
	}
	if len(plan.fields) != 1 || plan.fields[0].public != "Title" {
		t.Fatalf("fields = %+v, want a single 'Title'", plan.fields)
	}
	// Top-level Title (depth 0, index [1]) must win over the embedded one
	// (depth 1, index [0,0]).
	if got := plan.fields[0].index; len(got) != 1 || got[0] != 1 {
		t.Errorf("index = %v, want [1] (top-level shadows embedded)", got)
	}
}

func TestPatch_AmbiguousPromotionRejected(t *testing.T) {
	// left carries json:"Name"; right carries no tag (Go name "Name") — same
	// public name, same depth. Only one has a json tag, so go vet's structtag
	// check stays quiet while the ambiguity is real. Whitebox on the builder.
	type left struct {
		Name *string `json:"Name"`
	}
	type right struct {
		Name *string // no tag → Go name "Name", collides with left at depth 1
	}
	type ambiguous struct {
		left
		right
	}
	plan := buildPatchPlan(reflect.TypeOf(ambiguous{}))
	if plan.err == nil {
		t.Fatal("ambiguous same-depth promotion must be a structural error")
	}
	// A structural defect is a server bug → 500, never mapped to a client 400.
	if MapError(plan.err) != nil {
		t.Errorf("structural error must not be mapped: %v", plan.err)
	}
}

func TestPatch_EmbeddedPointerCycleGuard(t *testing.T) {
	// An anonymous self-pointer embed must not loop the plan builder.
	type cyc struct {
		*cyc          // cycle: seen-guard must stop the walk
		Title *string `json:"title"`
	}
	if Patch(&cyc{Title: ptr("x")}).IsEmpty() {
		t.Error("expected non-empty; cycle guard should still surface Title")
	}
}

// --- error surface ----------------------------------------------------------

func TestPatch_EmptyPatchMapsTo400(t *testing.T) {
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	err := s.Update(context.Background(), RID(p.RID), Patch(&patchReq{}))
	if !errors.Is(err, ErrEmptyPatch) {
		t.Fatalf("want ErrEmptyPatch, got %v", err)
	}
	mapped := MapError(err)
	if mapped == nil || mapped.Code != apierr.ErrInvalidArgument.Code {
		t.Fatalf("ErrEmptyPatch must map to 400, got %v", mapped)
	}
}

func TestPatch_TypeMismatchRejected(t *testing.T) {
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	// *string for the defined-type Kind column (string not assignable to
	// patchKind — a defined type is not its underlying type).
	type wrongKind struct {
		Kind *string `json:"kind"`
	}
	err := s.Update(context.Background(), RID(p.RID), Patch(&wrongKind{Kind: ptr("x")}))
	if err == nil || MapError(err) != nil {
		t.Fatalf("type mismatch must be 500, got %v (mapped=%v)", err, MapError(err))
	}
	// int for a string column: convertible but NOT assignable → rejected.
	type wrongTitle struct {
		Title *int `json:"title"`
	}
	err = s.Update(context.Background(), RID(p.RID), Patch(&wrongTitle{Title: ptr(3)}))
	if err == nil || MapError(err) != nil {
		t.Fatalf("convertible-but-not-assignable must be 500, got %v", err)
	}
}

func TestPatch_ProtectedColumnRejected(t *testing.T) {
	// Whitebox: a whitelist that (misconfigured) maps a public name to a
	// managed column must be caught by the build's protected check.
	s := setupPatchStore(t)
	type sneaky struct {
		Ver *int `json:"version"`
	}
	fm := map[string]string{"version": "version"}
	_, err := Patch(&sneaky{Ver: ptr(2)}).build(context.Background(), fm, s.modelSchema)
	if !errors.Is(err, ErrProtectedUpdateField) {
		t.Fatalf("want ErrProtectedUpdateField, got %v", err)
	}
}

// --- semantics --------------------------------------------------------------

func TestPatch_ZeroValueWritesAndNullableColumn(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	// Set Body to a value, then clear it via *"" — a zero value must be
	// written, not skipped.
	if err := s.Update(ctx, RID(p.RID), Patch(&patchReq{Body: ptr("bio")}).Onto(p)); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, RID(p.RID), Patch(&patchReq{Body: ptr("")}).Onto(p)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, RID(p.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Body == nil || *got.Body != "" {
		t.Errorf("Body = %v, want empty string (zero value persisted)", got.Body)
	}
}

func TestPatch_OntoAppliesValuesAndAdvancesVersion(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	v0 := p.Version
	pc := Patch(&patchReq{Title: ptr("changed"), Kind: ptr(patchKind("published"))}).Onto(p)
	if pc.IsEmpty() {
		t.Fatal("should not be empty")
	}
	if err := s.Update(ctx, RID(p.RID), pc); err != nil {
		t.Fatal(err)
	}
	if p.Title != "changed" || p.Kind != "published" {
		t.Errorf("values not applied onto object: %+v", p)
	}
	if p.Version != v0+1 {
		t.Errorf("Version = %d, want %d (advanced on success)", p.Version, v0+1)
	}
	got, _ := s.Get(ctx, RID(p.RID))
	if got.Title != "changed" || got.Kind != "published" {
		t.Errorf("db not updated: %+v", got)
	}
}

func TestPatch_OntoStaleVersionDoesNotAdvance(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	// Concurrent writer bumps the row so p is stale.
	other, _ := s.Get(ctx, RID(p.RID))
	if err := s.Update(ctx, RID(other.RID), Patch(&patchReq{Title: ptr("bumped")}).Onto(other)); err != nil {
		t.Fatal(err)
	}
	staleV := p.Version
	err := s.Update(ctx, RID(p.RID), Patch(&patchReq{Title: ptr("mine")}).Onto(p))
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("want ErrStaleVersion, got %v", err)
	}
	if p.Version != staleV {
		t.Errorf("Version advanced on failure: %d != %d", p.Version, staleV)
	}
}

func TestPatch_NoLockSkipsVersionGuard(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	p.Version = 999 // deliberately wrong in-memory version
	err := s.Update(ctx, RID(p.RID), Patch(&patchReq{Title: ptr("forced")}).Onto(p).NoLock())
	if err != nil {
		t.Fatalf("NoLock should ignore the stale version, got %v", err)
	}
	got, _ := s.Get(ctx, RID(p.RID))
	if got.Title != "forced" {
		t.Errorf("Title = %q, want forced", got.Title)
	}
}

func TestPatch_WithVersionOverridesImplicit(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	realV := p.Version
	p.Version = 999 // implicit lock would use this wrong value
	// Explicit WithVersion(realV) overrides the implicit 999 → succeeds.
	err := s.Update(ctx, RID(p.RID), Patch(&patchReq{Title: ptr("v")}).Onto(p), WithVersion(realV))
	if err != nil {
		t.Fatalf("explicit WithVersion should override implicit, got %v", err)
	}
}

func TestPatch_BareWithVersionStaleAndNotFound(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	// Bare patch (no Onto) has no implicit lock; WithVersion supplies it.
	err := s.Update(ctx, RID(p.RID), Patch(&patchReq{Title: ptr("x")}), WithVersion(p.Version+50))
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("want ErrStaleVersion, got %v", err)
	}
	// Absent row → NotFound.
	err = s.Update(ctx, RID("pa_does_not_exist"), Patch(&patchReq{Title: ptr("x")}))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPatch_HookSeesSnapshot(t *testing.T) {
	ctx := context.Background()
	gdb := setupDB(t)
	if err := gdb.Migrate(ctx, db.Table(&Patchable{})); err != nil {
		t.Fatal(err)
	}
	var seen []string
	s := New[Patchable](gdb, log.Empty(), WithBeforeUpdate(func(_ context.Context, _ Locator, snap ChangeSnapshot) error {
		seen = snap.Fields()
		return nil
	}))
	p := seedPatchable(t, s)
	if err := s.Update(ctx, RID(p.RID), Patch(&patchReq{Title: ptr("h"), Body: ptr("b")}).Onto(p)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "body" || seen[1] != "title" {
		t.Errorf("hook snapshot fields = %v, want [body title]", seen)
	}
}

func TestPatch_WhereLocatorBatch(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	a := &Patchable{Title: "a", Kind: "draft", Extra: "e"}
	b := &Patchable{Title: "b", Kind: "draft", Extra: "e"}
	for _, p := range []*Patchable{a, b} {
		if err := s.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// Bare patch over a Where locator updates every matched row. The public
	// "id" name resolves to the rid column (the numeric key never leaves the
	// process), so the filter values are the public RIDs.
	var n int64
	err := s.Update(ctx, Where(where.WithFilterIn("id", []string{a.RID, b.RID})),
		Patch(&patchReq{Kind: ptr(patchKind("archived"))}), WithRowsAffected(&n))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows affected = %d, want 2", n)
	}
}

func TestPatch_PlanCacheConcurrent(t *testing.T) {
	// Run under -race: concurrent plan resolution for the same DTO type.
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if Patch(&patchReq{Title: ptr("x")}).IsEmpty() {
				t.Error("unexpected empty")
			}
		}()
	}
	wg.Wait()
}

// --- review-round-1 regression fixtures & tests ----------------------------

// patchEnc implements driver.Valuer/sql.Scanner on its POINTER, so the
// value form does not satisfy driver.Valuer — the exact shape that made the
// bare Patch path (which used to store a dereferenced value) lose the
// encoding relative to Onto.
type patchEnc struct{ raw string }

func (e *patchEnc) Value() (driver.Value, error) { return "enc:" + e.raw, nil }
func (e *patchEnc) Scan(src any) error {
	switch s := src.(type) {
	case string:
		e.raw = strings.TrimPrefix(s, "enc:")
	case []byte:
		e.raw = strings.TrimPrefix(string(s), "enc:")
	}
	return nil
}

type ValuerModel struct {
	db.Model
	Enc *patchEnc `json:"enc" store:"update" gorm:"type:text"`
}

func (ValuerModel) RIDPrefix() string { return "vm" }

// #1 Critical — bare Patch must run the *E Valuer, matching Onto.
func TestPatch_BareValuerColumnRoundTrips(t *testing.T) {
	ctx := context.Background()
	gdb := setupDB(t)
	if err := gdb.Migrate(ctx, db.Table(&ValuerModel{})); err != nil {
		t.Fatal(err)
	}
	s := New[ValuerModel](gdb, log.Empty())
	m := &ValuerModel{Enc: &patchEnc{raw: "seed"}}
	if err := s.Create(ctx, m); err != nil {
		t.Fatal(err)
	}

	type encReq struct {
		Enc *patchEnc `json:"enc"`
	}
	// BARE patch (no Onto): payload must reach gorm as *patchEnc so Value()
	// runs; before the fix the bare path passed a patchEnc value and skipped it.
	if err := s.Update(ctx, RID(m.RID), Patch(&encReq{Enc: &patchEnc{raw: "bare"}})); err != nil {
		t.Fatal(err)
	}
	raw, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := raw.Raw("SELECT enc FROM valuer_models WHERE rid = ?", m.RID).Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored != "enc:bare" {
		t.Errorf("stored = %q, want enc:bare (bare path must encode like Onto)", stored)
	}
	got, err := s.Get(ctx, RID(m.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Enc == nil || got.Enc.raw != "bare" {
		t.Errorf("round-trip = %+v, want raw=bare", got.Enc)
	}
}

// #2 Critical — store:"-" on an anonymous embed must exclude it.
func TestPatch_AnonymousStoreDashOptOut(t *testing.T) {
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	type ctrl struct {
		Force *bool `json:"force"`
	}
	type req struct {
		ctrl  `store:"-"` // anonymous opt-out: Force must NOT be promoted
		Title *string     `json:"title"`
	}
	// Force targets no column; promotion despite store:"-" would make build
	// reject "force" as an unknown update field (500).
	err := s.Update(context.Background(), RID(p.RID),
		Patch(&req{ctrl: ctrl{Force: ptr(true)}, Title: ptr("t")}).Onto(p))
	if err != nil {
		t.Fatalf(`store:"-" on an anonymous embed must exclude it, got %v`, err)
	}
	if p.Title != "t" {
		t.Errorf("Title = %q, want t", p.Title)
	}
}

// #2 Critical — a named embed (json:"meta") is not promoted.
func TestPatch_NamedEmbedNotPromoted(t *testing.T) {
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	type inner struct {
		X *string `json:"x"`
	}
	type req struct {
		inner `json:"meta"` // named embed → not promoted; X must not participate
		Title *string       `json:"title"`
	}
	err := s.Update(context.Background(), RID(p.RID),
		Patch(&req{inner: inner{X: ptr("nope")}, Title: ptr("t")}).Onto(p))
	if err != nil {
		t.Fatalf("named embed must not promote its fields, got %v", err)
	}
	if p.Title != "t" {
		t.Errorf("Title = %q, want t", p.Title)
	}
}

// #3 High — Onto with a nil model errors instead of silently downgrading.
func TestPatch_OntoNilRejected(t *testing.T) {
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	if err := s.Update(context.Background(), RID(p.RID),
		Patch(&patchReq{Title: ptr("x")}).Onto(nil)); err == nil {
		t.Error("Onto(nil) must error, not silently downgrade to an unlocked bare write")
	}
	var np *Patchable
	if err := s.Update(context.Background(), RID(p.RID),
		Patch(&patchReq{Title: ptr("x")}).Onto(np)); err == nil {
		t.Error("Onto(typed nil) must error")
	}
}

// #4 Medium — a genuine diamond (same field, two equal-depth paths) is
// ambiguous, not silently deduplicated to the first path.
func TestPatch_DiamondSameDepthAmbiguous(t *testing.T) {
	type leaf struct {
		X *string // no json tag → vet's structtag check stays quiet
	}
	type midA struct{ leaf }
	type midB struct{ leaf }
	type diamond struct {
		midA
		midB
	}
	plan := buildPatchPlan(reflect.TypeOf(diamond{}))
	if plan.err == nil {
		t.Fatal("same-depth diamond promotion must be ambiguous (path-based cycle guard, not global dedup)")
	}
}

// #5 Medium — a typed nil DTO is rejected at construction, not at build.
func TestPatch_TypedNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Patch(typed nil pointer) should panic at construction")
		}
	}()
	var req *patchReq
	_ = Patch(req)
}

// #6 Low — IsEmpty has no schema: an all-nil DTO reports empty even when it
// names an unknown column (the shape check is Update's job; documented).
func TestPatch_IsEmptyDoesNotValidateShape(t *testing.T) {
	type badButEmpty struct {
		Bogus *string `json:"bogus"`
	}
	if !Patch(&badButEmpty{}).IsEmpty() {
		t.Error("all-nil DTO should report empty regardless of field validity")
	}
}

// --- review-round-2 regressions --------------------------------------------

// r2 #1 — parseJSONTag mirrors encoding/json: an invalid tag name falls back
// to the Go field name, json:"-,opts" is the literal explicit name "-", and
// json:"-" excludes. (Unit test on the resolver so the cases don't have to
// be written as source struct tags that go vet's structtag check would flag.)
func TestPatch_ParseJSONTagMirrorsEncodingJSON(t *testing.T) {
	sf := reflect.StructField{Name: "Extra", Tag: reflect.StructTag(`json:"a\"b"`)}
	if name, excl, exp := parseJSONTag(sf); name != "Extra" || excl || exp {
		t.Errorf("invalid tag → (%q,%v,%v), want (Extra,false,false)", name, excl, exp)
	}
	sf2 := reflect.StructField{Name: "X", Tag: reflect.StructTag(`json:"-,omitempty"`)}
	if name, excl, exp := parseJSONTag(sf2); name != "-" || excl || !exp {
		t.Errorf(`json:"-,omitempty" → (%q,%v,%v), want ("-",false,true)`, name, excl, exp)
	}
	sf3 := reflect.StructField{Name: "Y", Tag: reflect.StructTag(`json:"-"`)}
	if _, excl, _ := parseJSONTag(sf3); !excl {
		t.Error(`json:"-" should be excluded`)
	}
}

// r2 #1 — an embed with the literal json name "-" (json:"-,omitempty") is a
// named embed, NOT an unnamed one, so its fields are not promoted.
func TestPatch_AnonymousDashNameNotPromoted(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	type Meta struct {
		Title *string `json:"title"`
	}
	type req struct {
		Meta `json:"-,omitempty"` // literal name "-" → named embed, not promoted
		Body *string              `json:"body"`
	}
	if err := s.Update(ctx, RID(p.RID),
		Patch(&req{Meta: Meta{Title: ptr("nested")}, Body: ptr("b")}).Onto(p)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, RID(p.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "orig" {
		t.Errorf(`Title = %q, want orig (embed named "-" must not promote)`, got.Title)
	}
	if got.Body == nil || *got.Body != "b" {
		t.Errorf("Body = %v, want b", got.Body)
	}
}

// r2 #1 — an unexported anonymous non-struct pointer is invisible to
// encoding/json and must not enter the plan (or it would fail the build).
func TestPatch_UnexportedAnonScalarPtrIgnored(t *testing.T) {
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	type myScalar int
	type req struct {
		*myScalar         // unexported anonymous non-struct pointer: JSON-invisible
		Title     *string `json:"title"`
	}
	if err := s.Update(context.Background(), RID(p.RID),
		Patch(&req{Title: ptr("t")}).Onto(p)); err != nil {
		t.Fatalf("unexported anonymous scalar pointer must be ignored, got %v", err)
	}
	if p.Title != "t" {
		t.Errorf("Title = %q, want t", p.Title)
	}
}

// r2 #2 — a multi-level pointer (**Req) is rejected at construction, not left
// to panic in reflection at build.
func TestPatch_MultiLevelPointerPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Patch(**Req) should panic at construction")
		}
	}()
	var inner *patchReq
	outer := &inner
	_ = Patch(outer)
}

// r3 Low#1 — a named struct embed (json:"meta"), whether value or POINTER, is
// a nested object per encoding/json and must not participate as a flat patch
// field. The value case already skipped via the non-pointer check; the
// pointer case used to slip through and resolve to public "meta".
func TestPatch_NamedPointerEmbedNotPromoted(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	type Meta struct {
		Title *string `json:"title"`
	}
	type req struct {
		*Meta `json:"meta"` // named pointer embed → nested object, not a field
		Body  *string       `json:"body"`
	}
	// If *Meta participated as public "meta", build would 500 (no such
	// column); it must be ignored, leaving only Body to update.
	if err := s.Update(ctx, RID(p.RID),
		Patch(&req{Meta: &Meta{Title: ptr("nested")}, Body: ptr("b")}).Onto(p)); err != nil {
		t.Fatalf("named pointer embed must not participate, got %v", err)
	}
	got, err := s.Get(ctx, RID(p.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "orig" {
		t.Errorf("Title = %q, want orig", got.Title)
	}
	if got.Body == nil || *got.Body != "b" {
		t.Errorf("Body = %v, want b", got.Body)
	}
}

// r4 Medium — a non-pointer field must take part in JSON name/depth
// shadowing as a blocker: a top-level non-pointer field shadows a deeper
// same-name pointer, which is then invisible and must NOT enter the plan.
// Before the fix the deeper pointer leaked in and failed shape validation,
// blocking an otherwise-valid update.
func TestPatch_NonPointerWinnerShadowsDeepPointer(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	type Deep struct {
		Title *int `json:"Title"` // deep, same public name "Title"
	}
	type req struct {
		Deep
		Title string  // top-level non-pointer (Go name "Title") shadows Deep.Title
		Body  *string `json:"body"`
	}
	// encoding/json: the top-level Title shadows Deep.Title, so the deep *int
	// is invisible; only Body is patchable.
	if err := s.Update(ctx, RID(p.RID),
		Patch(&req{Deep: Deep{Title: ptr(7)}, Title: "ignored", Body: ptr("b")}).Onto(p)); err != nil {
		t.Fatalf("non-pointer winner must shadow the deep pointer (only Body patches), got %v", err)
	}
	got, err := s.Get(ctx, RID(p.RID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Body == nil || *got.Body != "b" {
		t.Errorf("Body = %v, want b", got.Body)
	}
}

// --- review-round-5 regressions --------------------------------------------
//
// r5 defect family: a shallow field that is excluded from patching but still
// visible to encoding/json (store:"-" leaf, store:"-" container, named embed)
// must SHADOW a deeper same-name field the way encoding/json does. Before the
// fix these were filtered out BEFORE dominance, so the deeper field leaked
// into the plan under a public name the wire actually routes elsewhere —
// turning a valid update into a 500 (or a silent no-op). Generalises the r4
// non-pointer blocker to the two remaining exclusion mechanisms.

func patchPlanPublicNames(t *testing.T, v any) []string {
	t.Helper()
	plan := buildPatchPlan(reflect.TypeOf(v))
	if plan.err != nil {
		t.Fatalf("unexpected plan error: %v", plan.err)
	}
	names := make([]string, 0, len(plan.fields))
	for _, f := range plan.fields {
		names = append(names, f.public)
	}
	sort.Strings(names)
	return names
}

// r5 #1 (Medium) — a named embed must shadow a deeper same-name promoted field.
// encoding/json routes the wire key to the named nested object; the deeper
// scalar is invisible and must NOT enter the plan. End-to-end: the deep name
// is not an update column, so before the fix even a valid body-only PATCH
// tripped ErrUnknownUpdateField (500).
func TestPatch_Round5_NamedEmbedShadowsDeepSameName(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	type deep struct {
		Meta *string `json:"meta"` // "meta" is NOT an update column
	}
	type nested struct {
		X *string `json:"x"`
	}
	type req struct {
		deep
		nested `json:"meta"` // named embed shadows deep.Meta at "meta"
		Body   *string       `json:"body"`
	}
	r := &req{nested: nested{X: ptr("ignored")}, Body: ptr("newbody")}
	// "meta" must be shadowed by the named embed → not in the plan → a
	// body-only update must not fail shape validation.
	if got := patchPlanPublicNames(t, *r); len(got) != 1 || got[0] != "body" {
		t.Fatalf("plan = %v, want [body] (named embed must shadow deep 'meta')", got)
	}
	if err := s.Update(ctx, RID(p.RID), Patch(r).Onto(p)); err != nil {
		t.Fatalf("valid body update must succeed once the deep 'meta' is shadowed, got %v", err)
	}
	if p.Body == nil || *p.Body != "newbody" {
		t.Errorf("Body = %v, want newbody", p.Body)
	}
}

// r5 #1 (Medium) — a store:"-" leaf must shadow a deeper same-name field. The
// wire routes the name to the opted-out shallow field; the deeper pointer is
// invisible and must not reappear in the plan.
func TestPatch_Round5_StoreDashLeafShadowsDeepSameName(t *testing.T) {
	type deep struct {
		Name *string `json:"name"`
	}
	type req struct {
		deep
		Name *string `json:"name" store:"-"` // opted out, shadows deep.Name
		Body *string `json:"body"`
	}
	if got := patchPlanPublicNames(t, req{}); len(got) != 1 || got[0] != "body" {
		t.Fatalf(`plan = %v, want [body] (store:"-" leaf must shadow deep "name")`, got)
	}
}

// r5 #1 (Medium) — a store:"-" anonymous container's promoted field must
// shadow a deeper same-name field: the whole subtree is walked (still visible
// to encoding/json) but marked non-patchable, so it shadows without patching.
func TestPatch_Round5_StoreDashContainerShadowsDeepSameName(t *testing.T) {
	type deep struct {
		Name *string `json:"name"`
	}
	type box struct{ deep } // promotes Name one level deeper
	type shallow struct {
		Name *string `json:"name"`
	}
	type req struct {
		box
		shallow `store:"-"` // opted-out container; its Name shadows box.deep.Name
		Body    *string     `json:"body"`
	}
	if got := patchPlanPublicNames(t, req{}); len(got) != 1 || got[0] != "body" {
		t.Fatalf(`plan = %v, want [body] (store:"-" container must shadow deeper "name")`, got)
	}
}

// r5 no-regression — a store:"-" embed opts out its whole subtree WITHOUT
// tripping the illegal-store-tag guard on inner fields, so an embedded model
// (whose fields carry query/update tags) can be opted out as a unit.
func TestPatch_Round5_StoreDashContainerIgnoresInnerStoreTags(t *testing.T) {
	type modelish struct {
		Secret *string `json:"secret" store:"update"` // inner model-side tag
	}
	type req struct {
		modelish `store:"-"`
		Title    *string `json:"title"`
	}
	plan := buildPatchPlan(reflect.TypeOf(req{}))
	if plan.err != nil {
		t.Fatalf(`store:"-" container must ignore inner store tags, got %v`, plan.err)
	}
	if got := patchPlanPublicNames(t, req{}); len(got) != 1 || got[0] != "title" {
		t.Errorf("plan = %v, want [title] (secret opted out)", got)
	}
}

// r5 no-regression — a public name shared by two opted-out containers is a
// clash among purely non-patchable candidates: encoding/json drops it, so
// Patch drops it silently too (no fail-loud 500 that would kill the whole DTO).
func TestPatch_Round5_TwoOptedOutContainersSameNameNoError(t *testing.T) {
	// Both containers promote a field under its Go name "Flag" (no json tag, so
	// go vet's structtag check stays quiet while the same-name clash is real).
	type ctrlA struct {
		Flag *string
	}
	type ctrlB struct {
		Flag *string
	}
	type req struct {
		ctrlA `store:"-"`
		ctrlB `store:"-"`
		Title *string `json:"title"`
	}
	plan := buildPatchPlan(reflect.TypeOf(req{}))
	if plan.err != nil {
		t.Fatalf("a name shared only by opted-out containers must drop silently, not error: %v", plan.err)
	}
	if got := patchPlanPublicNames(t, req{}); len(got) != 1 || got[0] != "title" {
		t.Errorf("plan = %v, want [title]", got)
	}
}

// --- review-round-6 regressions --------------------------------------------
//
// Selection must run encoding/json dominance among ALL candidates at the
// shallowest depth (a non-patchable sibling can win the name via tag-dominance
// or make it ambiguous) BEFORE emitting the patchable winner. Round 5's
// "exactly one patchable at min depth wins" bypassed this: a lone patchable
// field was emitted even when encoding/json dropped the name or routed it to a
// non-patchable sibling, exposing a name the wire never accepts (P⊄J) and, for
// a name that is not an update column, 500ing an otherwise-valid request.

// r6 #1 (Medium) — two untagged same-depth siblings (one patchable, one opted
// out) are ambiguous to encoding/json → the name is dropped, so Patch must drop
// it too (round 5 wrongly emitted the patchable one).
func TestPatch_Round6_UntaggedSameDepthAmbiguityDrops(t *testing.T) {
	// Both resolve to the Go name "X" (no json tag → go vet quiet, and both
	// untagged so encoding/json drops the name).
	type a struct {
		X *string
	}
	type b struct {
		X *string `store:"-"`
	}
	type req struct {
		a
		b
		Body *string `json:"body"`
	}
	if got := patchPlanPublicNames(t, req{}); len(got) != 1 || got[0] != "body" {
		t.Errorf("plan = %v, want [body] (untagged same-depth ambiguity drops X)", got)
	}
}

// r6 #1 (Medium) — end-to-end: an untagged same-depth ambiguity on a name that
// is NOT an update column must not phantom-500 a valid body-only PATCH. This is
// the reviewer's minimal repro.
func TestPatch_Round6_UntaggedAmbiguityNoPhantom500(t *testing.T) {
	ctx := context.Background()
	s := setupPatchStore(t)
	p := seedPatchable(t, s)
	type patchableG struct {
		Ghost *string // untagged "Ghost", not an update column
	}
	type optedG struct {
		Ghost *string `store:"-"` // untagged "Ghost", opted out
	}
	type req struct {
		patchableG
		optedG
		Body *string `json:"body"`
	}
	// encoding/json drops the ambiguous "Ghost"; Patch must too, so the plan is
	// [body] and a valid body update must not fail with ErrUnknownUpdateField.
	if err := s.Update(ctx, RID(p.RID), Patch(&req{Body: ptr("ok")}).Onto(p)); err != nil {
		t.Fatalf("valid body update must not 500 on a phantom ambiguous name, got %v", err)
	}
	if p.Body == nil || *p.Body != "ok" {
		t.Errorf("Body = %v, want ok", p.Body)
	}
}

// r6 #1 (Medium) — a tagged opted-out field wins the name via encoding/json
// tag-dominance; the name is then not patchable and Patch must NOT fall back to
// the untagged patchable sibling.
func TestPatch_Round6_TaggedOptOutWinsThenDrops(t *testing.T) {
	// a.X untagged (Go name "X"); ctrl.X tagged json:"X" but opted out. Only one
	// side carries a json tag, so go vet's structtag check stays quiet.
	type a struct {
		X *string
	}
	type ctrl struct {
		X *string `json:"X" store:"-"`
	}
	type req struct {
		a
		ctrl
		Body *string `json:"body"`
	}
	if got := patchPlanPublicNames(t, req{}); len(got) != 1 || got[0] != "body" {
		t.Errorf("plan = %v, want [body] (tagged opt-out wins 'X' → not patchable)", got)
	}
}

// r6 #1 (Medium) — a tagged non-pointer field wins the name via tag-dominance;
// non-pointers are not patchable, so Patch must drop the name rather than route
// it to the untagged pointer sibling.
func TestPatch_Round6_TaggedNonPointerWinsThenDrops(t *testing.T) {
	type a struct {
		X *string
	}
	type b struct {
		X string `json:"X"` // tagged non-pointer
	}
	type req struct {
		a
		b
		Body *string `json:"body"`
	}
	if got := patchPlanPublicNames(t, req{}); len(got) != 1 || got[0] != "body" {
		t.Errorf("plan = %v, want [body] (tagged non-pointer wins 'X' → not patchable)", got)
	}
}

// r6 — dominance is honoured in BOTH directions: when a tagged patchable field
// and an untagged opted-out sibling genuinely CONTEND for the same public name,
// encoding/json tag-dominance routes the name to the tagged patchable field, so
// Patch emits it (the fix must not over-drop mixed-eligibility clashes). Both
// sides resolve to "X" (only the patchable side is tagged, so go vet stays
// quiet), so the same-name competition is real — a naive "drop any name with a
// non-patchable sibling" rule would fail this.
func TestPatch_Round6_TaggedPatchableWinsSameName(t *testing.T) {
	type patchable struct {
		X *string `json:"X"` // tagged → public "X", patchable
	}
	type opted struct {
		X *string `store:"-"` // untagged Go name "X" → same public name, opted out
	}
	type req struct {
		patchable
		opted
	}
	// encoding/json: both resolve to "X" at the same depth, exactly one tagged
	// (patchable.X) → tag-dominance routes "X" to patchable.X.
	r := &req{patchable: patchable{X: ptr("P")}, opted: opted{X: ptr("O")}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["X"] != "P" {
		t.Fatalf(`json winner for "X" = %q, want "P" (patchable.X); harness assumption broken`, m["X"])
	}
	// The plan must contain "X" mapping to patchable.X (index [0,0]), not drop it
	// and not route it to the opted-out opted.X (index [1,0]).
	plan := buildPatchPlan(reflect.TypeOf(*r))
	if plan.err != nil {
		t.Fatalf("unexpected plan error: %v", plan.err)
	}
	var idx []int
	for _, f := range plan.fields {
		if f.public == "X" {
			idx = f.index
		}
	}
	if idx == nil {
		t.Fatalf("plan = %v, want it to contain patchable name \"X\"", patchPlanPublicNames(t, *r))
	}
	if len(idx) != 2 || idx[0] != 0 || idx[1] != 0 {
		t.Errorf(`"X" index = %v, want [0 0] (patchable.X, not opted.X [1 0])`, idx)
	}
}
