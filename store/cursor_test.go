package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// Arch-backlog #2 regression tests: ListWithCursor rides a composite
// (field, rid) keyset behind an opaque token. Non-unique sort columns no
// longer skip boundary ties, the token binds version/field/direction, and
// the tie-breaker is the public RID — never the internal numeric key.

// CursorTie deliberately has a NON-unique, possibly-empty Grp column: the
// worst case for single-column cursors (silent row skips on ties) and for
// the old WithCursorBy first-page heuristic (empty string treated as "no
// cursor" → infinite loop).
type CursorTie struct {
	db.Model
	Grp string `json:"grp" gorm:"size:20;not null;default:''"`
}

func (CursorTie) RIDPrefix() string { return "cti" }

func setupCursorTieStore(t *testing.T) *Store[CursorTie] {
	t.Helper()
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&CursorTie{})); err != nil {
		t.Fatal(err)
	}
	return New[CursorTie](gdb, log.Empty(), WithQueryFields("id", "grp", "created_at"))
}

// walkCursor pages through the whole store and returns every RID seen, in
// order, failing the test on any error or runaway pagination.
func walkCursor(t *testing.T, s *Store[CursorTie], field string, dir where.CursorDirection, size int) []string {
	t.Helper()
	var rids []string
	cursor := ""
	for range 100 { // hard stop: a heuristic regression means an infinite loop
		page, err := s.ListWithCursor(context.Background(), field, dir, cursor, size)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			rids = append(rids, it.RID)
		}
		if page.NextCursor == "" {
			return rids
		}
		cursor = page.NextCursor
	}
	t.Fatal("pagination did not terminate within 100 pages")
	return nil
}

func TestListWithCursor_CompositeNeverSkipsTies(t *testing.T) {
	s := setupCursorTieStore(t)
	// Three rows share the empty group, two share "a" — every page boundary
	// lands on a tie, and the empty string doubles as the old-heuristic
	// first-page trap.
	for _, grp := range []string{"", "", "", "a", "a"} {
		if err := s.Create(context.Background(), &CursorTie{Grp: grp}); err != nil {
			t.Fatal(err)
		}
	}

	rids := walkCursor(t, s, "grp", where.CursorAfter, 2)
	if len(rids) != 5 {
		t.Fatalf("composite cursor must traverse all 5 rows exactly once, got %d: %v", len(rids), rids)
	}
	seen := make(map[string]bool, len(rids))
	for _, r := range rids {
		if seen[r] {
			t.Fatalf("row %s returned twice", r)
		}
		seen[r] = true
	}

	// Same walk, descending.
	back := walkCursor(t, s, "grp", where.CursorBefore, 2)
	if len(back) != 5 {
		t.Fatalf("CursorBefore must traverse all 5 rows, got %d", len(back))
	}
}

func TestListWithCursor_OpaqueTokenRoundTrip(t *testing.T) {
	s := setupCursorTieStore(t)
	for _, grp := range []string{"a", "b", "c"} {
		if err := s.Create(context.Background(), &CursorTie{Grp: grp}); err != nil {
			t.Fatal(err)
		}
	}

	page, err := s.ListWithCursor(context.Background(), "grp", where.CursorAfter, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a NextCursor with rows remaining")
	}
	// Opaque: the raw boundary value must not be the token itself, and the
	// payload must carry the public RID, not the internal numeric id.
	if page.NextCursor == "b" {
		t.Fatal("NextCursor must be an opaque token, not the raw boundary value")
	}
	raw, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	if err != nil {
		t.Fatalf("token must be base64url: %v", err)
	}
	var tok cursorToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatalf("token payload must be the versioned JSON envelope: %v", err)
	}
	if tok.V != cursorTokenVersion || tok.Field != "grp" || tok.Dir != string(where.CursorAfter) {
		t.Fatalf("token must bind version/field/direction, got %+v", tok)
	}
	if tok.RID == "" || tok.RID[:4] != "cti_" {
		t.Fatalf("tie-breaker must be the public RID, got %q", tok.RID)
	}

	next, err := s.ListWithCursor(context.Background(), "grp", where.CursorAfter, page.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Items) != 1 || next.Items[0].Grp != "c" || next.NextCursor != "" {
		t.Fatalf("second page mismatch: %+v next=%q", next.Items, next.NextCursor)
	}
}

func TestListWithCursor_TokenContractBinding(t *testing.T) {
	s := setupCursorTieStore(t)
	for _, grp := range []string{"a", "b", "c"} {
		if err := s.Create(context.Background(), &CursorTie{Grp: grp}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ListWithCursor(context.Background(), "grp", where.CursorAfter, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	token := page.NextCursor

	cases := []struct {
		name   string
		field  string
		dir    where.CursorDirection
		cursor string
	}{
		{"different field", "id", where.CursorAfter, token},
		{"different direction", "grp", where.CursorBefore, token},
		{"garbage token", "grp", where.CursorAfter, "!!not-base64!!"},
		{"truncated token", "grp", where.CursorAfter, token[:len(token)/2]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.ListWithCursor(context.Background(), tc.field, tc.dir, tc.cursor, 1)
			if !errors.Is(err, apierr.ErrInvalidArgument) {
				t.Fatalf("must reject as ErrInvalidArgument, got %v", err)
			}
		})
	}

	// A future-versioned token is rejected instead of mis-paginating.
	forged, err := json.Marshal(cursorToken{V: cursorTokenVersion + 1, Field: "grp", Dir: string(where.CursorAfter), Kind: cursorKindString, Value: "a", RID: "cti_x"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ListWithCursor(context.Background(), "grp", where.CursorAfter,
		base64.RawURLEncoding.EncodeToString(forged), 1)
	if !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("unsupported token version must be rejected, got %v", err)
	}
}

func TestListWithCursor_TimeCursorTypeFidelity(t *testing.T) {
	// A created_at cursor must round-trip as time.Time, not a string —
	// text-vs-timestamp comparison errors on Postgres (the dual-run lane)
	// and orders wrong elsewhere. Rows likely share the same timestamp at
	// insert speed, so this also exercises tie-breaking on a time column.
	s := setupCursorTieStore(t)
	for range 3 {
		if err := s.Create(context.Background(), &CursorTie{Grp: "t"}); err != nil {
			t.Fatal(err)
		}
	}

	rids := walkCursor(t, s, "created_at", where.CursorAfter, 1)
	if len(rids) != 3 {
		t.Fatalf("time-keyed cursor must traverse all 3 rows, got %d", len(rids))
	}
}

func TestListWithCursor_IDFieldRidesPublicRID(t *testing.T) {
	// The public "id" field resolves to the rid column via the standing
	// alias; the composite (rid, rid) keyset stays correct and the token
	// still carries no numeric key.
	s := setupCursorTieStore(t)
	for range 3 {
		if err := s.Create(context.Background(), &CursorTie{Grp: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	rids := walkCursor(t, s, "id", where.CursorAfter, 2)
	if len(rids) != 3 {
		t.Fatalf("id-keyed cursor must traverse all rows, got %d", len(rids))
	}
}
