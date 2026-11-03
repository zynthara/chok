// Package q holds what store and store/where share without exporting it:
// resolving a public field name to a column, safely.
package q

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownField is where.ErrUnknownField, defined here so both
// packages return the same value.
var ErrUnknownField = errors.New("where: unknown field")

// UnknownFieldError is the ErrUnknownField that Column returns: it keeps
// the field the code named, which store logs, and errors.Is matches it
// with ErrUnknownField.
type UnknownFieldError struct {
	Field string
	// Column is set when the field is known but its column is not a
	// plain identifier.
	Column string
}

func (e *UnknownFieldError) Error() string {
	if e.Column != "" {
		return fmt.Sprintf("%s %q: column %q is not a plain identifier", ErrUnknownField, e.Field, e.Column)
	}
	return fmt.Sprintf("%s %q", ErrUnknownField, e.Field)
}

func (e *UnknownFieldError) Unwrap() error { return ErrUnknownField }

// Column resolves a public field name to its column.
func Column(fields map[string]string, field string) (string, error) {
	col, ok := fields[field]
	if !ok {
		return "", &UnknownFieldError{Field: field}
	}
	if !SafeIdent(col) {
		return "", &UnknownFieldError{Field: field, Column: col}
	}
	return col, nil
}

// SafeIdent accepts a column or table.column made of [A-Za-z0-9_],
// starting with a letter or underscore: nothing that could carry SQL.
func SafeIdent(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || (p[0] >= '0' && p[0] <= '9') {
			return false
		}
		for i := 0; i < len(p); i++ {
			if c := p[i]; c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
				return false
			}
		}
	}
	return true
}
