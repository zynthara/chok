// Package cache provides a pluggable caching layer with memory, file, and Redis
// backends. Backends can be composed into a multi-level chain via [Chain].
package cache

import (
	"context"
	"time"

	"golang.org/x/sync/singleflight"
)

// defaultSF is the package-level singleflight group used by GetOrLoad.
// Callers that need isolation across Cache instances should use separate
// key prefixes. A single group is acceptable for most use cases because
// the singleflight key includes the caller's chosen cache key.
var defaultSF singleflight.Group

// Cache is the unified caching interface.
// All implementations are safe for concurrent use.
type Cache interface {
	// Get retrieves a value by key. Returns (nil, false, nil) on cache miss.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores a value with the given TTL. TTL=0 means "use the backend's
	// configured default TTL" (from MemoryOptions.TTL, FileOptions.TTL, etc.).
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Delete removes a key. Missing keys are a no-op (not an error).
	Delete(ctx context.Context, key string) error
	// Close releases resources. Idempotent.
	Close() error
}

// Loader is called by GetOrLoad on a cache miss. It returns the value to
// cache and an error. Concurrent calls for the same key share a single
// loader invocation (singleflight).
type Loader func(ctx context.Context) ([]byte, error)

// GetOrLoad retrieves a value by key from the cache. On miss, calls
// loader to populate the cache, using singleflight to ensure only one
// concurrent loader runs per key (preventing cache stampede / thundering
// herd). The result is stored with the given TTL.
//
// This is a standalone function rather than an interface method so that
// existing Cache implementations don't need updating. It works with any
// Cache.
func GetOrLoad(ctx context.Context, c Cache, key string, loader Loader, ttl time.Duration) ([]byte, error) {
	data, ok, err := c.Get(ctx, key)
	if err == nil && ok {
		return data, nil
	}
	val, err, _ := defaultSF.Do(key, func() (any, error) {
		// Double-check after winning the singleflight race.
		if data, ok, err := c.Get(ctx, key); err == nil && ok {
			return data, nil
		}
		result, err := loader(ctx)
		if err != nil {
			return nil, err
		}
		_ = c.Set(ctx, key, result, ttl)
		return result, nil
	})
	if err != nil {
		return nil, err
	}
	return val.([]byte), nil
}

// MemoryOptions configures the in-memory cache (otter).
type MemoryOptions struct {
	Capacity int           // maximum number of entries
	TTL      time.Duration // default TTL for entries
}

// FileOptions configures the file-based cache (badger).
type FileOptions struct {
	Path string        // directory path for badger data files
	TTL  time.Duration // default TTL for entries
}
