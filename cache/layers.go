package cache

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/maypok86/otter/v2"
	"github.com/redis/go-redis/v9"
)

// memory is the in-process layer, on otter: size-bounded, every entry
// with its own TTL.
type memory struct {
	c   *otter.Cache[string, entry]
	ttl time.Duration
}

// entry is a value with the TTL it was stored for.
type entry struct {
	val []byte
	ttl time.Duration
}

func newMemory(capacity int, ttl time.Duration) (*memory, error) {
	c, err := otter.New(&otter.Options[string, entry]{
		MaximumSize: capacity,
		ExpiryCalculator: otter.ExpiryWritingFunc(func(e otter.Entry[string, entry]) time.Duration {
			return e.Value.ttl
		}),
	})
	if err != nil {
		return nil, err
	}
	return &memory{c: c, ttl: ttl}, nil
}

func (m *memory) get(_ context.Context, key string) ([]byte, bool, error) {
	e, ok := m.c.GetIfPresent(key)
	if !ok {
		return nil, false, nil
	}
	return bytes.Clone(e.val), true, nil
}

func (m *memory) set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	if ttl == 0 {
		ttl = m.ttl
	}
	m.c.Set(key, entry{val: bytes.Clone(val), ttl: ttl})
	return nil
}

func (m *memory) del(_ context.Context, key string) error {
	m.c.Invalidate(key)
	return nil
}

func (m *memory) close() { m.c.StopAllGoroutines() }

// redisLayer is the shared layer.
type redisLayer struct {
	client *redis.Client
	ttl    time.Duration
}

func (r *redisLayer) get(ctx context.Context, key string) ([]byte, bool, error) {
	v, err := r.client.Get(ctx, key).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return v, true, nil
}

// set stores val for ttl; 0 is the layer's TTL, never "no expiry".
func (r *redisLayer) set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if ttl == 0 {
		ttl = r.ttl
	}
	return r.client.Set(ctx, key, val, ttl).Err()
}

func (r *redisLayer) del(ctx context.Context, key string) error {
	return r.client.Del(ctx, key).Err()
}
