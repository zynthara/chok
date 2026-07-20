package store

import (
	"errors"
	"regexp"
	"strings"

	"github.com/zynthara/chok/v2/apierr"
)

// MapError maps store sentinel errors to *apierr.Error.
// Returns nil for unrecognized errors.
//
// Only client-visible errors are mapped. Programming errors
// (ErrMissingConditions, ErrMissingColumns) are intentionally excluded —
// they are server-side bugs and should surface as 500, not mislead the
// client with a 400.
//
// Usage: register per-App via chok.WithErrorMapper:
//
//	chok.New("app", chok.WithErrorMapper(store.MapError), ...)
func MapError(err error) *apierr.Error {
	switch {
	case errors.Is(err, ErrNotFound):
		return apierr.ErrNotFound
	case errors.Is(err, ErrStaleVersion):
		return apierr.ErrConflict.WithMessage("resource version conflict")
	case errors.Is(err, ErrDegenerateConditions):
		return apierr.ErrInvalidArgument.WithMessage("filter matches nothing; at least one filter value is required")
	case errors.Is(err, ErrEmptyPatch):
		return apierr.ErrInvalidArgument.WithMessage("patch carried no updatable fields")
	case errors.Is(err, ErrDuplicate):
		// Surface only the constraint name, not the full driver error:
		// the raw message typically contains the offending value
		// (`"alice@example.com"`) plus SQL snippets that leak schema
		// layout. Clients get enough to branch on (the constraint that
		// failed), nothing more.
		//
		// A Store-declared constraint→field mapping (WithConstraintFields)
		// is preferred over the raw name: the field is API vocabulary,
		// stable across migrations, and leaks no schema naming.
		var dup *DuplicateEntryError
		if errors.As(err, &dup) {
			if dup.Field != "" {
				return apierr.ErrConflict.
					WithMessage("duplicate entry").
					WithMetadata("field", dup.Field)
			}
			if dup.Detail != "" {
				if constraint := extractConstraintName(dup.Detail); constraint != "" {
					return apierr.ErrConflict.
						WithMessage("duplicate entry").
						WithMetadata("constraint", constraint)
				}
			}
		}
		return apierr.ErrConflict.WithMessage("duplicate entry")
	}
	return nil
}

// Constraint-name patterns across major dialects.
//
//	MySQL:      Duplicate entry 'x' for key 'uk_email'
//	Postgres:   duplicate key value violates unique constraint "uk_email"
//	SQLite:     UNIQUE constraint failed: users.email, users.tenant_id
var (
	reMySQL    = regexp.MustCompile(`for key '([^']+)'`)
	rePostgres = regexp.MustCompile(`unique constraint "([^"]+)"`)
	reSQLite   = regexp.MustCompile(`UNIQUE constraint failed:\s*([^\n]+)`)
	// glebarez/sqlite appends the numeric extended result code to the
	// message, so the extracted column list ends in a parenthesised code
	// (2067 for UNIQUE). Stripped during declaration matching only — the
	// undeclared-constraint metadata keeps the raw extraction.
	reTrailingCode = regexp.MustCompile(`\s*\(\d+\)$`)
)

// extractConstraintName tries to pull the constraint identifier out of
// the driver-specific duplicate-entry message. Returns "" when no
// pattern matches — callers should fall back to a generic message.
func extractConstraintName(detail string) string {
	if m := reMySQL.FindStringSubmatch(detail); len(m) == 2 {
		return m[1]
	}
	if m := rePostgres.FindStringSubmatch(detail); len(m) == 2 {
		return m[1]
	}
	if m := reSQLite.FindStringSubmatch(detail); len(m) == 2 {
		return m[1]
	}
	return ""
}

// lookupConstraintField resolves an extracted constraint identifier
// against a WithConstraintFields declaration. Postgres reports the bare
// constraint name and matches exactly; the second probe normalises the
// dialects that qualify identifiers with the table: MySQL 8 reports keys
// as table.key, and SQLite reports the violated COLUMN list (as
// table.col, table.col — no index name at all), so table prefixes are
// stripped from every comma-separated segment and the segments rejoined
// bare (a composite column list matches a comma-joined key, in index
// column order).
func lookupConstraintField(fields map[string]string, constraint string) (string, bool) {
	if field, ok := fields[constraint]; ok {
		return field, true
	}
	constraint = reTrailingCode.ReplaceAllString(constraint, "")
	segments := strings.Split(constraint, ",")
	for i, segment := range segments {
		segment = strings.TrimSpace(segment)
		if dot := strings.LastIndex(segment, "."); dot >= 0 {
			segment = segment[dot+1:]
		}
		segments[i] = segment
	}
	field, ok := fields[strings.Join(segments, ",")]
	return field, ok
}
