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
	var layerErrs []error
	for i, layer := range c.layers {
		// Short-circuit if the caller's context is already done —
		// callers distinguish cancellation from a genuine miss, otherwise
		// they'd invoke the loader against a dead context.
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		data, ok, err := layer.Get(ctx, key)
		if err != nil {
			// Layer error → treat as miss, try next layer.
			// This makes Chain resilient to transient failures in any
			// layer (e.g. Redis network timeout) without aborting the
			// entire lookup. Cancellation is handled above; any error
			// here is a genuine layer failure. Errors are collected so
			// callers can distinguish "every layer failed" (operational
			// problem worth alerting on) from "row really doesn't exist".
			layerErrs = append(layerErrs, err)
			continue
		}
		if ok {
			// Backfill upper layers using a detached context: the caller's
			// ctx may be near its deadline, and pushing a sync write into
			// Redis on a near-dead ctx tends to fail rather than help.
			// Cap the backfill with a short independent timeout so slow
			// writers can't block the response path.
			bfCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 200*time.Millisecond)
			for j := 0; j < i; j++ {
				_ = c.layers[j].Set(bfCtx, key, data, 0)
			}
			cancel()
			return data, true, nil
		}
	}
	// Every layer failed → surface a joined error rather than a silent
	// miss so the caller can decide whether to retry, fall back to the
	// upstream source, or alert. A genuine miss (some layer answered
	// "no row") still returns (nil, false, nil).
	if len(layerErrs) > 0 && len(layerErrs) == len(c.layers) {
		return nil, false, errors.Join(layerErrs...)
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
