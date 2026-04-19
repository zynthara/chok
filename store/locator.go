package store

import (
	"fmt"

	"gorm.io/gorm"

	"github.com/zynthara/chok/store/where"
)

// Locator identifies which record(s) a Get/Update/Delete operation targets.
// It is the "who" axis of the CRUD matrix, orthogonal to Changes ("what") and
// UpdateOption/DeleteOption ("how").
//
// The three built-in locators cover all common cases:
//
//   - RID(rid) — external contract, safe to expose to clients
//   - ID(id)   — internal numeric PK, for cross-table joins and batch work
//   - Where(opts...) — arbitrary conditions via the where DSL (whitelist-enforced)
//
// Locator.apply runs after scopes (OwnerScope, etc.) are applied, so the final
// WHERE clause is the intersection of scopes and locator conditions.
type Locator interface {
	// apply adds WHERE conditions to db. queryFieldMap is the Store's query
	// whitelist; RID/ID locators ignore it, Where uses it to validate fields.
	apply(db *gorm.DB, queryFieldMap map[string]string) (*gorm.DB, error)
}

// RID returns a Locator matching records by their public resource ID (rid column).
// This is the default locator for HTTP-facing operations.
func RID(id string) Locator {
	return ridLocator(id)
}

// ID returns a Locator matching records by their internal numeric primary key.
// Intended for server-side cross-table lookups where foreign keys are numeric.
// Do not expose the numeric ID to external clients.
func ID(id uint) Locator {
	return idLocator(id)
}

// Where returns a Locator that applies arbitrary where.Options.
// Field references are validated against the Store's query whitelist.
// Useful for batch operations (e.g. Delete all expired tokens) and complex
// single-record lookups (e.g. Get by email).
func Where(opts ...where.Option) Locator {
	return whereLocator(opts)
}

type ridLocator string

func (r ridLocator) apply(db *gorm.DB, _ map[string]string) (*gorm.DB, error) {
	return db.Where("rid = ?", string(r)), nil
}

func (r ridLocator) String() string { return fmt.Sprintf("rid:%s", string(r)) }

type idLocator uint

func (i idLocator) apply(db *gorm.DB, _ map[string]string) (*gorm.DB, error) {
	return db.Where("id = ?", uint(i)), nil
}

// String intentionally omits the numeric value so error messages — which
// may be logged or (via apierr.Mapper mis-registration) surfaced to
// clients — don't leak internal primary keys. Callers that need the value
// for server-side diagnostics can type-assert: if l, ok := by.(idLocator);
// ok { log("id", uint(l)) }.
func (i idLocator) String() string { return "id" }

type whereLocator []where.Option

func (w whereLocator) String() string { return "where" }

func (w whereLocator) apply(db *gorm.DB, fm map[string]string) (*gorm.DB, error) {
	q, cfg, err := where.Apply(db, fm, []where.Option(w))
	if err != nil {
		return nil, err
	}
	// Safety guard: Where locator used for Get/Update/Delete must carry a
	// filter condition. Pagination/ordering alone on these operations would
	// silently touch the first-or-all rows, which is never what the caller
	// wants. Use store.Create + store.List for whole-table operations.
	if !cfg.HasFilter {
		return nil, ErrMissingConditions
	}
	// DegenerateFilter means the filter collapsed to match-nothing (e.g.
	// WithFilterIn with an empty slice, WithFilter("x", nil)). Get would
	// see 0 rows and return NotFound naturally; Update/Delete would look
	// like a successful 0-rows-affected, masking a caller mistake. We
	// cannot distinguish the operation here, so reject defensively.
	// Using a distinct error lets callers tell "empty input" apart from
	// "forgot to set any filter at all" (ErrMissingConditions).
	if cfg.DegenerateFilter {
		return nil, ErrDegenerateConditions
	}
	return q, nil
}

// locatorString returns a human-readable representation of a Locator for
// use in error messages and diagnostics. Falls back to a type description
// when the locator doesn't implement fmt.Stringer.
func locatorString(by Locator) string {
	if s, ok := by.(fmt.Stringer); ok {
		return s.String()
	}
	return fmt.Sprintf("%T", by)
}

// newNotFoundError builds a NotFoundError with redaction-safe Locator
// plus the original RID / ID value preserved in typed fields for callers
// who use errors.As to recover diagnostic info server-side. HasRID /
// HasID distinguish "locator was RID/ID" from zero-value ambiguity.
func newNotFoundError(by Locator) *NotFoundError {
	e := &NotFoundError{Locator: locatorString(by)}
	switch v := by.(type) {
	case ridLocator:
		e.RID = string(v)
		e.HasRID = true
	case idLocator:
		e.IDValue = uint(v)
		e.HasID = true
	}
	return e
}

// newVersionConflictError mirrors newNotFoundError for stale-version
// errors so callers can recover the targeted key alongside the version
// that was rejected.
func newVersionConflictError(by Locator, version int) *VersionConflictError {
	e := &VersionConflictError{Locator: locatorString(by), Version: version}
	switch v := by.(type) {
	case ridLocator:
		e.RID = string(v)
		e.HasRID = true
	case idLocator:
		e.IDValue = uint(v)
		e.HasID = true
	}
	return e
}
