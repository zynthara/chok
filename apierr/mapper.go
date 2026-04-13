package apierr

import "sync"

// ErrorMapper maps a non-*Error to an *Error. Returns nil if unrecognized.
type ErrorMapper func(error) *Error

var (
	mapperMu sync.RWMutex
	mappers  []ErrorMapper
)

// RegisterMapper registers an error mapper. Typically called during the
// application startup phase (e.g. inside the Setup callback).
// Panics if m is nil.
//
// Thread-safe: protected by a read-write mutex. Resolve acquires a read lock,
// so concurrent requests are not blocked by each other.
func RegisterMapper(m ErrorMapper) {
	if m == nil {
		panic("apierr: mapper must not be nil")
	}
	mapperMu.Lock()
	defer mapperMu.Unlock()
	mappers = append(mappers, m)
}

// Resolve tries all registered mappers in order. Returns nil if no mapper matches.
func Resolve(err error) *Error {
	mapperMu.RLock()
	defer mapperMu.RUnlock()
	for _, m := range mappers {
		if ae := m(err); ae != nil {
			return ae
		}
	}
	return nil
}

// ResetMappersForTest clears all registered mappers.
// Exported for cross-package test cleanup only — do not use in production code.
func ResetMappersForTest() {
	mapperMu.Lock()
	defer mapperMu.Unlock()
	mappers = nil
}
