// Package cache provides a pluggable caching layer with memory, file, and Redis
// backends. Backends can be composed into a multi-level chain via [Chain].
package cache

import (
	"context"
	"time"
)

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
