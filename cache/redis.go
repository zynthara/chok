package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// defaultRedisTTL is used when callers pass TTL=0, which in go-redis means
// "no expiration". Using a finite default prevents unbounded key growth from
// chain backfills.
const defaultRedisTTL = 10 * time.Minute

type redisCache struct {
	client     *redis.Client
	defaultTTL time.Duration
}

// NewRedis creates a Redis-backed cache.
// The client lifecycle is managed externally (e.g. via chok.App.AddCleanup);
// Close() on this cache is a no-op.
func NewRedis(client *redis.Client) Cache {
	return &redisCache{client: client, defaultTTL: defaultRedisTTL}
}

// NewRedisWithTTL creates a Redis-backed cache with a custom default TTL
// applied when callers pass TTL=0. This prevents go-redis from setting
// keys with no expiration.
func NewRedisWithTTL(client *redis.Client, defaultTTL time.Duration) Cache {
	if defaultTTL <= 0 {
		defaultTTL = defaultRedisTTL
	}
	return &redisCache{client: client, defaultTTL: defaultTTL}
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
	if ttl == 0 {
		ttl = r.defaultTTL
	}
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r *redisCache) Delete(ctx context.Context, key string) error {
	return r.client.Del(ctx, key).Err()
}

func (r *redisCache) Close() error {
	return nil // client lifecycle managed externally
}
