package cache

import (
	"context"
	"sync"
	"time"

	"github.com/maypok86/otter/v2"
)

type memoryCache struct {
	c         *otter.Cache[string, []byte]
	ttl       time.Duration
	closeOnce sync.Once
}

// NewMemory creates an in-memory cache backed by otter.
func NewMemory(opts *MemoryOptions) (Cache, error) {
	c, err := otter.New[string, []byte](&otter.Options[string, []byte]{
		MaximumSize:      opts.Capacity,
		ExpiryCalculator: otter.ExpiryWriting[string, []byte](opts.TTL),
	})
	if err != nil {
		return nil, err
	}
	return &memoryCache{c: c, ttl: opts.TTL}, nil
}

func (m *memoryCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	v, ok := m.c.GetIfPresent(key)
	return v, ok, nil
}

func (m *memoryCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.c.Set(key, value)
	// Per-key TTL override. The entry was created with the default TTL
	// from ExpiryWriting; SetExpiresAfter adjusts it. There is a tiny
	// race window where the entry exists with the default TTL, but the
	// value is always correct — only the expiry may briefly differ.
	if ttl > 0 {
		m.c.SetExpiresAfter(key, ttl)
	}
	return nil
}

func (m *memoryCache) Delete(_ context.Context, key string) error {
	m.c.Invalidate(key)
	return nil
}

func (m *memoryCache) Close() error {
	m.closeOnce.Do(func() {
		m.c.StopAllGoroutines()
	})
	return nil
}
