package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type redisCache struct {
	client *redis.Client
}

// NewRedis creates a Redis-backed cache.
// The client lifecycle is managed externally (e.g. via chok.App.AddCleanup);
// Close() on this cache is a no-op.
func NewRedis(client *redis.Client) Cache {
	return &redisCache{client: client}
}

func (r *redisCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	data, err := r.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (r *redisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r *redisCache) Delete(ctx context.Context, key string) error {
	return r.client.Del(ctx, key).Err()
}

func (r *redisCache) Close() error {
	return nil // client lifecycle managed externally
}
