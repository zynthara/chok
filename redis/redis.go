package redis

import (
	"github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/config"
)

// New creates a Redis client from config options.
// Note: the returned client is lazily connected — connection errors will
// surface on first use. Use RedisComponent.Health() or an explicit Ping
// for startup verification.
//
// Network timeouts fall through to go-redis library defaults only when
// the config value is zero; applications that explicitly want unbounded
// waits must set a negative value (go-redis convention).
func New(opts *config.RedisOptions) (*redis.Client, error) {
	// Callers that construct RedisOptions in code (tests, custom config
	// structs) may skip the viper-default layer. Match the declared
	// default="10" here so PoolSize is never implicitly left as
	// go-redis' own default (10 * GOMAXPROCS — wildly different from
	// the documented value).
	poolSize := opts.PoolSize
	if poolSize <= 0 {
		poolSize = 10
	}
	client := redis.NewClient(&redis.Options{
		Addr:         opts.Addr,
		Password:     opts.Password,
		DB:           opts.DB,
		DialTimeout:  opts.DialTimeout,
		ReadTimeout:  opts.ReadTimeout,
		WriteTimeout: opts.WriteTimeout,
		PoolTimeout:  opts.PoolTimeout,
		PoolSize:     poolSize,
	})
	return client, nil
}
