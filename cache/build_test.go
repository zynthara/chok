package cache

import (
	"context"
	"testing"
	"time"
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

func TestBuild_MemoryAndFile(t *testing.T) {
	c, err := Build(BuildOptions{
		Memory: &MemoryOptions{Capacity: 100, TTL: time.Minute},
		File:   &FileOptions{Path: t.TempDir(), TTL: time.Hour},
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

func TestBuild_NoLayers(t *testing.T) {
	c, err := Build(BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Fatal("expected nil cache with no layers")
	}
}
