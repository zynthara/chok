package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestBuild_MemoryOnly(t *testing.T) {
	c, err := Build(BuildOptions{
		Memory: &MemoryOptions{Capacity: 100, TTL: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()
	c.Set(ctx, "k", []byte("v"), time.Minute)
	data, ok, _ := c.Get(ctx, "k")
	if !ok || string(data) != "v" {
		t.Fatal("expected hit")
	}
}

func TestBuild_MemoryAndRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	c, err := Build(BuildOptions{
		Memory:   &MemoryOptions{Capacity: 100, TTL: time.Minute},
		Redis:    client,
		RedisTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()
	c.Set(ctx, "k", []byte("v"), time.Minute)
	data, ok, _ := c.Get(ctx, "k")
	if !ok || string(data) != "v" {
		t.Fatal("expected hit")
	}
	// Write-through must reach the redis layer, not just memory.
	if got, err := client.Get(ctx, "k").Result(); err != nil || got != "v" {
		t.Fatalf("redis layer GET = %q, %v; want write-through value", got, err)
	}
}

func TestBuild_RedisTTLOverridesLayerDefault(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	c, err := Build(BuildOptions{Redis: client, RedisTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()
	// ttl=0 means "layer default", which Build set to one minute.
	if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	ttl := mr.TTL("k")
	if ttl <= 0 || ttl > time.Minute {
		t.Fatalf("expected layer-default TTL ≈1m on the redis key, got %s", ttl)
	}
}

func TestBuild_NoLayers(t *testing.T) {
	c, err := Build(BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Fatal("expected nil cache with no layers")
	}
}
