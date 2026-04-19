package cache

import (
	"github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/log"
)

// BuildOptions configures which cache layers to create.
// Nil or zero-value options skip that layer.
type BuildOptions struct {
	Memory  *MemoryOptions  // nil = skip memory layer
	File    *FileOptions    // nil = skip file layer
	Redis   *redis.Client   // nil = skip Redis layer
	Breaker *BreakerOptions // nil = no circuit breaker on Redis layer
	Logger  log.Logger      // optional, used for file cache (badger) logging
}

// Build creates a multi-level Cache from the given options.
// Layers are added in order: memory → file → Redis.
// Only enabled layers (non-nil options) are included.
// Returns nil if no layers are configured.
func Build(opts BuildOptions) (Cache, error) {
	var layers []Cache

	// closeLayers cleans up already-created layers on partial failure.
	closeLayers := func() {
		for _, l := range layers {
			_ = l.Close()
		}
	}

	if opts.Memory != nil && opts.Memory.Capacity > 0 {
		m, err := NewMemory(opts.Memory)
		if err != nil {
			return nil, err
		}
		layers = append(layers, m)
	}

	if opts.File != nil && opts.File.Path != "" {
		args := []log.Logger{}
		if opts.Logger != nil {
			args = append(args, opts.Logger)
		}
		f, err := NewFile(opts.File, args...)
		if err != nil {
			closeLayers()
			return nil, err
		}
		layers = append(layers, f)
	}

	if opts.Redis != nil {
		var rc Cache = NewRedis(opts.Redis)
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
