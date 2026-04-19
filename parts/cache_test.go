package parts

import (
	"context"
	"testing"
	"time"

	"github.com/zynthara/chok/cache"
	"github.com/zynthara/chok/component"
)

func TestCacheComponent_Init_NilCache(t *testing.T) {
	c := NewCacheComponent(func(component.Kernel) (cache.Cache, error) {
		return nil, nil
	}).WithDependencies() // no deps for this test

	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if c.Cache() != nil {
		t.Fatal("nil builder should leave Cache() nil")
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCacheComponent_Init_BuildsMemoryCache(t *testing.T) {
	c := NewCacheComponent(func(component.Kernel) (cache.Cache, error) {
		return cache.NewMemory(&cache.MemoryOptions{Capacity: 100, TTL: time.Minute})
	}).WithDependencies()

	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if c.Cache() == nil {
		t.Fatal("Cache() should not be nil")
	}

	if err := c.Cache().Set(context.Background(), "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Cache().Get(context.Background(), "k")
	if err != nil || !ok || string(got) != "v" {
		t.Fatalf("roundtrip failed: got=%q ok=%v err=%v", got, ok, err)
	}

	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCacheComponent_DeclaresRedisOptionalDependencyByDefault(t *testing.T) {
	c := NewCacheComponent(func(component.Kernel) (cache.Cache, error) { return nil, nil })
	// Redis should be an optional dependency, not a hard one.
	hardDeps := c.Dependencies()
	if len(hardDeps) != 0 {
		t.Fatalf("default hard deps should be empty, got %v", hardDeps)
	}
	optDeps := c.OptionalDependencies()
	if len(optDeps) != 1 || optDeps[0] != "redis" {
		t.Fatalf("default optional deps should be [\"redis\"], got %v", optDeps)
	}
}

func TestCacheComponent_Health_AlwaysOK(t *testing.T) {
	c := NewCacheComponent(func(component.Kernel) (cache.Cache, error) { return nil, nil }).WithDependencies()
	_ = c.Init(context.Background(), newMockKernel(nil))
	if c.Health(context.Background()).Status != component.HealthOK {
		t.Fatal("cache health should always be OK")
	}
}
