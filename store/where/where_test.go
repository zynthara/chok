package where

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// ---------------------------------------------------------------------------
// Apply pagination coherence (HandleList envelope fix)
// ---------------------------------------------------------------------------

// TestApply_ClampKeepsPageInfoCoherent: when MaxPageSize clamps a
// page-based request, the effective size AND the offset move together
// ("page 2 of clamped-size pages") and Config.PageInfo reports exactly
// what was rendered into the SQL — the envelope's single source.
func TestApply_ClampKeepsPageInfoCoherent(t *testing.T) {
	db := testDB(t)
	_, cfg, err := Apply(db, nil, []Option{WithMaxPageSize(100), WithPage(2, 5000)})
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.PageInfo()
	want := PageInfo{Page: 2, Size: 100, Offset: 100}
	if got != want {
		t.Fatalf("clamped PageInfo = %+v, want %+v", got, want)
	}
}

// TestApply_UnclampedPageInfoPassesThrough: below the cap nothing moves.
func TestApply_UnclampedPageInfoPassesThrough(t *testing.T) {
	db := testDB(t)
	_, cfg, err := Apply(db, nil, []Option{WithMaxPageSize(100), WithPage(3, 10)})
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.PageInfo()
	want := PageInfo{Page: 3, Size: 10, Offset: 20}
	if got != want {
		t.Fatalf("PageInfo = %+v, want %+v", got, want)
	}
}

// TestApply_OffsetPaginationPageInfo: raw offset/limit report Page 0 —
// there is no page number to lie about — and the clamp still applies
// to the limit without touching the caller's offset.
func TestApply_OffsetPaginationPageInfo(t *testing.T) {
	db := testDB(t)
	_, cfg, err := Apply(db, nil, []Option{WithMaxPageSize(50), WithOffset(7), WithLimit(200)})
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.PageInfo()
	want := PageInfo{Page: 0, Size: 50, Offset: 7}
	if got != want {
		t.Fatalf("PageInfo = %+v, want %+v", got, want)
	}
}

func TestApply_MaxPageSizeCannotBeRaisedOrExceedPackageCeiling(t *testing.T) {
	db := testDB(t)
	_, cfg, err := Apply(db, nil, []Option{
		WithMaxPageSize(25),
		WithMaxPageSize(50_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.PageInfo().Size; got != 25 {
		t.Fatalf("later max-page option raised the effective cap: got %d want 25", got)
	}

	_, cfg, err = Apply(db, nil, []Option{WithMaxPageSize(50_000)})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.PageInfo().Size; got != MaxPageSize {
		t.Fatalf("configured cap escaped package ceiling: got %d want %d", got, MaxPageSize)
	}
}

func TestWithFilterNull_BuiltinsUseAllowlistAndMarkFilter(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"deleted_at": "deleted_at"}
	for _, opt := range []Option{WithFilterNull("deleted_at"), WithFilterNotNull("deleted_at")} {
		_, cfg, err := Apply(db, fm, []Option{opt})
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.HasFilter || cfg.DegenerateFilter {
			t.Fatalf("null predicate metadata = %+v", cfg)
		}
	}
	if _, _, err := Apply(db, fm, []Option{WithFilterNull("secret")}); !errors.Is(err, ErrUnknownField) {
		t.Fatalf("null predicate bypassed field allowlist: %v", err)
	}
}

// ---------------------------------------------------------------------------
// WithCursor tests
// ---------------------------------------------------------------------------

func TestWithCursor_After(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"id": "rid"}

	opt := WithCursor("id", CursorAfter, "usr_abc", 10)
	result, err := opt(db, &Config{}, fm)
	if err != nil {
		t.Fatalf("WithCursor(CursorAfter) error: %v", err)
	}
	if result == nil {
		t.Fatal("WithCursor returned nil DB")
	}
}

func TestWithCursor_Before(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"id": "rid"}

	opt := WithCursor("id", CursorBefore, "usr_xyz", 10)
	result, err := opt(db, &Config{}, fm)
	if err != nil {
		t.Fatalf("WithCursor(CursorBefore) error: %v", err)
	}
	if result == nil {
		t.Fatal("WithCursor returned nil DB")
	}
}

