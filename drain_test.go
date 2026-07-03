package chok

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/web"
)

func drainSnapshot(t *testing.T, yaml string) *conf.Snapshot {
	t.Helper()
	loader := conf.NewLoader("draintest", "DRAINTEST")
	if err := loader.Register("http", web.Options{}); err != nil {
		t.Fatal(err)
	}
	if yaml != "" {
		dir := t.TempDir()
		path := filepath.Join(dir, "draintest.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		loader.SetPath(path)
	}
	store, err := conf.NewStore(loader)
	if err != nil {
		t.Fatal(err)
	}
	return store.Snapshot()
}

// TestDrainDelay_InheritsFromHTTPSection pins the SPEC §9 wiring:
// yaml http.drain_delay feeds the kernel draining phase when
// WithDrainDelay was not set (v1 autoRegisterHTTP inheritance).
func TestDrainDelay_InheritsFromHTTPSection(t *testing.T) {
	comps := []kernel.Component{web.Module()}

	// Default: the section's default 5s applies.
	snap := drainSnapshot(t, "")
	if got := inheritDrainDelay(0, comps, snap); got != 5*time.Second {
		t.Fatalf("default inheritance = %s, want 5s", got)
	}

	// Explicit yaml value wins over the default.
	snap = drainSnapshot(t, "http:\n  drain_delay: 9s\n")
	if got := inheritDrainDelay(0, comps, snap); got != 9*time.Second {
		t.Fatalf("yaml inheritance = %s, want 9s", got)
	}

	// WithDrainDelay (explicit) beats the section value.
	if got := inheritDrainDelay(2*time.Second, comps, snap); got != 2*time.Second {
		t.Fatalf("explicit option must win, got %s", got)
	}

	// No http-owning module assembled ⇒ nothing to inherit.
	if got := inheritDrainDelay(0, nil, snap); got != 0 {
		t.Fatalf("no http module ⇒ 0, got %s", got)
	}

	// Disabled http section ⇒ nothing to inherit.
	snap = drainSnapshot(t, "http:\n  enabled: false\n  drain_delay: 9s\n")
	if got := inheritDrainDelay(0, comps, snap); got != 0 {
		t.Fatalf("disabled http ⇒ 0, got %s", got)
	}
}
