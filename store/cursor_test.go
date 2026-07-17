package store

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// Arch-backlog #2 regression tests: ListWithCursor rides a composite
// (field, rid) keyset behind an opaque token. Non-unique sort columns no
// longer skip boundary ties, the token binds version/field/direction, and
// the tie-breaker is the public RID — never the internal numeric key.

// tieStatus is a defined string type — the round-6 P1 case: GORM's
// Field.ValueOf preserves defined types, which an exact-type switch in the
// cursor encoder silently failed to match, truncating pagination.
type tieStatus string

// tieClock is the round-7 P1 case, modelled on gorm.io/datatypes.Time: the
// Go underlying type is an integer (time.Duration) while the wire type is
// a string. Deriving the token expectation from the Go type alone would
// expect "int" and reject every legitimately issued "str" token on the
// second page.
type tieClock time.Duration

func (c tieClock) Value() (driver.Value, error) { return time.Duration(c).String(), nil }

func (c *tieClock) Scan(v any) error {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		return fmt.Errorf("tieClock: unsupported scan type %T", v)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*c = tieClock(d)
	return nil
}

// tieDynamic is a legal driver.Valuer whose concrete wire type varies by
// value. The interface permits this: zero probes as string, while real rows
// encode as int64. It proves a zero sample cannot by itself guarantee that
// every boundary will match the schema-pinned cursor kind.
type tieDynamic int64

func (v tieDynamic) Value() (driver.Value, error) {
	if v == 0 {
		return "0", nil
	}
	return int64(v), nil
}

func (v *tieDynamic) Scan(src any) error {
	var (
		n   int64
		err error
	)
	switch x := src.(type) {
	case int64:
		n = x
	case string:
		n, err = strconv.ParseInt(x, 10, 64)
	case []byte:
		n, err = strconv.ParseInt(string(x), 10, 64)
	default:
		return fmt.Errorf("tieDynamic: unsupported scan type %T", src)
	}
	if err != nil {
		return err
	}
	*v = tieDynamic(n)
	return nil
}

// CursorTie deliberately has a NON-unique, possibly-empty Grp column (the
// worst case for single-column cursors and for the old WithCursorBy
// first-page heuristic), a defined-type Status, a NARROW int8 Pri (range
// validation), a Valuer Clock whose wire type differs from its Go type,
// and a []byte Blob (statically underivable → rejected as a cursor field).
type CursorTie struct {
	db.Model
	Grp     string     `json:"grp" gorm:"size:20;not null;default:''"`
	Status  tieStatus  `json:"status" gorm:"size:20;not null;default:''"`
	Pri     int8       `json:"pri" gorm:"not null;default:0"`
	Clock   tieClock   `json:"clock" gorm:"type:text;not null"`
	Ts      int64      `json:"ts" gorm:"serializer:unixtime;type:timestamp;not null"`
	Dynamic tieDynamic `json:"dynamic" gorm:"type:text;not null"`
	Blob    []byte     `json:"blob"`
}

func (CursorTie) RIDPrefix() string { return "cti" }

func setupCursorTieStore(t *testing.T) *Store[CursorTie] {
	t.Helper()
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&CursorTie{})); err != nil {
		t.Fatal(err)
	}
	return New[CursorTie](gdb, log.Empty(),
		WithQueryFields("id", "grp", "created_at", "status", "pri", "clock", "ts", "dynamic", "blob"))
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

