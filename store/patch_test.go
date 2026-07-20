package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
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
