package store

import (
	"errors"

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
	case errors.Is(err, ErrDuplicate):
		return apierr.ErrConflict.WithMessage("duplicate entry")
	}
	return nil
}
