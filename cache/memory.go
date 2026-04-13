package cache

import (
	"context"
	"time"

	"github.com/maypok86/otter/v2"
)

type memoryCache struct {
	c   *otter.Cache[string, []byte]
	ttl time.Duration
}

// NewMemory creates an in-memory cache backed by otter.
func NewMemory(opts *MemoryOptions) (Cache, error) {
	c, err := otter.New[string, []byte](&otter.Options[string, []byte]{
		MaximumSize: opts.Capacity,
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
	m.c.StopAllGoroutines()
	return nil
}
