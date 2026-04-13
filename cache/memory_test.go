package cache

import (
	"context"
	"testing"
	"time"
)

func TestMemory_GetSetDelete(t *testing.T) {
	c, err := NewMemory(&MemoryOptions{Capacity: 100, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()

	// Miss
	_, ok, err := c.Get(ctx, "k1")
	if err != nil || ok {
		t.Fatal("expected miss")
	}

	// Set + Get
	if err := c.Set(ctx, "k1", []byte("v1"), time.Minute); err != nil {
		t.Fatal(err)
	}
	data, ok, err := c.Get(ctx, "k1")
	if err != nil || !ok {
		t.Fatal("expected hit")
	}
	if string(data) != "v1" {
		t.Fatalf("got %q, want v1", data)
	}

	// Delete + Miss
	if err := c.Delete(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	_, ok, _ = c.Get(ctx, "k1")
	if ok {
		t.Fatal("expected miss after delete")
	}
}

func TestMemory_TTLExpiry(t *testing.T) {
	c, err := NewMemory(&MemoryOptions{Capacity: 100, TTL: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()

	c.Set(ctx, "k1", []byte("v1"), 100*time.Millisecond)
	time.Sleep(300 * time.Millisecond)

	_, ok, _ := c.Get(ctx, "k1")
	if ok {
		t.Fatal("expected miss after TTL expiry")
	}
}
