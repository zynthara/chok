package cache

import (
	"context"
	"testing"
	"time"
)

func TestFile_GetSetDelete(t *testing.T) {
	c, err := NewFile(&FileOptions{Path: t.TempDir(), TTL: time.Minute})
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

func TestFile_PersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Write and close.
	c1, err := NewFile(&FileOptions{Path: dir, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c1.Set(ctx, "persist", []byte("data"), time.Hour)
	c1.Close()

	// Reopen and read.
	c2, err := NewFile(&FileOptions{Path: dir, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	data, ok, err := c2.Get(ctx, "persist")
	if err != nil || !ok {
		t.Fatal("expected data to persist across reopen")
	}
	if string(data) != "data" {
		t.Fatalf("got %q, want data", data)
	}
}

func TestFile_TTLExpiry(t *testing.T) {
	c, err := NewFile(&FileOptions{Path: t.TempDir(), TTL: 100 * time.Millisecond})
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
