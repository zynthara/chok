package redis

import (
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/config"
)

// New creates a Redis client from config options.
func New(opts *config.RedisOptions) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     opts.Addr,
		Password: opts.Password,
		DB:       opts.DB,
	})
	if client == nil {
		return nil, fmt.Errorf("redis: failed to create client")
	}
	return client, nil
}
