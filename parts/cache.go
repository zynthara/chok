package parts

import (
	"context"
	"fmt"

	"github.com/zynthara/chok/cache"
	"github.com/zynthara/chok/component"
)

// CacheBuilder constructs the application's Cache, typically by calling
// cache.Build with memory/file options and an optional *redis.Client
// retrieved via k.Get("redis").
type CacheBuilder func(k component.Kernel) (cache.Cache, error)

// CacheComponent owns the shared cache.Cache instance and manages its
// lifecycle. It depends on RedisComponent so that a builder needing
// the Redis client can trust it's already initialised.
//
// Returning a nil Cache from the builder is valid: the user has no
// cache configured. Calls to Cache() then return nil and consumers
// should handle that as "cache disabled".
//
// WithPreBuilt(existing) skips the builder and adopts an externally
// constructed cache (see chok.App's legacy initCache path). In that
// mode Close becomes a no-op by default since the App registers its
// own cleanup for the cache; use WithPreBuilt(existing, owned=true)
// to transfer ownership to the component.
type CacheComponent struct {
	build    CacheBuilder
	hardDeps []string
	optDeps  []string
	c        cache.Cache

	preBuilt      cache.Cache
	preBuiltOwned bool
}

// NewCacheComponent wires the builder. Redis is declared as an optional
// dependency: when a RedisComponent is registered, it is Init'd before
// Cache so builders can safely call k.Get("redis"); when Redis is not
// registered, the Cache still initializes (memory/file-only mode).
//
// To adjust dependencies explicitly, use WithHardDependencies
// (replaces hard deps only) or WithoutOptionalDependencies (clears
// the auto-redis optional).
func NewCacheComponent(build CacheBuilder) *CacheComponent {
	return &CacheComponent{
		build:   build,
		optDeps: []string{"redis"},
	}
}

// WithDependencies replaces BOTH hard and optional dependency lists
// with the supplied hard deps. Calling it with no arguments drops
// every dependency, including the default redis optional — a
// potentially surprising shortcut kept for backward compatibility.
// New code should prefer WithHardDependencies /
// WithoutOptionalDependencies for clarity.
func (c *CacheComponent) WithDependencies(deps ...string) *CacheComponent {
	c.optDeps = nil
	c.hardDeps = deps
	return c
}

// WithHardDependencies replaces the hard dependency list without
// touching the optional list. Use when the builder unconditionally
// requires a specific component (e.g. a custom kv store) while still
// preferring redis when available.
func (c *CacheComponent) WithHardDependencies(deps ...string) *CacheComponent {
	c.hardDeps = deps
	return c
}

// WithoutOptionalDependencies clears the optional dependency list
// (the default redis auto-wait). Use for pre-built caches or
// deployments that never involve a RedisComponent so Cache Init does
// not wait for an absent sibling's phase.
func (c *CacheComponent) WithoutOptionalDependencies() *CacheComponent {
	c.optDeps = nil
	return c
}

// WithPreBuilt binds an existing cache.Cache so Init doesn't call the
// builder. When owned is true, the component will Close the cache on
// shutdown; when false, the caller (typically chok.App's AddCleanup
// path) retains ownership and the component's Close is a no-op.
func (c *CacheComponent) WithPreBuilt(existing cache.Cache, owned bool) *CacheComponent {
	c.preBuilt = existing
	c.preBuiltOwned = owned
	return c
}

// Name implements component.Component.
func (c *CacheComponent) Name() string { return "cache" }

// ConfigKey implements component.Component.
func (c *CacheComponent) ConfigKey() string { return "cache" }

// Dependencies implements component.Dependent (hard dependencies).
func (c *CacheComponent) Dependencies() []string { return c.hardDeps }

// OptionalDependencies implements component.OptionalDependent.
// Redis is optional by default: when registered it is Init'd first;
// when absent, cache initializes without it (memory/file only).
func (c *CacheComponent) OptionalDependencies() []string { return c.optDeps }

// Init runs the builder, unless WithPreBuilt supplied an existing cache
// in which case Init adopts it. A nil cache (from either path) is valid
// and represents "cache disabled".
func (c *CacheComponent) Init(ctx context.Context, k component.Kernel) error {
	if c.preBuilt != nil {
		c.c = c.preBuilt
		return nil
	}
	cc, err := c.build(k)
	if err != nil {
		return fmt.Errorf("cache init: %w", err)
	}
	c.c = cc
	return nil
}

// Close releases cache resources. Idempotent. For pre-built caches
// where ownership was not transferred (WithPreBuilt owned=false),
// Close is a no-op — the caller is expected to close the cache.
func (c *CacheComponent) Close(ctx context.Context) error {
	if c.c == nil {
		return nil
	}
	if c.preBuilt != nil && !c.preBuiltOwned {
		return nil
	}
	return c.c.Close()
}

// Health returns OK whether cache is disabled or functioning. A cache
// probe would require a test key write/read per poll, which is more
// expensive than /healthz warrants; monitor via the hit-rate metric
// instead.
func (c *CacheComponent) Health(ctx context.Context) component.HealthStatus {
	return component.HealthStatus{Status: component.HealthOK}
}

// Cache returns the built cache, or nil when disabled / uninitialised.
func (c *CacheComponent) Cache() cache.Cache { return c.c }
