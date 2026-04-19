package cache

import (
	"context"
	"testing"
	"time"
)

func TestChain_L1MissL2Hit_BackfillsL1(t *testing.T) {
	l1, _ := NewMemory(&MemoryOptions{Capacity: 100, TTL: time.Minute})
	l2, _ := NewMemory(&MemoryOptions{Capacity: 100, TTL: time.Minute})
	defer l1.Close()
	defer l2.Close()
	ctx := context.Background()

	// Only L2 has the key.
	l2.Set(ctx, "k1", []byte("v1"), time.Minute)

	chain := Chain(l1, l2)

	data, ok, err := chain.Get(ctx, "k1")
	if err != nil || !ok {
		t.Fatal("expected hit from L2")
	}
	if string(data) != "v1" {
		t.Fatalf("got %q, want v1", data)
	}

	// L1 should now have the key (backfilled).
	data, ok, _ = l1.Get(ctx, "k1")
	if !ok {
		t.Fatal("expected L1 to be backfilled")
	}
	if string(data) != "v1" {
		t.Fatalf("L1 got %q, want v1", data)
	}
}

func TestChain_WriteThrough(t *testing.T) {
	l1, _ := NewMemory(&MemoryOptions{Capacity: 100, TTL: time.Minute})
	l2, _ := NewMemory(&MemoryOptions{Capacity: 100, TTL: time.Minute})
	defer l1.Close()
	defer l2.Close()
	ctx := context.Background()

	chain := Chain(l1, l2)
	chain.Set(ctx, "k1", []byte("v1"), time.Minute)

	// Both layers should have the value.
	for i, layer := range []Cache{l1, l2} {
		data, ok, _ := layer.Get(ctx, "k1")
		if !ok || string(data) != "v1" {
			t.Fatalf("layer %d: expected v1, got ok=%v data=%q", i, ok, data)
		}
	}
}

func TestChain_DeleteThrough(t *testing.T) {
	l1, _ := NewMemory(&MemoryOptions{Capacity: 100, TTL: time.Minute})
	l2, _ := NewMemory(&MemoryOptions{Capacity: 100, TTL: time.Minute})
	defer l1.Close()
	defer l2.Close()
	ctx := context.Background()

	chain := Chain(l1, l2)
	chain.Set(ctx, "k1", []byte("v1"), time.Minute)
	chain.Delete(ctx, "k1")

	for i, layer := range []Cache{l1, l2} {
		_, ok, _ := layer.Get(ctx, "k1")
		if ok {
			t.Fatalf("layer %d: expected miss after delete", i)
		}
	}
}

func TestChain_AllMiss(t *testing.T) {
	l1, _ := NewMemory(&MemoryOptions{Capacity: 100, TTL: time.Minute})
	l2, _ := NewMemory(&MemoryOptions{Capacity: 100, TTL: time.Minute})
	defer l1.Close()
	defer l2.Close()

	chain := Chain(l1, l2)
	_, ok, err := chain.Get(context.Background(), "nonexistent")
	if err != nil || ok {
		t.Fatal("expected miss from all layers")
	}
}

func TestChain_Empty(t *testing.T) {
	chain := Chain()
	ctx := context.Background()

	_, ok, err := chain.Get(ctx, "k1")
	if err != nil || ok {
		t.Fatal("empty chain should always miss")
	}
	if err := chain.Set(ctx, "k1", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := chain.Delete(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if err := chain.Close(); err != nil {
		t.Fatal(err)
	}
}
