package store

import (
	"errors"
	"regexp"

	"github.com/zynthara/chok/apierr"
)

// MapError maps store sentinel errors to *apierr.Error.
// Returns nil for unrecognized errors.
//
// Only client-visible errors are mapped. Programming errors
// (ErrMissingConditions, ErrMissingColumns) are intentionally excluded —
// they are server-side bugs and should surface as 500, not mislead the
// client with a 400.
//
// Usage: register in the application's Setup callback:
//
//	apierr.RegisterMapper(store.MapError)
func MapError(err error) *apierr.Error {
	switch {
	case errors.Is(err, ErrNotFound):
		return apierr.ErrNotFound
	case errors.Is(err, ErrStaleVersion):
		return apierr.ErrConflict.WithMessage("resource version conflict")
	case errors.Is(err, ErrDegenerateConditions):
		return apierr.ErrInvalidArgument.WithMessage("filter matches nothing; at least one filter value is required")
	case errors.Is(err, ErrDuplicate):
		// Surface only the constraint name, not the full driver error:
		// the raw message typically contains the offending value
		// (`"alice@example.com"`) plus SQL snippets that leak schema
		// layout. Clients get enough to branch on (the constraint that
		// failed), nothing more.
		var dup *DuplicateEntryError
		if errors.As(err, &dup) && dup.Detail != "" {
			if constraint := extractConstraintName(dup.Detail); constraint != "" {
				return apierr.ErrConflict.
					WithMessage("duplicate entry").
					WithMetadata("constraint", constraint)
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
