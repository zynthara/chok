package cache

import (
	"context"
	"errors"
	"time"
)

type chainCache struct {
	layers []Cache
}

// Chain composes multiple caches into a multi-level chain.
//
// Get: tries L0, L1, ... in order. On first hit at layer N, backfills layers
// 0..N-1 (with TTL=0, relying on each layer's own eviction policy).
//
// Set/Delete: applied to all layers.
//
// An empty chain (no layers) is valid: Get always misses, Set/Delete are no-ops.
func Chain(caches ...Cache) Cache {
	return &chainCache{layers: caches}
}

func (c *chainCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	for i, layer := range c.layers {
		data, ok, err := layer.Get(ctx, key)
		if err != nil {
			return nil, false, err
		}
		if ok {
			// Backfill upper layers.
			for j := 0; j < i; j++ {
				_ = c.layers[j].Set(ctx, key, data, 0)
			}
			return data, true, nil
		}
	}
	return nil, false, nil
}

func (c *chainCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	var errs []error
	for _, layer := range c.layers {
		if err := layer.Set(ctx, key, value, ttl); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *chainCache) Delete(ctx context.Context, key string) error {
	var errs []error
	for _, layer := range c.layers {
		if err := layer.Delete(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *chainCache) Close() error {
	var errs []error
	for _, layer := range c.layers {
		if err := layer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
