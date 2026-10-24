package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/v3/internal/conf"
)

// Cache is the cache battery: the layers Config enables, memory first,
// behind one key-value interface. Register it with app.Use: it reports
// Redis's health and closes the Redis client.
type Cache struct {
	logger  *slog.Logger
	layers  []layer // memory, then redis (behind the breaker when it is on)
	memory  *memory
	client  *redis.Client // nil without the redis layer
	breaker bool
	flight  flight

	closeOnce sync.Once
	closeErr  error
}

// layer is one level of the cache.
type layer interface {
	get(ctx context.Context, key string) ([]byte, bool, error)
	set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	del(ctx context.Context, key string) error
}

// Option configures New.
type Option func(*options)

type options struct {
	logger *slog.Logger
}

// Logger is where the cache reports, such as the breaker opening; the
// default is slog.Default().
func Logger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// New builds the layers cfg enables. A Config with neither the memory
// nor the redis layer on is an error: a Cache that caches nothing is a
// mistake, not a mode. The Redis client connects on first use; New does
// not reach the server, Health does.
func New(cfg Config, opts ...Option) (*Cache, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	cfg = cfg.withDefaults()
	if err := conf.Check(&cfg); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	if !cfg.Memory.Enabled && !cfg.Redis.Enabled {
		return nil, errors.New("cache: no layer is enabled: turn on cache.memory or cache.redis, or build no Cache")
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	c := &Cache{logger: o.logger}
	var client *redis.Client
	if cfg.Redis.Enabled {
		var err error
		if client, err = newClient(cfg); err != nil {
			return nil, err
		}
	}
	if cfg.Memory.Enabled {
		m, err := newMemory(cfg.Memory.Capacity, cfg.Memory.TTL)
		if err != nil {
			if client != nil {
				_ = client.Close()
			}
			return nil, fmt.Errorf("cache: memory: %w", err)
		}
		c.memory = m
		c.layers = append(c.layers, m)
	}
	if client != nil {
		c.client = client
		var l layer = &redisLayer{client: client, ttl: cfg.Redis.TTL}
		if cfg.Breaker.Enabled {
			b := cfg.Breaker
			l = newBreaker(l, b.Threshold, b.ResetTimeout, b.HalfOpenSuccesses, b.ProbeTimeout, o.logger)
			c.breaker = true
		}
		c.layers = append(c.layers, l)
	}
	return c, nil
}

// MustNew is New that panics on error.
func MustNew(cfg Config, opts ...Option) *Cache {
	c, err := New(cfg, opts...)
	if err != nil {
		panic(err)
	}
	return c
}

// Get returns the value under key from the first layer that has it, and
// copies a value Redis had into the memory layer. A miss is (nil, false,
// nil). A layer that fails counts as a miss; the error is returned only
// when every layer failed, or when ctx has ended. The value is the
// caller's to keep and change.
func (c *Cache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var errs []error
	for i, l := range c.layers {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		v, ok, err := l.get(ctx, key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok {
			continue
		}
		if i > 0 && c.memory != nil {
			_ = c.memory.set(ctx, key, v, 0) // the memory layer's TTL
		}
		return v, true, nil
	}
	if len(errs) > 0 && len(errs) == len(c.layers) {
		return nil, false, fmt.Errorf("cache: get %q: %w", key, errors.Join(errs...))
	}
	return nil, false, nil
}

// Set stores val under key in every layer, for ttl; 0 means each layer's
// configured TTL. The cache keeps its own copy of val.
func (c *Cache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if ttl < 0 {
		return fmt.Errorf("cache: set %q: the TTL must not be negative, got %s", key, ttl)
	}
	var errs []error
	for _, l := range c.layers {
		if err := l.set(ctx, key, val, ttl); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("cache: set %q: %w", key, errors.Join(errs...))
	}
	return nil
}

// Delete removes key from every layer; a missing key is not an error.
func (c *Cache) Delete(ctx context.Context, key string) error {
	var errs []error
	for _, l := range c.layers {
		if err := l.del(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("cache: delete %q: %w", key, errors.Join(errs...))
	}
	return nil
}

// GetOrLoad returns the value under key, or calls load on a miss and
// stores what it returns for ttl (0: each layer's TTL). Concurrent
// callers missing the same key share one call of load, made with the
// context of the first; a caller whose own context ends stops waiting. A
// value that cannot be stored is still returned, and the failure logged;
// load's error is returned as it is and nothing is stored.
func (c *Cache) GetOrLoad(ctx context.Context, key string, ttl time.Duration, load func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	if load == nil {
		return nil, fmt.Errorf("cache: get or load %q: nil load function", key)
	}
	if ttl < 0 {
		return nil, fmt.Errorf("cache: get or load %q: the TTL must not be negative, got %s", key, ttl)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if v, ok, err := c.Get(ctx, key); err == nil && ok {
		return v, nil
	}
	return c.flight.do(ctx, key, func() ([]byte, error) {
		// Another caller may have stored it while this one queued.
		if v, ok, err := c.Get(ctx, key); err == nil && ok {
			return v, nil
		}
		v, err := load(ctx)
		if err != nil {
			return nil, err
		}
		if err := c.Set(ctx, key, v, ttl); err != nil {
			c.logger.WarnContext(ctx, "cache: the loaded value was not stored", "key", key, "error", err)
		}
		return v, nil
	})
}

// Redis returns the client of the redis layer, nil when the layer is
// off, for what the cache does not do (a rate limiter, a lock, rbac's
// policy watcher). It belongs to the Cache: Close closes it.
func (c *Cache) Redis() *redis.Client { return c.client }

// Health pings Redis while the redis layer runs without the breaker: then
// a Redis that is down fails every call. With the breaker, or with the
// memory layer only, the Cache is healthy: a down Redis is the degraded
// mode the breaker exists for, and its opening is logged.
func (c *Cache) Health(ctx context.Context) error {
	if c.client == nil || c.breaker {
		return nil
	}
	if err := c.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("cache: redis: %w", err)
	}
	return nil
}

// Name is "cache", its name in the health report.
func (c *Cache) Name() string { return "cache" }

// Close stops the memory layer's maintenance and closes the Redis client.
// It is idempotent.
func (c *Cache) Close(context.Context) error {
	c.closeOnce.Do(func() {
		if c.memory != nil {
			c.memory.close()
		}
		if c.client != nil {
			c.closeErr = c.client.Close()
		}
	})
	return c.closeErr
}
