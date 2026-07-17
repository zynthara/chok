package store

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"time"
	"unicode/utf8"

	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/v2/store/where"
)

// The opaque cursor behind ListWithCursor.
//
// A cursor is base64url(JSON(cursorToken)): the keyset position — the sort
// field's typed boundary value plus the public RID tie-breaker — bound to
// the pagination contract it was issued under (format version, field,
// direction). Decode rejects a token replayed against a different field,
// direction or format version, so a contract change surfaces as an
// invalid-argument error instead of a silently wrong scan position.
//
// Deliberately NOT bound: filters. Reusing a cursor under different filter
// options grants no capability beyond what those filters already grant —
// scopes still apply — so binding them would add token bulk without a
// security payoff. Keeping filters stable across pages is the caller's
// side of the contract.
//
// base64 is encoding, not encryption or integrity: the payload is
// client-readable and forgeable. That is acceptable because forging a
// boundary value is no more powerful than requesting it via filters, and
// it is exactly why the tie-breaker is the public RID — the internal
// numeric key must never ride a client-visible token.

// cursorTokenVersion is the opaque-cursor format version. Bump when the
// token layout changes; decode rejects other versions so stale cursors
// fail loudly instead of mis-paginating.
const cursorTokenVersion = 1

// Cursor size discipline — both bounds are public contract:
//
//   - MaxCursorTokenLen caps the opaque token ListWithCursor accepts from
//     clients. The check runs before base64/JSON work, so an arbitrarily
//     long token costs its length check, not a proportional decode
//     allocation. Over-limit tokens are apierr.ErrInvalidArgument (400).
//   - MaxCursorValueLen caps the string representation of a boundary
//     value the encoder will sign. Only string fields can exceed it (every
//     other kind renders a few dozen bytes at most); an over-limit
//     boundary is refused as a server-side field-contract error — the
//     framework never silently truncates a value into a token that would
//     scan the next page from a wrong position.
//
// The two interlock: a signed token must stay decodable, so after
// assembly the encoder also refuses any token longer than
// MaxCursorTokenLen (JSON escaping can inflate a near-limit boundary —
// control characters expand up to six bytes each — past what the repr
// bound alone guarantees).
const (
	MaxCursorTokenLen = 4096
	MaxCursorValueLen = 1024
)

type cursorToken struct {
	V     int    `json:"v"`
	Field string `json:"f"`
	Dir   string `json:"d"`
	Kind  string `json:"k"`
	Value string `json:"x"`
	RID   string `json:"r"`
}

// Cursor value kinds. The boundary value must round-trip with its Go type
// intact — handing a stringified timestamp or number back to the row-value
// comparison would make the database compare text against a typed column
// (an error on Postgres, silently wrong ordering elsewhere).
const (
	cursorKindTime   = "time"
	cursorKindString = "str"
	cursorKindInt    = "int"
	cursorKindUint   = "uint"
	cursorKindFloat  = "float"
	cursorKindBool   = "bool"
)

var (
	cursorTimeType   = reflect.TypeOf(time.Time{})
	cursorValuerType = reflect.TypeOf((*driver.Valuer)(nil)).Elem()
)

