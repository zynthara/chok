package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"

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

// encodeCursorValue splits a boundary value into (kind, repr). ok is false
// for unsupported types — notably NULLable boundary values — in which case
// the page simply carries no NextCursor.
func encodeCursorValue(val any) (kind, repr string, ok bool) {
	switch v := val.(type) {
	case time.Time:
		// Preserve the zone (RFC3339 carries the offset): SQLite stores
		// timestamps as text in the writer's zone and compares them
		// lexicographically, so a UTC-normalised boundary value would
		// render differently from the stored rows and mis-compare.
		// Postgres compares the instant, for which the offset is neutral.
		return cursorKindTime, v.Format(time.RFC3339Nano), true
	case string:
		return cursorKindString, v, true
	case bool:
		return cursorKindBool, strconv.FormatBool(v), true
	case int, int8, int16, int32, int64:
		return cursorKindInt, strconv.FormatInt(reflect.ValueOf(v).Int(), 10), true
	case uint, uint8, uint16, uint32, uint64:
		return cursorKindUint, strconv.FormatUint(reflect.ValueOf(v).Uint(), 10), true
	case float32, float64:
		return cursorKindFloat, strconv.FormatFloat(reflect.ValueOf(v).Float(), 'g', -1, 64), true
	}
	return "", "", false
}

// decodeCursorValue is encodeCursorValue's inverse: it rebuilds the typed
// boundary value the SQL comparison needs.
func decodeCursorValue(kind, repr string) (any, error) {
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
		v, err := strconv.ParseInt(repr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad int value: %w", err)
		}
		return v, nil
	case cursorKindUint:
		v, err := strconv.ParseUint(repr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad uint value: %w", err)
		}
		return v, nil
	case cursorKindFloat:
		v, err := strconv.ParseFloat(repr, 64)
		if err != nil {
			return nil, fmt.Errorf("bad float value: %w", err)
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
// field and direction. Every failure wraps where.ErrInvalidParam — cursors
// come from clients, and mapQueryError turns this into the invalid-argument
// 400 the rest of the query surface produces.
func decodeCursor(token, field string, direction where.CursorDirection) (fieldValue any, rid string, err error) {
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
	value, err := decodeCursorValue(tok.Kind, tok.Value)
	if err != nil {
		return nil, "", fmt.Errorf("%w: invalid cursor: %v", where.ErrInvalidParam, err)
	}
	return value, tok.RID, nil
}

// encodeItemCursor builds the NextCursor token from the last item included
// in a page: the sort field's boundary value plus the row's public RID.
// Returns the empty string — "no more pages" from the client's view — when
// the boundary value's type cannot round-trip (e.g. a NULLable column);
// cursor fields should be NOT NULL.
func (s *Store[T]) encodeItemCursor(item T, field string, direction where.CursorDirection) string {
	col, err := where.ResolveField(s.queryFieldMap, field)
	if err != nil || s.modelSchema == nil {
		return ""
	}
	fieldSchema := s.modelSchema.LookUpField(col)
	ridSchema := s.modelSchema.LookUpField("RID")
	if fieldSchema == nil || ridSchema == nil {
		return ""
	}
	rv := reflect.ValueOf(item)
	val, _ := fieldSchema.ValueOf(context.Background(), rv)
	ridVal, _ := ridSchema.ValueOf(context.Background(), rv)
	rid, _ := ridVal.(string)
	if rid == "" {
		return ""
	}
	kind, repr, ok := encodeCursorValue(val)
	if !ok {
		return ""
	}
	return encodeCursor(field, direction, kind, repr, rid)
}
