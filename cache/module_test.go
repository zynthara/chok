package cache_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/zynthara/chok/v2/cache"
	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/redis"
)

func TestModule_MemoryOnly_RoundTrip(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
cache:
  memory:
    enabled: true
`, cache.Module())

	cc, ok := kernel.Get[*cache.Component](tk, "cache")
	if !ok {
		t.Fatal("cache component not visible")
	}
	c := cc.Cache()
	if c == nil {
		t.Fatal("expected a non-nil cache with the memory layer enabled")
	}
	ctx := context.Background()
	if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	data, hit, err := c.Get(ctx, "k")
	if err != nil || !hit || string(data) != "v" {
		t.Fatalf("Get = %q, %v, %v; want hit \"v\"", data, hit, err)
	}
}

func TestModule_MemoryAndRedisChain(t *testing.T) {
	mr := miniredis.RunT(t)
	tk := choktest.NewTestKernel(t, fmt.Sprintf(`
redis:
  addr: %s
cache:
  memory:
    enabled: true
  redis:
    enabled: true
`, mr.Addr()), redis.Module(), cache.Module())

	cc, _ := kernel.Get[*cache.Component](tk, "cache")
	c := cc.Cache()
	ctx := context.Background()
	if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	// Write-through must land in the shared redis, proving the layer
	// rides the redis module's client.
	if got, err := mr.Get("k"); err != nil || got != "v" {
		t.Fatalf("miniredis GET = %q, %v; want write-through value", got, err)
	}
}

func TestModule_NoLayers_NilCacheIsValid(t *testing.T) {
	tk := choktest.NewTestKernel(t, "", cache.Module())

	cc, ok := kernel.Get[*cache.Component](tk, "cache")
	if !ok {
		t.Fatal("component should be up with zero layers")
	}
	if cc.Cache() != nil {
		t.Fatal("expected nil cache with no layers enabled")
	}
}

func TestModule_RedisLayerWithoutRedisModule_FailsStartup(t *testing.T) {
	_, err := choktest.StartKernel(t, `
cache:
  redis:
    enabled: true
`, cache.Module())
	if err == nil {
		t.Fatal("expected startup failure: cache.redis enabled without the redis module")
	}
	if !strings.Contains(err.Error(), "redis module") {
		t.Fatalf("error should point at the missing redis module, got: %v", err)
	}
}

func TestModule_RedisLayerWithDisabledRedis_FailsStartup(t *testing.T) {
	_, err := choktest.StartKernel(t, `
redis:
  enabled: false
cache:
  redis:
    enabled: true
`, redis.Module(), cache.Module())
	if err == nil {
		t.Fatal("expected startup failure: cache.redis enabled while redis is disabled")
	}
}

func TestModule_BreakerWithoutRedisLayer_RejectedByValidate(t *testing.T) {
	_, err := choktest.StartKernel(t, `
cache:
  breaker:
    enabled: true
`, cache.Module())
	if err == nil {
		t.Fatal("expected validation failure: breaker without redis layer")
	}
	if !strings.Contains(err.Error(), "breaker") {
		t.Fatalf("error should mention the breaker, got: %v", err)
	}
}

func TestModule_BreakerDegradesRedisFailuresToMisses(t *testing.T) {
	mr := miniredis.RunT(t)
	tk := choktest.NewTestKernel(t, fmt.Sprintf(`
redis:
  addr: %s
cache:
  redis:
    enabled: true
  breaker:
    enabled: true
    threshold: 1
`, mr.Addr()), redis.Module(), cache.Module())

	cc, _ := kernel.Get[*cache.Component](tk, "cache")
	c := cc.Cache()
	ctx := context.Background()
	if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}

	// Kill the backend: the breaker absorbs the failure and the cache
	// degrades to misses instead of surfacing errors on the hot path.
	mr.Close()
	if _, hit, err := c.Get(ctx, "k"); err != nil || hit {
		t.Fatalf("first Get after backend death = hit=%v err=%v; want silent miss", hit, err)
	}
	if _, hit, err := c.Get(ctx, "k"); err != nil || hit {
		t.Fatalf("breaker-open Get = hit=%v err=%v; want silent miss", hit, err)
	}
}