func TestListWithCursor_DefinedTypeCursorField(t *testing.T) {
	// Round-6 P1: `type Status string` and friends fell through the exact
	// type switch — the encoder returned "no cursor" while the lookahead
	// had proven more rows exist, silently truncating pagination.
	s := setupCursorTieStore(t)
	for _, st := range []tieStatus{"a", "a", "b"} {
		if err := s.Create(context.Background(), &CursorTie{Status: st}); err != nil {
			t.Fatal(err)
		}
	}

	var rids []string
	cursor := ""
	for range 10 {
		page, err := s.ListWithCursor(context.Background(), "status", where.CursorAfter, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			rids = append(rids, it.RID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(rids) != 3 {
		t.Fatalf("defined-type cursor field must traverse all 3 rows, got %d", len(rids))
	}
}

func TestListWithCursor_UnencodableBoundaryIsError(t *testing.T) {
	// Round-6 P1 (honesty half), tightened by round-7: a field whose token
	// kind is not statically derivable is rejected UP FRONT — before any
	// token is issued — as a server-side configuration error, never as an
	// empty NextCursor that tells the client "done" while rows remain.
	s := setupCursorTieStore(t)
	for _, b := range [][]byte{[]byte("x"), []byte("y")} {
		if err := s.Create(context.Background(), &CursorTie{Blob: b}); err != nil {
			t.Fatal(err)
		}
	}

	_, err := s.ListWithCursor(context.Background(), "blob", where.CursorAfter, "", 1)
	if err == nil {
		t.Fatal("underivable cursor field must error, not truncate silently")
	}
	if errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("the rejection is a server-side condition, not client input: %v", err)
	}
}

func TestListWithCursor_ValuerWireTypeDiffersFromGoType(t *testing.T) {
	// Round-7 P1: tieClock's Go underlying type is int64 (time.Duration)
	// but its wire type is string. The expectation derivation must be
	// Valuer-aware like the encoder — the old Go-type derivation expected
	// "int", so the framework 400-rejected the very token it issued on the
	// previous page.
	s := setupCursorTieStore(t)
	for _, d := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour} {
		if err := s.Create(context.Background(), &CursorTie{Clock: tieClock(d)}); err != nil {
			t.Fatal(err)
		}
	}

	var rids []string
	cursor := ""
	for range 10 {
		page, err := s.ListWithCursor(context.Background(), "clock", where.CursorAfter, cursor, 1)
		if err != nil {
			t.Fatalf("self-issued token must be accepted: %v", err)
		}
		for _, it := range page.Items {
			rids = append(rids, it.RID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(rids) != 3 {
		t.Fatalf("Valuer-typed cursor field must traverse all 3 rows, got %d", len(rids))
	}

	// The schema pin follows the WIRE kind: forging an "int" token on the
	// clock field is rejected even though the Go underlying type is int64.
	raw, err := json.Marshal(cursorToken{V: cursorTokenVersion, Field: "clock", Dir: string(where.CursorAfter), Kind: cursorKindInt, Value: "42", RID: "cti_x"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ListWithCursor(context.Background(), "clock", where.CursorAfter,
		base64.RawURLEncoding.EncodeToString(raw), 1)
	if !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("forged kind on a Valuer field must be rejected, got %v", err)
	}
}

func TestListWithCursor_NarrowIntOverflowRejected(t *testing.T) {
	// Round-7 P2: Pri is int8. A token whose value parses as int64 but
	// overflows the field's declared width must be a clean 400, not a
	// driver/database conversion failure downstream.
	s := setupCursorTieStore(t)
	if err := s.Create(context.Background(), &CursorTie{Pri: 1}); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(cursorToken{V: cursorTokenVersion, Field: "pri", Dir: string(where.CursorAfter), Kind: cursorKindInt, Value: "300", RID: "cti_x"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ListWithCursor(context.Background(), "pri", where.CursorAfter,
		base64.RawURLEncoding.EncodeToString(raw), 1)
	if !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("int8 overflow must be rejected as ErrInvalidArgument, got %v", err)
	}

	// In-range values still work end to end.
	if _, err := s.ListWithCursor(context.Background(), "pri", where.CursorAfter, "", 1); err != nil {
		t.Fatalf("narrow int cursor field must still paginate: %v", err)
	}
}

func TestListWithCursor_SerializerFieldRoundTrip(t *testing.T) {
	// Round-8 P1: Ts is int64 with `serializer:unixtime` — GORM's
	// Field.ValueOf wraps serializer fields into driver.Valuers, so the
	// encoder sees time.Time while the Go type says int. The expectation
	// probe now runs the exact encoder pipeline on a zero row, so both
	// sides agree on "time" and self-issued tokens stay consumable.
	s := setupCursorTieStore(t)
	for _, ts := range []int64{1000, 2000, 3000} {
		if err := s.Create(context.Background(), &CursorTie{Ts: ts}); err != nil {
			t.Fatal(err)
		}
	}

	var rids []string
	cursor := ""
	for range 10 {
		page, err := s.ListWithCursor(context.Background(), "ts", where.CursorAfter, cursor, 1)
		if err != nil {
			t.Fatalf("self-issued token must be accepted: %v", err)
		}
		for _, it := range page.Items {
			rids = append(rids, it.RID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(rids) != 3 {
		t.Fatalf("serializer cursor field must traverse all 3 rows, got %d", len(rids))
	}

	// The pin follows the serialized wire kind: forging "int" — the Go
	// type's kind — is rejected.
	raw, err := json.Marshal(cursorToken{V: cursorTokenVersion, Field: "ts", Dir: string(where.CursorAfter), Kind: cursorKindInt, Value: "2000", RID: "cti_x"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ListWithCursor(context.Background(), "ts", where.CursorAfter,
		base64.RawURLEncoding.EncodeToString(raw), 1)
	if !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("forged Go-type kind on a serializer field must be rejected, got %v", err)
	}
}

func TestEncodeCursorValue_NeverSignsUndecodableToken(t *testing.T) {
	// Round-8 P2: the decoder rejects NaN, so the encoder must refuse to
	// sign it — otherwise a NaN boundary row with a next page issues a
	// token the client can never consume. ±Inf stays symmetric: signable
	// and decodable.
	if _, _, err := encodeCursorValue(math.NaN()); err == nil {
		t.Fatal("encoder must refuse to sign NaN")
	}
	kind, repr, err := encodeCursorValue(math.Inf(1))
	if err != nil {
		t.Fatalf("+Inf must be signable: %v", err)
	}
	if _, err := decodeCursorValue(kind, repr, 0); err != nil {
		t.Fatalf("everything the encoder signs must decode: %v", err)
	}
}

func TestEncodeCursorValue_Round9RejectsNonRFC3339Time(t *testing.T) {
	// time.Format happily emits a five-digit year, but time.Parse with the
	// same RFC3339Nano layout rejects it. PostgreSQL can store such years,
	// so the cursor encoder must use strict RFC3339 validation rather than
	// sign an unusable NextCursor.
	outsideRFC3339 := time.Date(10000, time.January, 2, 3, 4, 5, 0, time.UTC)
	if _, _, err := encodeCursorValue(outsideRFC3339); err == nil {
		t.Fatal("encoder must reject a time value its decoder cannot parse")
	}

	valid := time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
	kind, repr, err := encodeCursorValue(valid)
	if err != nil {
		t.Fatalf("valid RFC3339 time must encode: %v", err)
	}
	if _, err := decodeCursorValue(kind, repr, 0); err != nil {
		t.Fatalf("encoded time must decode: %v", err)
	}
}

func TestListWithCursor_Round9DynamicValuerDriftRejectedBeforeSigning(t *testing.T) {
	// The zero row pins tieDynamic to string, while non-zero boundaries
	// return int64. The mismatch is a server-side field-contract error: it
	// must be caught on the issuing page, never returned as a token that the
	// next request rejects as forged client input. Exercised off-database —
	// encodeItemCursor takes the boundary item directly — so no dialect ever
	// binds tieDynamic's non-zero int64 wire value against the text column
	// (the previous DB-backed version was sqlite-only by accident).
	s := setupCursorTieStore(t)

	col, err := where.ResolveField(s.queryFieldMap, "dynamic")
	if err != nil {
		t.Fatal(err)
	}
	fieldSchema := s.modelSchema.LookUpField(col)
	if fieldSchema == nil {
		t.Fatal("dynamic column missing from schema")
	}
	spec, ok := cursorSpecForSchemaField(s.modelSchema.ModelType, fieldSchema)
	if !ok {
		t.Fatal("dynamic must derive a spec from its zero probe")
	}
	if spec.kind != cursorKindString {
		t.Fatalf("zero probe must pin the wire kind to str, got %q", spec.kind)
	}

	item := CursorTie{Dynamic: 1}
	item.RID = "cti_drift" // satisfy the RID guard so the drift check is what fires
	_, err = s.encodeItemCursor(item, "dynamic", where.CursorAfter, spec)
	if err == nil {
		t.Fatal("wire-kind drift must fail before signing")
	}
	if errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("wire-kind drift is a server-side field contract, not client input: %v", err)
	}
	if !strings.Contains(err.Error(), "wire kind changed") {
		t.Fatalf("expected explicit wire-kind drift error, got %v", err)
	}
}

func TestEncodeItemCursor_RejectsInvalidUTF8Boundary(t *testing.T) {
	// Post-commit review #1: json.Marshal silently replaces invalid UTF-8
	// with U+FFFD without erroring, so a token signed over such a boundary
	// decodes to a DIFFERENT value and the next page scans from a wrong
	// position — the silent variant of signing an unconsumable token.
	// SQLite TEXT can genuinely hold such bytes. Exercised off-database:
	// Postgres enforces valid UTF-8 and could never store the row, so a
	// DB-backed version would be sqlite-only.
	if _, _, err := encodeCursorValue("a\xffb"); err == nil {
		t.Fatal("encoder must refuse an invalid-UTF-8 string boundary")
	}

	s := setupCursorTieStore(t)
	col, err := where.ResolveField(s.queryFieldMap, "grp")
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := cursorSpecForSchemaField(s.modelSchema.ModelType, s.modelSchema.LookUpField(col))
	if !ok {
		t.Fatal("grp must derive a spec")
	}
	item := CursorTie{Grp: "a\xffb"}
	item.RID = "cti_utf8"
	_, err = s.encodeItemCursor(item, "grp", where.CursorAfter, spec)
	if err == nil {
		t.Fatal("signing over an invalid-UTF-8 boundary must error, not mutate silently")
	}
	if errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("the rejection is a server-side data condition, not client input: %v", err)
	}
}

func TestDecodeCursorValue_FloatEdgeCases(t *testing.T) {
	// NaN never orders — reject; ±Inf is comparable and a float8 column can
	// genuinely hold it, so the encoder's own output must stay decodable.
	if _, err := decodeCursorValue(cursorKindFloat, "NaN", 0); err == nil {
		t.Fatal("NaN must be rejected")
	}
	if _, err := decodeCursorValue(cursorKindFloat, "+Inf", 0); err != nil {
		t.Fatalf("+Inf must round-trip: %v", err)
	}
	if _, err := decodeCursorValue(cursorKindFloat, "1e400", 32); err == nil {
		t.Fatal("float32 overflow must be rejected")
	}
}

func TestListWithCursor_WorksWithoutIDInAllowlist(t *testing.T) {
	// Round-6 P2: the tie-breaker binds to the model's RID column directly.
	// A store whose allowlist never exposes "id" must still paginate.
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&CursorTie{})); err != nil {
		t.Fatal(err)
	}
	s := New[CursorTie](gdb, log.Empty(), WithQueryFields("grp"))

	for _, grp := range []string{"a", "b", "c"} {
		if err := s.Create(context.Background(), &CursorTie{Grp: grp}); err != nil {
			t.Fatal(err)
		}
	}

	page, err := s.ListWithCursor(context.Background(), "grp", where.CursorAfter, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("first page mismatch: %d items, next=%q", len(page.Items), page.NextCursor)
	}
	next, err := s.ListWithCursor(context.Background(), "grp", where.CursorAfter, page.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Items) != 1 || next.NextCursor != "" {
		t.Fatalf("second page mismatch: %d items, next=%q", len(next.Items), next.NextCursor)
	}
}

func TestListWithCursor_ForgedKindAndValueRejected(t *testing.T) {
	// Round-6 P3: the token is client-forgeable, so its kind tag is never
	// the type source of truth — the expected kind derives from the field's
	// schema. A forged "str" on an int column previously rode into the
	// row-value comparison as a mistyped parameter.
	s := setupCursorTieStore(t)
	for i, grp := range []string{"a", "b", "c"} {
		if err := s.Create(context.Background(), &CursorTie{Grp: grp, Pri: int8(i)}); err != nil {
			t.Fatal(err)
		}
	}

	forge := func(t *testing.T, tok cursorToken) string {
		t.Helper()
		raw, err := json.Marshal(tok)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}

	// Kind forged to string on an int field → schema pin rejects.
	forged := forge(t, cursorToken{V: cursorTokenVersion, Field: "pri", Dir: string(where.CursorAfter), Kind: cursorKindString, Value: "abc", RID: "cti_x"})
	_, err := s.ListWithCursor(context.Background(), "pri", where.CursorAfter, forged, 1)
	if !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("forged kind must be rejected as ErrInvalidArgument, got %v", err)
	}

	// Kind honest but value garbage → value parse rejects.
	forged = forge(t, cursorToken{V: cursorTokenVersion, Field: "pri", Dir: string(where.CursorAfter), Kind: cursorKindInt, Value: "abc", RID: "cti_x"})
	_, err = s.ListWithCursor(context.Background(), "pri", where.CursorAfter, forged, 1)
	if !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("forged value must be rejected as ErrInvalidArgument, got %v", err)
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

// Arch-backlog #16 regression tests: cursor size discipline. The decode
// side bounds the client-supplied token BEFORE any base64/JSON work; the
// encode side refuses over-long string boundaries — and any assembled
// token past the decode bound — instead of signing a token the framework
// itself rejects, or silently truncating the value.

// CursorWide carries an unconstrained TEXT column so boundary values can
// legitimately reach and exceed MaxCursorValueLen.
type CursorWide struct {
	db.Model
	Val string `json:"val" gorm:"not null"`
}

func (CursorWide) RIDPrefix() string { return "cwd" }

func setupCursorWideStore(t *testing.T) *Store[CursorWide] {
	t.Helper()
	gdb := setupDB(t)
	if err := gdb.Migrate(context.Background(), db.Table(&CursorWide{})); err != nil {
		t.Fatal(err)
	}
	return New[CursorWide](gdb, log.Empty(), WithQueryFields("val"))
}

func TestListWithCursor_OversizedTokenRejectedBeforeDecode(t *testing.T) {
	s := setupCursorWideStore(t)

	_, err := s.ListWithCursor(context.Background(), "val", where.CursorAfter,
		strings.Repeat("A", MaxCursorTokenLen+1), 1)
	if !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("over-limit token must be ErrInvalidArgument, got %v", err)
	}
	if !strings.Contains(err.Error(), "MaxCursorTokenLen") {
		t.Fatalf("rejection must come from the length gate, got %v", err)
	}

	// Exactly at the limit the length gate passes; the token then fails as
	// ordinary garbage (still 400) — proving the bound is not off-by-one.
	_, err = s.ListWithCursor(context.Background(), "val", where.CursorAfter,
		strings.Repeat("A", MaxCursorTokenLen), 1)
	if !errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("at-limit garbage token must still be ErrInvalidArgument, got %v", err)
	}
	if strings.Contains(err.Error(), "MaxCursorTokenLen") {
		t.Fatalf("at-limit token must pass the length gate, got %v", err)
	}
}

func TestListWithCursor_OversizedStringBoundaryRefusedAtSigning(t *testing.T) {
	// The lookahead proves a next page exists, so the encoder cannot skip
	// the over-long boundary — and must not truncate it into a token that
	// scans from a wrong position. Server-side error, not client input.
	s := setupCursorWideStore(t)
	for _, v := range []string{
		"a" + strings.Repeat("x", MaxCursorValueLen), // 1 byte over the bound
		"b",
	} {
		if err := s.Create(context.Background(), &CursorWide{Val: v}); err != nil {
			t.Fatal(err)
		}
	}

	_, err := s.ListWithCursor(context.Background(), "val", where.CursorAfter, "", 1)
	if err == nil {
		t.Fatal("over-limit boundary must error, not sign or truncate")
	}
	if errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("the refusal is a server-side field-contract error, not client input: %v", err)
	}
	if !strings.Contains(err.Error(), "MaxCursorValueLen") {
		t.Fatalf("refusal must name the value bound, got %v", err)
	}
}

func TestListWithCursor_MaxLenStringBoundarySignsAndRoundTrips(t *testing.T) {
	// A boundary exactly AT MaxCursorValueLen is legitimate: it signs, the
	// token stays under MaxCursorTokenLen, and the next page consumes it.
	s := setupCursorWideStore(t)
	for _, prefix := range []string{"a", "b", "c"} {
		v := prefix + strings.Repeat("x", MaxCursorValueLen-1)
		if err := s.Create(context.Background(), &CursorWide{Val: v}); err != nil {
			t.Fatal(err)
		}
	}

	var rids []string
	cursor := ""
	for range 10 {
		page, err := s.ListWithCursor(context.Background(), "val", where.CursorAfter, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.NextCursor) > MaxCursorTokenLen {
			t.Fatalf("signed token is %d bytes, past MaxCursorTokenLen %d", len(page.NextCursor), MaxCursorTokenLen)
		}
		for _, it := range page.Items {
			rids = append(rids, it.RID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(rids) != 3 {
		t.Fatalf("at-limit boundary must round-trip all 3 rows, got %d", len(rids))
	}
}

func TestListWithCursor_EscapeInflatedTokenRefusedAtSigning(t *testing.T) {
	// A control-character string within MaxCursorValueLen still inflates
	// ~6x under JSON escaping, assembling a token past MaxCursorTokenLen.
	// decode would refuse that token, and a signed token must always be
	// decodable — so the encoder refuses to sign it in the first place.
	s := setupCursorWideStore(t)
	for _, v := range []string{
		strings.Repeat("\x01", MaxCursorValueLen), // within the repr bound
		strings.Repeat("\x02", MaxCursorValueLen),
	} {
		if err := s.Create(context.Background(), &CursorWide{Val: v}); err != nil {
			t.Fatal(err)
		}
	}

	_, err := s.ListWithCursor(context.Background(), "val", where.CursorAfter, "", 1)
	if err == nil {
		t.Fatal("escape-inflated token must be refused at signing")
	}
	if errors.Is(err, apierr.ErrInvalidArgument) {
		t.Fatalf("the refusal is a server-side condition, not client input: %v", err)
	}
	if !strings.Contains(err.Error(), "MaxCursorTokenLen") {
		t.Fatalf("refusal must name the token bound, got %v", err)
	}
}

func TestEncodeCursorValue_StringLengthBound(t *testing.T) {
	if _, _, err := encodeCursorValue(strings.Repeat("x", MaxCursorValueLen)); err != nil {
		t.Fatalf("a boundary exactly at MaxCursorValueLen must encode: %v", err)
	}
	_, _, err := encodeCursorValue(strings.Repeat("x", MaxCursorValueLen+1))
	if err == nil {
		t.Fatal("a boundary past MaxCursorValueLen must be refused")
	}
	if !strings.Contains(err.Error(), "MaxCursorValueLen") {
		t.Fatalf("refusal must name the bound, got %v", err)
	}
}