func TestWithCursor_InvalidSize(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"id": "rid"}

	opt := WithCursor("id", CursorAfter, "usr_abc", 0)
	_, err := opt(db, &Config{}, fm)
	if err == nil {
		t.Fatal("expected error for size < 1")
	}
	if !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("expected ErrInvalidParam, got %v", err)
	}
}

func TestWithCursor_InvalidDirection(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"id": "rid"}

	opt := WithCursor("id", CursorDirection("sideways"), "usr_abc", 10)
	_, err := opt(db, &Config{}, fm)
	if err == nil {
		t.Fatal("expected error for invalid direction")
	}
	if !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("expected ErrInvalidParam, got %v", err)
	}
}

func TestWithCursor_UnknownField(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"id": "rid"}

	opt := WithCursor("bogus", CursorAfter, "x", 10)
	_, err := opt(db, &Config{}, fm)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !errors.Is(err, ErrUnknownField) {
		t.Fatalf("expected ErrUnknownField, got %v", err)
	}
}

func TestWithCursor_SetsConfigFlags(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"id": "rid"}
	cfg := &Config{}

	opt := WithCursor("id", CursorAfter, "usr_abc", 5)
	_, err := opt(db, cfg, fm)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasPage {
		t.Fatal("HasPage should be true after WithCursor")
	}
	if !cfg.HasCursor {
		t.Fatal("HasCursor should be true after WithCursor")
	}
}

func TestWithCursor_CountOnly_Noop(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"id": "rid"}
	cfg := &Config{countOnly: true}

	opt := WithCursor("id", CursorAfter, "usr_abc", 5)
	result, err := opt(db, cfg, fm)
	if err != nil {
		t.Fatal(err)
	}
	// In countOnly mode the DB should be returned unchanged (pagination skipped).
	if result == nil {
		t.Fatal("expected non-nil DB in countOnly mode")
	}
	if !cfg.HasPage {
		t.Fatal("HasPage should still be set in countOnly mode")
	}
}

// TestResolveField_RejectsTaintedColumns covers the C2 fix: resolveField
// must refuse column names that aren't safe SQL identifiers, even when
// the caller has whitelisted them via WithQueryFields, because every
// downstream raw-SQL site (ORDER BY, cursor predicates) trusts the
// returned name.
func TestResolveField_RejectsTaintedColumns(t *testing.T) {
	cases := []struct {
		name string
		col  string
	}{
		{"semicolon", "id; DROP TABLE users"},
		{"quote", "id'"},
		{"comment", "id--"},
		{"space", "id name"},
		{"leading_digit", "0col"},
		{"three_dots", "a.b.c"},
		{"empty_segment", "a..b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fm := map[string]string{"f": tc.col}
			if _, err := resolveField(fm, "f"); err == nil {
				t.Fatalf("resolveField accepted tainted column %q", tc.col)
			} else if !errors.Is(err, ErrUnknownField) {
				t.Fatalf("expected ErrUnknownField, got %v", err)
			}
		})
	}
}

func TestResolveField_AcceptsSafeColumns(t *testing.T) {
	for _, col := range []string{"id", "user_id", "users.id", "_internal"} {
		fm := map[string]string{"f": col}
		got, err := resolveField(fm, "f")
		if err != nil {
			t.Fatalf("safe column %q rejected: %v", col, err)
		}
		if got != col {
			t.Fatalf("expected %q, got %q", col, got)
		}
	}
}

// TestWithFilterLike_EscapesPattern covers the H3 fix: the default
// WithFilterLike must escape % and _ so user input cannot expand the
// match set. WithFilterLikeRaw remains an explicit opt-out for trusted
// pattern construction.
func TestWithFilterLike_EscapesPattern(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"name": "name"}
	cfg := &Config{}

	got, err := WithFilterLike("name", "100%_off")(db, cfg, fm)
	if err != nil {
		t.Fatal(err)
	}
	stmt := got.Session(&gorm.Session{DryRun: true}).Find(&struct {
		Name string
	}{}).Statement
	sql := stmt.SQL.String()
	if !contains(sql, `ESCAPE '\'`) {
		t.Fatalf("escaped LIKE missing ESCAPE clause: %s", sql)
	}
	// One of the SQL drivers may parameterise the value; just verify
	// the escape clause was injected — the value escaping itself is
	// covered by escapeLikePattern's own contract.
}

