package store

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"time"
	"unicode/utf8"

	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/v3/store/where"
)

// A cursor is base64url(JSON(cursorToken)): the keyset position — the
// order field's boundary value and the row's RID, which breaks ties — bound
// to the field and direction it was issued for, so a cursor replayed
// against another order fails as a 400 instead of scanning from a wrong
// position. Filters are not bound: a cursor reused under other filters
// grants nothing the filters do not. The token is readable and forgeable
// by the client, which is harmless (a forged boundary is no more than a
// filter) and is why the tie-break is the public RID, never the numeric
// key.

const (
	cursorVersion  = 1
	maxCursorToken = 4096 // longest token accepted, checked before decoding
	maxCursorValue = 1024 // longest string boundary a token carries
)

type cursorToken struct {
	V     int    `json:"v"`
	Field string `json:"f"`
	Desc  bool   `json:"d"`
	Kind  string `json:"k"`
	Value string `json:"x"`
	RID   string `json:"r"`
}

// The kinds of boundary value. A value must come back with its Go type: a
// timestamp or number compared as text is an error on Postgres and a
// wrong order elsewhere.
const (
	kindTime   = "time"
	kindString = "str"
	kindInt    = "int"
	kindUint   = "uint"
	kindFloat  = "float"
	kindBool   = "bool"
)

var (
	timeType   = reflect.TypeFor[time.Time]()
	valuerType = reflect.TypeFor[driver.Valuer]()
)

// fieldSpec is what a column's values look like on the wire: the kind and,
// for numbers, the bit width that bounds a decoded value. textual marks a
// column declared with a text type, which aggregates refuse to add or
// order as a number or a time.
type fieldSpec struct {
	kind    string
	bits    int
	textual bool
}

// specOf derives the wire kind of a model field by running a zero row
// through gorm's ValueOf and the same normalization the encoder applies,
// so serializer and driver.Valuer fields classify by what they write. A
// probe that yields nothing (a nil pointer, a NULL sql.Null*) falls back to
// the Go type, unless a serializer or Valuer could rewrite real values.
func specOf(modelType reflect.Type, f *schema.Field) (fieldSpec, bool) {
	if sample, ok := zeroProbe(modelType, f); ok {
		return scalarSpec(reflect.TypeOf(sample))
	}
	if f.Serializer != nil {
		return fieldSpec{}, false
	}
	t := derefType(f.FieldType)
	if t.Implements(valuerType) || reflect.PointerTo(t).Implements(valuerType) {
		return fieldSpec{}, false
	}
	return scalarSpec(t)
}

func zeroProbe(modelType reflect.Type, f *schema.Field) (sample any, ok bool) {
	defer func() {
		if recover() != nil {
			sample, ok = nil, false
		}
	}()
	v, _ := f.ValueOf(context.Background(), reflect.New(modelType).Elem())
	resolved, err := normalizeValue(v)
	if err != nil || resolved == nil {
		return nil, false
	}
	return resolved, true
}

func scalarSpec(t reflect.Type) (fieldSpec, bool) {
	if t == timeType || (t.Kind() == reflect.Struct && t.ConvertibleTo(timeType)) {
		return fieldSpec{kind: kindTime}, true
	}
	switch t.Kind() {
	case reflect.String:
		return fieldSpec{kind: kindString}, true
	case reflect.Bool:
		return fieldSpec{kind: kindBool}, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return fieldSpec{kind: kindInt, bits: t.Bits()}, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return fieldSpec{kind: kindUint, bits: t.Bits()}, true
	case reflect.Float32, reflect.Float64:
		return fieldSpec{kind: kindFloat, bits: t.Bits()}, true
	}
	return fieldSpec{}, false
}

// normalizeValue dereferences pointers and resolves driver.Valuers, to the
// value a driver would send.
func normalizeValue(v any) (any, error) {
	for depth := 0; v != nil; depth++ {
		if depth >= 32 {
			return nil, errors.New("value nests too deeply")
		}
		if valuer, ok := v.(driver.Valuer); ok {
			resolved, err := valuer.Value()
			if err != nil {
				return nil, err
			}
			v = resolved
			continue
		}
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Pointer && rv.Kind() != reflect.Interface {
			break
		}
		if rv.IsNil() {
			return nil, nil
		}
		v = rv.Elem().Interface()
	}
	return v, nil
}

