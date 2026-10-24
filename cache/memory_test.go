package cache

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestMemory_GetSetDelete(t *testing.T) {
	m, err := newMemory(100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	ctx := t.Context()
	if _, ok, err := m.get(ctx, "k1"); err != nil || ok {
		t.Fatal("expected miss")
	}
	if err := m.set(ctx, "k1", []byte("v1"), time.Minute); err != nil {
		t.Fatal(err)
	}
	data, ok, err := m.get(ctx, "k1")
	if err != nil || !ok || string(data) != "v1" {
		t.Fatalf("expected hit v1, got %q %v %v", data, ok, err)
	}
	if err := m.del(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.get(ctx, "k1"); ok {
		t.Fatal("expected miss after delete")
	}
}

// TestMemory_TTLExpiry: an entry lives for the TTL it was set with, the
// layer's when Set is given 0.
func TestMemory_TTLExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, err := newMemory(100, 100*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		defer m.close()
		ctx := t.Context()
		_ = m.set(ctx, "default", []byte("v"), 0)
		_ = m.set(ctx, "short", []byte("v"), 100*time.Millisecond)
		_ = m.set(ctx, "long", []byte("v"), time.Hour)
		time.Sleep(300 * time.Millisecond)
		for key, want := range map[string]bool{"default": false, "short": false, "long": true} {
			if _, ok, _ := m.get(ctx, key); ok != want {
				t.Fatalf("%s after 300ms: present %v, want %v", key, ok, want)
			}
		}
	})
}
