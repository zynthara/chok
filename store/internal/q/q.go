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

// Column resolves a public field name to its column.
func Column(fields map[string]string, field string) (string, error) {
	col, ok := fields[field]
	if !ok {
		return "", fmt.Errorf("%w %q", ErrUnknownField, field)
	}
	if !SafeIdent(col) {
		return "", fmt.Errorf("%w %q: column %q is not a plain identifier", ErrUnknownField, field, col)
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
