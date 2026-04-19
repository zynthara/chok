package where

import (
	"errors"
	"testing"

	"gorm.io/driver/sqlite"
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

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
