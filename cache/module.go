package cache

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/v2/kernel"
)

// Module returns the cache component for chok.Use. Consumers take the
// built cache through the accessor:
//
//	cc, ok := chok.Get[*cache.Component](k, "cache")
//	c := cc.Cache() // may be nil: "no layers configured" is a valid mode
//
// The redis layer rides the redis module's shared client (soft
// dependency); enabling cache.redis without assembling the redis
// module is a fail-fast startup error.
func Module() kernel.Component { return &Component{} }

// Component owns the application-wide layered Cache.
type Component struct {
	opts Options
	c    Cache
}

// Describe implements kernel.Component.
func (c *Component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "cache",
		ConfigKey: "cache",
		Options:   Options{},
		Needs: []kernel.Dep{
			{Kind: "redis", Optional: true},
			{Kind: "log", Optional: true},
		},
	}
}

// Init assembles the configured layers. Zero enabled layers is valid —
// the component stays up with a nil Cache ("cache disabled") so
// consumers can treat absence uniformly.
func (c *Component) Init(ctx context.Context, k kernel.Kernel) error {
	if err := k.Config().Section("cache", &c.opts); err != nil {
		return fmt.Errorf("cache: decode section: %w", err)
	}

	var bopts BuildOptions
	if c.opts.Memory.Enabled {
		bopts.Memory = &MemoryOptions{Capacity: c.opts.Memory.Capacity, TTL: c.opts.Memory.TTL}
	}
	if c.opts.Redis.Enabled {
		rc, ok := kernel.Get[interface{ Client() *goredis.Client }](k, "redis")
		if !ok {
			return fmt.Errorf("cache: redis layer enabled but the redis module is not assembled (chok.Use(redis.Module())) or is disabled")
		}
		client := rc.Client()
		if client == nil {
			return fmt.Errorf("cache: redis layer enabled but the redis module has no client")
		}
		bopts.Redis = client
		bopts.RedisTTL = c.opts.Redis.TTL
		if c.opts.Breaker.Enabled {
			bopts.Breaker = &BreakerOptions{
				Threshold:         c.opts.Breaker.Threshold,
				ResetTimeout:      c.opts.Breaker.ResetTimeout,
				HalfOpenSuccesses: c.opts.Breaker.HalfOpenSuccesses,
				ProbeTimeout:      c.opts.Breaker.ProbeTimeout,
			}
		}
	}

	built, err := Build(bopts)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	if built == nil {
		k.Logger().Info("cache: no layers enabled, cache disabled")
	}
	c.c = built
	return nil
}

// Close releases cache resources (chain broadcast). The redis layer's
// Close is a no-op by design — the shared client belongs to the redis
// module.
func (c *Component) Close(ctx context.Context) error {
	if c.c == nil {
		return nil
	}
	return c.c.Close()
}

// Cache returns the built cache, or nil when no layers are enabled.
func (c *Component) Cache() Cache { return c.c }