// TestWithFilterLikeRaw_PassesThrough verifies the raw-mode option
// preserves wildcards verbatim for trusted callers.
func TestWithFilterLikeRaw_PassesThrough(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"sku": "sku"}
	cfg := &Config{}

	got, err := WithFilterLikeRaw("sku", "ABC-%")(db, cfg, fm)
	if err != nil {
		t.Fatal(err)
	}
	stmt := got.Session(&gorm.Session{DryRun: true}).Find(&struct {
		Sku string
	}{}).Statement
	if contains(stmt.SQL.String(), `ESCAPE`) {
		t.Fatalf("raw LIKE should not inject ESCAPE clause: %s", stmt.SQL.String())
	}
}

// Arch-backlog #2: only a nil fieldCursor means "first page". The old
// heuristic also treated the empty string and id 0 as first-page markers,
// so a legitimate empty-string boundary value restarted pagination — an
// infinite loop for the caller.
func TestWithCursorBy_EmptyStringCursorIsNotFirstPage(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"grp": "grp"}

	got, err := WithCursorBy("grp", CursorAfter, "", 7, 2)(db.Table("ties"), &Config{}, fm)
	if err != nil {
		t.Fatal(err)
	}
	stmt := got.Session(&gorm.Session{DryRun: true}).Find(&[]map[string]any{}).Statement
	if !contains(stmt.SQL.String(), "(grp, id) > (?, ?)") {
		t.Fatalf("empty-string cursor must render the keyset predicate, got: %s", stmt.SQL.String())
	}
}

func TestWithCursorBy_NilCursorIsFirstPage(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"grp": "grp"}

	got, err := WithCursorBy("grp", CursorAfter, nil, 0, 2)(db.Table("ties"), &Config{}, fm)
	if err != nil {
		t.Fatal(err)
	}
	stmt := got.Session(&gorm.Session{DryRun: true}).Find(&[]map[string]any{}).Statement
	sql := stmt.SQL.String()
	if contains(sql, "(grp, id) >") {
		t.Fatalf("nil cursor must not render a keyset predicate, got: %s", sql)
	}
	if !contains(sql, "ORDER BY grp ASC,id ASC") && !contains(sql, "ORDER BY grp ASC, id ASC") {
		t.Fatalf("first page must order by both keyset columns, got: %s", sql)
	}
}

// WithCursorByField takes the tie-breaker as a raw column: the sort field
// is client-facing and resolves through the allowlist, while the tie column
// is trusted-caller infrastructure (Store binds the model's RID column) —
// deliberately NOT allowlist-resolved, so stores that never exposed "id"
// still paginate. It is identifier-validated instead.
func TestWithCursorByField_TieColumnIsDirectAndValidated(t *testing.T) {
	db := testDB(t)
	fm := map[string]string{"grp": "grp"} // note: no "id" in the allowlist

	got, err := WithCursorByField("grp", CursorAfter, "a", "rid", "cti_x", 2)(db.Table("ties"), &Config{}, fm)
	if err != nil {
		t.Fatal(err)
	}
	stmt := got.Session(&gorm.Session{DryRun: true}).Find(&[]map[string]any{}).Statement
	if !contains(stmt.SQL.String(), "(grp, rid) > (?, ?)") {
		t.Fatalf("tie column must bind directly without allowlist resolution, got: %s", stmt.SQL.String())
	}

	if _, err := WithCursorByField("grp", CursorAfter, "a", "rid; DROP TABLE ties", "x", 2)(db, &Config{}, fm); !errors.Is(err, ErrUnknownField) {
		t.Fatalf("unsafe tie identifier must be rejected, got %v", err)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