// encodeValue renders a boundary value as (kind, text).
func encodeValue(v any) (kind, text string, err error) {
	resolved, err := normalizeValue(v)
	if err != nil {
		return "", "", err
	}
	if resolved == nil {
		return "", "", errors.New("the boundary value is NULL; a cursor needs a NOT NULL field")
	}
	rv := reflect.ValueOf(resolved)
	if rv.Type() == timeType || (rv.Kind() == reflect.Struct && rv.Type().ConvertibleTo(timeType)) {
		// The zone stays: SQLite compares timestamps as text in the
		// writer's zone, so a boundary rendered in another zone would
		// compare wrongly.
		b, err := rv.Convert(timeType).Interface().(time.Time).MarshalText()
		if err != nil {
			return "", "", fmt.Errorf("the boundary time is not RFC 3339: %w", err)
		}
		return kindTime, string(b), nil
	}
	switch rv.Kind() {
	case reflect.String:
		s := rv.String()
		if len(s) > maxCursorValue {
			return "", "", fmt.Errorf("the boundary string is %d bytes, over %d; order cursors by a short key", len(s), maxCursorValue)
		}
		if !utf8.ValidString(s) {
			return "", "", errors.New("the boundary string is not valid UTF-8")
		}
		return kindString, s, nil
	case reflect.Bool:
		return kindBool, strconv.FormatBool(rv.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return kindInt, strconv.FormatInt(rv.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return kindUint, strconv.FormatUint(rv.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if math.IsNaN(f) {
			return "", "", errors.New("the boundary value is NaN, which does not order")
		}
		return kindFloat, strconv.FormatFloat(f, 'g', -1, 64), nil
	}
	return "", "", fmt.Errorf("a %T cannot be a cursor boundary", v)
}

// decodeValue is encodeValue's inverse, bounded to the field's width.
func decodeValue(kind, text string, bits int) (any, error) {
	if bits == 0 {
		bits = 64
	}
	switch kind {
	case kindTime:
		return time.Parse(time.RFC3339Nano, text)
	case kindString:
		return text, nil
	case kindBool:
		return strconv.ParseBool(text)
	case kindInt:
		return strconv.ParseInt(text, 10, bits)
	case kindUint:
		return strconv.ParseUint(text, 10, bits)
	case kindFloat:
		f, err := strconv.ParseFloat(text, bits)
		if err == nil && math.IsNaN(f) {
			return nil, errors.New("NaN does not order")
		}
		return f, err
	}
	return nil, fmt.Errorf("unknown value kind %q", kind)
}

// decodeCursor checks a client's token against the request's order and
// returns its boundary. The field's spec, not the token, says what type
// the value is.
func decodeCursor(token, field string, desc bool, spec fieldSpec) (value any, rid string, err error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: invalid cursor: %s", where.ErrInvalidParam, fmt.Sprintf(format, args...))
	}
	if len(token) > maxCursorToken {
		return nil, "", bad("%d bytes, over %d", len(token), maxCursorToken)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, "", bad("not base64url")
	}
	var tok cursorToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, "", bad("malformed")
	}
	switch {
	case tok.V != cursorVersion:
		return nil, "", bad("version %d", tok.V)
	case tok.Field != field || tok.Desc != desc:
		return nil, "", bad("issued for another order than %s", orderText(field, desc))
	case tok.RID == "":
		return nil, "", bad("no tie-break")
	case tok.Kind != spec.kind:
		return nil, "", bad("a %s value for %s", tok.Kind, field)
	}
	value, err = decodeValue(tok.Kind, tok.Value, spec.bits)
	if err != nil {
		return nil, "", bad("%v", err)
	}
	return value, tok.RID, nil
}

func orderText(field string, desc bool) string {
	if desc {
		return field + ":desc"
	}
	return field + ":asc"
}

// encodeCursor builds the cursor after row: its boundary value and RID.
// It runs only when a next page exists, so failing is better than an
// empty cursor that would end the client's pagination early.
func encodeCursor(row reflect.Value, field string, desc bool, f, ridField *schema.Field, spec fieldSpec) (string, error) {
	v, _ := f.ValueOf(context.Background(), row)
	r, _ := ridField.ValueOf(context.Background(), row)
	rid, _ := r.(string)
	if rid == "" {
		return "", errors.New("store: ListWithCursor: the boundary row has no RID")
	}
	kind, text, err := encodeValue(v)
	if err != nil {
		return "", fmt.Errorf("store: ListWithCursor: %s: %w", field, err)
	}
	if kind != spec.kind {
		return "", fmt.Errorf("store: ListWithCursor: %s wrote a %s value where its type promises %s", field, kind, spec.kind)
	}
	if _, err := decodeValue(kind, text, spec.bits); err != nil {
		return "", fmt.Errorf("store: ListWithCursor: %s: %w", field, err)
	}
	b, err := json.Marshal(cursorToken{V: cursorVersion, Field: field, Desc: desc, Kind: kind, Value: text, RID: rid})
	if err != nil {
		return "", fmt.Errorf("store: ListWithCursor: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	if len(token) > maxCursorToken {
		return "", fmt.Errorf("store: ListWithCursor: %s makes a %d-byte cursor, over %d; order cursors by a short key", field, len(token), maxCursorToken)
	}
	return token, nil
}