// encodeCursorValue splits a boundary value into (kind, repr). The value is
// normalised first — pointers dereferenced, driver.Valuer resolved (the
// normalizeConflictValue helper BatchUpsert already uses) — and then matched
// by reflect.Kind, so defined scalar types (status enums, time.Duration),
// non-nil pointer fields and Valuer types (UUID, decimal) all encode. An
// error means the value genuinely cannot ride a cursor: a NULL boundary or
// an unsupported shape. Callers must surface it — a page that provably has
// a successor (the size+1 lookahead saw it) must never swallow the failure
// into an empty NextCursor, silently ending the client's pagination.
func encodeCursorValue(val any) (kind, repr string, err error) {
	resolved, err := normalizeConflictValue(val)
	if err != nil {
		return "", "", fmt.Errorf("normalize cursor value: %w", err)
	}
	if resolved == nil {
		return "", "", fmt.Errorf("cursor boundary value is NULL; cursor fields must be NOT NULL")
	}
	rv := reflect.ValueOf(resolved)
	if rv.Type() == cursorTimeType || (rv.Kind() == reflect.Struct && rv.Type().ConvertibleTo(cursorTimeType)) {
		// Preserve the zone (RFC3339 carries the offset): SQLite stores
		// timestamps as text in the writer's zone and compares them
		// lexicographically, so a UTC-normalised boundary value would
		// render differently from the stored rows and mis-compare.
		// Postgres compares the instant, for which the offset is neutral.
		t := rv.Convert(cursorTimeType).Interface().(time.Time)
		encoded, err := t.MarshalText()
		if err != nil {
			// Format would silently emit timestamps that Parse cannot consume
			// (for example a five-digit year or a sub-minute zone offset).
			// MarshalText applies RFC3339's strict representability checks, so
			// the encoder never signs a time token its decoder rejects.
			return "", "", fmt.Errorf("cursor boundary time is not RFC3339-representable: %w", err)
		}
		return cursorKindTime, string(encoded), nil
	}
	switch rv.Kind() {
	case reflect.String:
		s := rv.String()
		// The length bound comes first: it is the cheap check, and it spares
		// the UTF-8 scan from walking a value that is already refused.
		// Truncating instead would sign a token that decodes to a DIFFERENT
		// value and scans the next page from a wrong position.
		if len(s) > MaxCursorValueLen {
			return "", "", fmt.Errorf("cursor boundary string is %d bytes, exceeding MaxCursorValueLen %d; cursor fields must be short scalar keys", len(s), MaxCursorValueLen)
		}
		// json.Marshal replaces invalid UTF-8 with U+FFFD without erroring,
		// so a token signed over such a boundary would decode to a DIFFERENT
		// value and scan the next page from a wrong position — the silent
		// variant of "signing what the decoder cannot consume". SQLite TEXT
		// can genuinely hold such bytes; refuse at the source like NaN.
		// Every other kind's repr is ASCII by construction.
		if !utf8.ValidString(s) {
			return "", "", fmt.Errorf("cursor boundary string is not valid UTF-8; it cannot ride a JSON token without silent mutation")
		}
		return cursorKindString, s, nil
	case reflect.Bool:
		return cursorKindBool, strconv.FormatBool(rv.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return cursorKindInt, strconv.FormatInt(rv.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return cursorKindUint, strconv.FormatUint(rv.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		// The decoder rejects NaN as unorderable, so signing one would
		// issue a token the next page cannot consume. Refuse at the source.
		if math.IsNaN(f) {
			return "", "", fmt.Errorf("cursor boundary value is NaN, which is not orderable")
		}
		return cursorKindFloat, strconv.FormatFloat(f, 'g', -1, 64), nil
	}
	return "", "", fmt.Errorf("cursor boundary value of type %T is not a supported scalar", val)
}

// cursorFieldSpec pins what a cursor field's token must carry: the value
// kind plus the integer/float bit width used for range validation. bits 0
// means no numeric width applies; decode defaults it to 64 defensively.
type cursorFieldSpec struct {
	kind string
	bits int
}

// cursorSpecForSchemaField derives the token expectation for a schema
// field by running the SAME pipeline the encoder runs on real rows: a
// zero-value row through GORM's Field.ValueOf — which wraps serializer
// fields into driver.Valuers — and the encoder's normalization. Anything
// less lets the framework issue tokens its own decoder rejects; two
// in-tree traps prove it: gorm.io/datatypes.Time (Go int64, wire string)
// and `serializer:unixtime` (Go int64, wire time.Time).
//
// When the zero probe is inconclusive — a nil sample (sql.Null* zero
// values are NULL, pointer fields are nil), an error or a panic — falling
// back to the raw Go type is safe only when nothing rewrites values on
// the way out: with a serializer or Valuer in play the encoder WILL
// transform real values, so such fields are rejected outright rather
// than pinned to a kind the issued tokens would contradict. ListWithCursor
// surfaces the rejection up front — signing a token the next page cannot
// consume, or skipping forged-kind validation, are both worse than a loud
// configuration error.
func cursorSpecForSchemaField(modelType reflect.Type, field *schema.Field) (cursorFieldSpec, bool) {
	if sample, ok := cursorZeroProbe(modelType, field); ok {
		// The sample IS the wire shape; a non-scalar sample (e.g. a JSON
		// serializer's []byte) rejects the field rather than falling back.
		return cursorScalarSpec(reflect.TypeOf(sample))
	}
	if field.Serializer != nil {
		return cursorFieldSpec{}, false
	}
	t := field.FieldType
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Implements(cursorValuerType) || reflect.PointerTo(t).Implements(cursorValuerType) {
		return cursorFieldSpec{}, false
	}
	return cursorScalarSpec(t)
}

// cursorZeroProbe runs a zero-value row through Field.ValueOf and the
// encoder's normalization, returning the wire sample. Defensive: exotic
// Valuer or serializer implementations may error — or panic — on zero
// receivers, all of which mean "not statically derivable".
func cursorZeroProbe(modelType reflect.Type, field *schema.Field) (sample any, ok bool) {
	defer func() {
		if recover() != nil {
			sample, ok = nil, false
		}
	}()
	zeroRow := reflect.New(modelType).Elem()
	val, _ := field.ValueOf(context.Background(), zeroRow)
	resolved, err := normalizeConflictValue(val)
	if err != nil || resolved == nil {
		return nil, false
	}
	return resolved, true
}

// cursorScalarSpec classifies a plain scalar type into its token kind and
// declared bit width.
func cursorScalarSpec(t reflect.Type) (cursorFieldSpec, bool) {
	if t == cursorTimeType || (t.Kind() == reflect.Struct && t.ConvertibleTo(cursorTimeType)) {
		return cursorFieldSpec{kind: cursorKindTime}, true
	}
	switch t.Kind() {
	case reflect.String:
		return cursorFieldSpec{kind: cursorKindString}, true
	case reflect.Bool:
		return cursorFieldSpec{kind: cursorKindBool}, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return cursorFieldSpec{kind: cursorKindInt, bits: t.Bits()}, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return cursorFieldSpec{kind: cursorKindUint, bits: t.Bits()}, true
	case reflect.Float32, reflect.Float64:
		return cursorFieldSpec{kind: cursorKindFloat, bits: t.Bits()}, true
	}
	return cursorFieldSpec{}, false
}

// decodeCursorValue is encodeCursorValue's inverse: it rebuilds the typed
// boundary value the SQL comparison needs. bits is the field's declared
// integer/float width (0 = unknown, validate at 64) — a forged value that
// parses as int64 but overflows an int8 column would otherwise surface as
// a driver/database conversion failure (a 500) instead of a clean 400.
func decodeCursorValue(kind, repr string, bits int) (any, error) {
	if bits == 0 {
		bits = 64
	}
	switch kind {
	case cursorKindTime:
		v, err := time.Parse(time.RFC3339Nano, repr)
		if err != nil {
			return nil, fmt.Errorf("bad time value: %w", err)
		}
		return v, nil
	case cursorKindString:
		return repr, nil
	case cursorKindBool:
		v, err := strconv.ParseBool(repr)
		if err != nil {
			return nil, fmt.Errorf("bad bool value: %w", err)
		}
		return v, nil
	case cursorKindInt:
		v, err := strconv.ParseInt(repr, 10, bits)
		if err != nil {
			return nil, fmt.Errorf("bad int value: %w", err)
		}
		return v, nil
	case cursorKindUint:
		v, err := strconv.ParseUint(repr, 10, bits)
		if err != nil {
			return nil, fmt.Errorf("bad uint value: %w", err)
		}
		return v, nil
	case cursorKindFloat:
		v, err := strconv.ParseFloat(repr, bits)
		if err != nil {
			return nil, fmt.Errorf("bad float value: %w", err)
		}
		// NaN never orders — a keyset predicate against it is undefined
		// everywhere. ±Inf stays: it is comparable, and a float8 column can
		// genuinely hold it (the encoder round-trips it).
		if math.IsNaN(v) {
			return nil, fmt.Errorf("bad float value: NaN is not orderable")
		}
		return v, nil
	}
	return nil, fmt.Errorf("unknown value kind %q", kind)
}

// encodeCursor renders the opaque token string.
func encodeCursor(field string, direction where.CursorDirection, kind, value, rid string) string {
	payload, err := json.Marshal(cursorToken{
		V:     cursorTokenVersion,
		Field: field,
		Dir:   string(direction),
		Kind:  kind,
		Value: value,
		RID:   rid,
	})
	if err != nil {
		// Marshalling a flat struct of strings/int cannot fail; treat a
		// failure as "no cursor" rather than panicking a read path.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// decodeCursor parses and validates an opaque token against the request's
// field and direction. spec is the field's schema-derived token
// expectation (ListWithCursor rejects fields without one up front): the
// token is client-forgeable, so its own kind tag is never the source of
// type truth — a forged "str" on an integer column would otherwise ride
// into the row-value comparison as a mistyped parameter (an error on
// Postgres, silently wrong ordering elsewhere) — and the value is
// range-validated at the field's declared width. Every failure wraps
// where.ErrInvalidParam — cursors come from clients, and mapQueryError
// turns this into the invalid-argument 400 the rest of the query surface
// produces.
func decodeCursor(token, field string, direction where.CursorDirection, spec cursorFieldSpec) (fieldValue any, rid string, err error) {
	// Length gate BEFORE any decoding: the token is client-supplied, and
	// base64+JSON work on an unbounded input is allocation the server owes
	// nobody. Legitimate tokens cannot exceed this — the encoder refuses to
	// sign past the same bound.
	if len(token) > MaxCursorTokenLen {
		return nil, "", fmt.Errorf("%w: invalid cursor: token length %d exceeds MaxCursorTokenLen %d", where.ErrInvalidParam, len(token), MaxCursorTokenLen)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, "", fmt.Errorf("%w: invalid cursor: not base64url", where.ErrInvalidParam)
	}
	var tok cursorToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, "", fmt.Errorf("%w: invalid cursor: malformed payload", where.ErrInvalidParam)
	}
	if tok.V != cursorTokenVersion {
		return nil, "", fmt.Errorf("%w: invalid cursor: unsupported version %d", where.ErrInvalidParam, tok.V)
	}
	if tok.Field != field {
		return nil, "", fmt.Errorf("%w: invalid cursor: issued for field %q, request paginates %q", where.ErrInvalidParam, tok.Field, field)
	}
	if tok.Dir != string(direction) {
		return nil, "", fmt.Errorf("%w: invalid cursor: issued for direction %q, request scans %q", where.ErrInvalidParam, tok.Dir, direction)
	}
	if tok.RID == "" {
		return nil, "", fmt.Errorf("%w: invalid cursor: missing tie-breaker", where.ErrInvalidParam)
	}
	if tok.Kind != spec.kind {
		return nil, "", fmt.Errorf("%w: invalid cursor: value kind %q does not match field %q (%s expected)", where.ErrInvalidParam, tok.Kind, field, spec.kind)
	}
	value, err := decodeCursorValue(tok.Kind, tok.Value, spec.bits)
	if err != nil {
		return nil, "", fmt.Errorf("%w: invalid cursor: %v", where.ErrInvalidParam, err)
	}
	return value, tok.RID, nil
}

// encodeItemCursor builds the NextCursor token from the last item included
// in a page: the sort field's boundary value plus the row's public RID.
// It is only called when the size+1 lookahead has PROVEN a next page
// exists, so every failure is returned as an error — an empty NextCursor
// here would tell the client "done" while rows remain. The failures are
// server-side conditions (a NULL boundary value, a field type the token
// cannot carry, a model without RID), not client input, so they surface
// as plain 500-class errors.
func (s *Store[T]) encodeItemCursor(item T, field string, direction where.CursorDirection, spec cursorFieldSpec) (string, error) {
	col, err := where.ResolveField(s.queryFieldMap, field)
	if err != nil {
		return "", fmt.Errorf("store: ListWithCursor: resolve cursor field %q: %w", field, err)
	}
	if s.modelSchema == nil {
		return "", fmt.Errorf("store: ListWithCursor: model schema unavailable")
	}
	fieldSchema := s.modelSchema.LookUpField(col)
	ridSchema := s.modelSchema.LookUpField("RID")
	if fieldSchema == nil || ridSchema == nil {
		return "", fmt.Errorf("store: ListWithCursor: cursor field %q or RID missing from model schema", field)
	}
	rv := reflect.ValueOf(item)
	val, _ := fieldSchema.ValueOf(context.Background(), rv)
	ridVal, _ := ridSchema.ValueOf(context.Background(), rv)
	rid, _ := ridVal.(string)
	if rid == "" {
		return "", fmt.Errorf("store: ListWithCursor: boundary row carries no RID")
	}
	kind, repr, err := encodeCursorValue(val)
	if err != nil {
		return "", fmt.Errorf("store: ListWithCursor: cursor field %q: %w", field, err)
	}
	// A zero-value probe can observe a Valuer/serializer's usual wire
	// shape, but driver.Valuer does not promise that the concrete Value type
	// stays constant for every row. Refuse an actual value that drifts from
	// the schema pin before signing it; otherwise the next request would
	// reject the framework's own token. Re-decoding also catches same-kind
	// drift outside the sampled width/domain (for example int8 → int64(300)).
	if kind != spec.kind {
		return "", fmt.Errorf("store: ListWithCursor: cursor field %q wire kind changed from zero-probe %q to boundary value %q; serializer/driver.Valuer cursor fields must keep a stable wire kind", field, spec.kind, kind)
	}
	if _, err := decodeCursorValue(kind, repr, spec.bits); err != nil {
		return "", fmt.Errorf("store: ListWithCursor: cursor field %q encoded a boundary outside its schema-derived cursor domain: %w", field, err)
	}
	token := encodeCursor(field, direction, kind, repr, rid)
	// The repr bound alone does not guarantee the assembled token fits:
	// JSON escaping inflates control-character-heavy strings up to six
	// bytes per input byte, so a boundary near MaxCursorValueLen can render
	// a token past MaxCursorTokenLen. decode refuses such a token, and a
	// signed token must always be decodable — refuse at the source.
	if len(token) > MaxCursorTokenLen {
		return "", fmt.Errorf("store: ListWithCursor: cursor field %q produced a %d-byte token exceeding MaxCursorTokenLen %d (JSON escaping inflated the boundary value); pick a shorter cursor key", field, len(token), MaxCursorTokenLen)
	}
	return token, nil
}
