package store

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/store/where"
)

func TestEncodeValue_RoundTripsEveryKind(t *testing.T) {
	type status string
	zone := time.FixedZone("x", -5*3600)
	for _, v := range []any{
		"s", status("draft"), true, int8(-3), int64(math.MaxInt64), uint16(9), uint64(math.MaxUint64),
		1.5, float32(0.25), math.Inf(1), math.Inf(-1),
		time.Date(2026, 1, 2, 3, 4, 5, 6, zone), new(int64(7)),
	} {
		kind, text, err := encodeValue(v)
		if err != nil {
			t.Fatalf("encode %v: %v", v, err)
		}
		got, err := decodeValue(kind, text, 64)
		if err != nil {
			t.Fatalf("decode %v (%s %q): %v", v, kind, text, err)
		}
		if tm, ok := v.(time.Time); ok {
			if g := got.(time.Time); !g.Equal(tm) || g.Format(time.RFC3339) != tm.Format(time.RFC3339) {
				t.Fatalf("time %v → %v: the instant and the zone stay", tm, g)
			}
		}
	}
}

func TestEncodeValue_RefusesWhatCannotRoundTrip(t *testing.T) {
	for name, v := range map[string]any{
		"nil":          nil,
		"nil pointer":  (*int)(nil),
		"NaN":          math.NaN(),
		"long string":  strings.Repeat("x", maxCursorValue+1),
		"invalid utf8": "\xff\xfe",
		"year 10000":   time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		"null string":  sql.NullString{},
		"slice":        []byte("x"),
	} {
		if _, _, err := encodeValue(v); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
	if _, _, err := encodeValue(strings.Repeat("x", maxCursorValue)); err != nil {
		t.Fatalf("a string at the limit encodes: %v", err)
	}
}

func TestDecodeValue_BoundsTheWidth(t *testing.T) {
	for _, c := range []struct {
		kind, text string
		bits       int
		ok         bool
	}{
		{kindInt, "127", 8, true},
		{kindInt, "128", 8, false},
		{kindUint, "-1", 64, false},
		{kindFloat, "NaN", 64, false},
		{kindFloat, "+Inf", 64, true},
		{kindFloat, "1e39", 32, false},
		{kindBool, "maybe", 0, false},
		{kindTime, "yesterday", 0, false},
		{"blob", "x", 0, false},
	} {
		if _, err := decodeValue(c.kind, c.text, c.bits); (err == nil) != c.ok {
			t.Errorf("%s %q /%d: %v", c.kind, c.text, c.bits, err)
		}
	}
}

type row struct {
	Key string
	RID string
}

func fakeField(t *testing.T, name string) *schema.Field {
	t.Helper()
	s, err := schema.Parse(&row{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	return s.LookUpField(name)
}

func TestEncodeCursor_RefusesTokensItCouldNotRead(t *testing.T) {
	spec := fieldSpec{kind: kindString}
	f := fakeField(t, "Key")
	rid := fakeField(t, "RID")
	// Control characters escape to six bytes each in JSON, so a value
	// under the length limit can still make a token over it.
	inflated := reflect.ValueOf(row{Key: strings.Repeat("\x01", maxCursorValue), RID: "r"})
	if _, err := encodeCursor(inflated, "key", false, f, rid, spec); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("inflated token: %v", err)
	}
	ok := reflect.ValueOf(row{Key: "k", RID: "r"})
	token, err := encodeCursor(ok, "key", true, f, rid, spec)
	if err != nil {
		t.Fatal(err)
	}
	v, r, err := decodeCursor(token, "key", true, spec)
	if err != nil || v != "k" || r != "r" {
		t.Fatalf("round trip: %v %q %v", v, r, err)
	}
	if _, _, err := decodeCursor(token, "key", true, fieldSpec{kind: kindInt}); !errors.Is(err, where.ErrInvalidParam) {
		t.Fatalf("the field's kind wins over the token's: %v", err)
	}
	if _, err := encodeCursor(reflect.ValueOf(row{Key: "k"}), "key", false, f, rid, spec); err == nil {
		t.Fatal("a row without a RID")
	}
	if _, err := encodeCursor(ok, "key", false, f, rid, fieldSpec{kind: kindInt}); err == nil {
		t.Fatal("a value whose kind drifted from the field's")
	}
}

// loop is a driver.Valuer that answers with itself.
type loop string

func (l loop) Value() (driver.Value, error) { return l, nil }

// TestNormalizeValue_RecursiveValuerIsBounded: a Valuer that never
// reaches a driver value fails instead of spinning.
func TestNormalizeValue_RecursiveValuerIsBounded(t *testing.T) {
	if _, err := normalizeValue(loop("x")); err == nil || !strings.Contains(err.Error(), "nests too deeply") {
		t.Fatalf("recursive Valuer: %v", err)
	}
	if v, err := normalizeValue(new(new(int64(3)))); err != nil || v != int64(3) {
		t.Fatalf("pointers resolve: %v %v", v, err)
	}
	if v, err := normalizeValue((*int)(nil)); err != nil || v != nil {
		t.Fatalf("a nil pointer is NULL: %v %v", v, err)
	}
}
