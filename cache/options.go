package cache

import (
	"fmt"
	"time"
)

// Options is the "cache" yaml section. Layer topology cannot be
// re-plumbed under live traffic, so every field is restart-only
// (untagged = restart, the conservative conf default).
//
// v1 note: the memory/file layers lived in separate cache.memory /
// cache.file sections and the redis layer attached implicitly whenever
// a redis component existed. v2 folds everything into one section,
// drops the badger file layer (SPEC §2.4) and makes the redis layer an
// explicit switch — configuration drives behaviour, and an enabled
// redis layer with no redis module is a fail-fast assembly error, not
// a silent downgrade.
type Options struct {
	Enabled bool                `mapstructure:"enabled" default:"true"`
	Memory  MemoryLayerOptions  `mapstructure:"memory"`
	Redis   RedisLayerOptions   `mapstructure:"redis"`
	Breaker BreakerLayerOptions `mapstructure:"breaker"`
}

// MemoryLayerOptions configures the in-process otter layer.
type MemoryLayerOptions struct {
	Enabled  bool          `mapstructure:"enabled"  default:"false"`
	Capacity int           `mapstructure:"capacity" default:"10000"`
	TTL      time.Duration `mapstructure:"ttl"      default:"5m"`
}

// RedisLayerOptions configures the shared-redis layer. Enabling it
// requires the redis module in the assembly.
type RedisLayerOptions struct {
	Enabled bool          `mapstructure:"enabled" default:"false"`
	TTL     time.Duration `mapstructure:"ttl"     default:"10m"`
}

// BreakerLayerOptions wraps the redis layer in a circuit breaker so a
// down Redis degrades to cache misses instead of stretching every
// request to the client timeout.
type BreakerLayerOptions struct {
	Enabled           bool          `mapstructure:"enabled"             default:"false"`
	Threshold         int           `mapstructure:"threshold"           default:"5"`
	ResetTimeout      time.Duration `mapstructure:"reset_timeout"       default:"30s"`
	HalfOpenSuccesses int           `mapstructure:"half_open_successes" default:"3"`
	ProbeTimeout      time.Duration `mapstructure:"probe_timeout"       default:"2s"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Memory.Enabled {
		if o.Memory.Capacity <= 0 {
			return fmt.Errorf("cache: memory.capacity must be positive when enabled")
		}
		if o.Memory.TTL < 0 {
			return fmt.Errorf("cache: memory.ttl must not be negative")
		}
	}
	if o.Redis.Enabled && o.Redis.TTL < 0 {
		return fmt.Errorf("cache: redis.ttl must not be negative")
	}
	if o.Breaker.Enabled {
		if !o.Redis.Enabled {
			return fmt.Errorf("cache: breaker wraps the redis layer; enable cache.redis or disable cache.breaker")
		}
		if o.Breaker.Threshold <= 0 {
			return fmt.Errorf("cache: breaker.threshold must be positive")
		}
		if o.Breaker.ResetTimeout <= 0 {
			return fmt.Errorf("cache: breaker.reset_timeout must be positive")
		}
		if o.Breaker.HalfOpenSuccesses <= 0 {
			return fmt.Errorf("cache: breaker.half_open_successes must be positive")
		}
		if o.Breaker.ProbeTimeout <= 0 {
			return fmt.Errorf("cache: breaker.probe_timeout must be positive")
		}
	}
	return nil
}
