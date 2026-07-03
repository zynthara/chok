package cache

import (
	"time"

	"github.com/redis/go-redis/v9"
)

// BuildOptions configures which cache layers to create.
// Nil or zero-value options skip that layer.
//
// The badger file layer was removed in v2 (SPEC §2.4): the layer
// composition below (memory → Redis, chained) is the extension slot —
// a future persistent layer slots back in as another Cache in the
// chain, without a heavyweight dependency riding every build.
type BuildOptions struct {
	Memory   *MemoryOptions  // nil = skip memory layer
	Redis    *redis.Client   // nil = skip Redis layer
	RedisTTL time.Duration   // 0 = the redis layer's default (10m)
	Breaker  *BreakerOptions // nil = no circuit breaker on Redis layer
}

// Build creates a multi-level Cache from the given options.
// Layers are added in order: memory → Redis.
// Only enabled layers (non-nil options) are included.
// Returns nil if no layers are configured.
func Build(opts BuildOptions) (Cache, error) {
	var layers []Cache

	if opts.Memory != nil && opts.Memory.Capacity > 0 {
		m, err := NewMemory(opts.Memory)
		if err != nil {
			return nil, err
		}
		layers = append(layers, m)
	}

	if opts.Redis != nil {
		var rc Cache
		if opts.RedisTTL > 0 {
			rc = NewRedisWithTTL(opts.Redis, opts.RedisTTL)
		} else {
			rc = NewRedis(opts.Redis)
		}
		if opts.Breaker != nil {
			rc = WithBreaker(rc, *opts.Breaker)
		}
		layers = append(layers, rc)
	}

	if len(layers) == 0 {
		return nil, nil
	}
	if len(layers) == 1 {
		return layers[0], nil
	}
	return Chain(layers...), nil
}
