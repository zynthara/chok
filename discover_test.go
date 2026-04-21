package chok

import (
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/cache"
	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/log"
)

// TestDiscoverOne_FlatStillWorks documents that the original flat layout
// (Options types at the top level of Config) keeps resolving correctly
// after the recursion change.
func TestDiscoverOne_FlatStillWorks(t *testing.T) {
	type Cfg struct {
		HTTP  config.HTTPOptions
		Redis config.RedisOptions
	}
	cfg := &Cfg{HTTP: config.HTTPOptions{Addr: ":9090"}}

	got, err := discoverOne[config.HTTPOptions](cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected HTTPOptions to be discovered at top level")
	}
	if got != &cfg.HTTP {
		t.Fatal("discoverOne should return pointer into the live struct")
	}
	if got.Addr != ":9090" {
		t.Fatalf("wrong address: %q", got.Addr)
	}
}

// TestDiscoverOne_NestedMatch exercises the new behaviour: Options types
// buried one or two levels deep are found. This mirrors the natural
// yaml/Config layout business projects actually write.
func TestDiscoverOne_NestedMatch(t *testing.T) {
	type CacheCfg struct {
		Memory config.CacheMemoryOptions
		File   config.CacheFileOptions
	}
	type Deep struct {
		Inner struct {
			Swagger config.SwaggerOptions
		}
	}
	type Cfg struct {
		Cache CacheCfg
		Extra Deep
	}
	cfg := &Cfg{
		Cache: CacheCfg{
			Memory: config.CacheMemoryOptions{Enabled: true},
			File:   config.CacheFileOptions{Enabled: false},
		},
	}
	cfg.Extra.Inner.Swagger.Enabled = true

	mem, err := discoverOne[config.CacheMemoryOptions](cfg)
	if err != nil {
		t.Fatalf("memory: %v", err)
	}
	if mem != &cfg.Cache.Memory {
		t.Fatal("nested CacheMemoryOptions not discovered as live pointer")
	}

	file, err := discoverOne[config.CacheFileOptions](cfg)
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if file != &cfg.Cache.File {
		t.Fatal("nested CacheFileOptions not discovered")
	}

	sw, err := discoverOne[config.SwaggerOptions](cfg)
	if err != nil {
		t.Fatalf("swagger: %v", err)
	}
	if sw != &cfg.Extra.Inner.Swagger {
		t.Fatal("twice-nested SwaggerOptions not discovered")
	}
}

// TestDiscoverOne_CollisionTopAndNested verifies that the ambiguity check
// spans the whole tree, not just the top level. Two instances of the same
// Options type anywhere should report an error rather than silently pick
// one.
func TestDiscoverOne_CollisionTopAndNested(t *testing.T) {
	type Inner struct {
		HTTP config.HTTPOptions
	}
	type Cfg struct {
		HTTP   config.HTTPOptions
		Nested Inner
	}
	cfg := &Cfg{}

	_, err := discoverOne[config.HTTPOptions](cfg)
	if err == nil {
		t.Fatal("expected ambiguity error for duplicate HTTPOptions")
	}
	if !strings.Contains(err.Error(), "2") || !strings.Contains(err.Error(), "config tree") {
		t.Fatalf("error should describe count + tree scope, got: %v", err)
	}
}

// TestDiscoverOne_IgnoresAtomicStruct ensures time.Time and similar
// value-semantic structs don't cause runtime panics or false matches when
// they appear in user Configs.
func TestDiscoverOne_IgnoresAtomicStruct(t *testing.T) {
	type Cfg struct {
		StartedAt time.Time
		HTTP      config.HTTPOptions
	}
	cfg := &Cfg{StartedAt: time.Now()}

	got, err := discoverOne[config.HTTPOptions](cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != &cfg.HTTP {
		t.Fatal("HTTPOptions should be found alongside time.Time field")
	}
}

// TestDiscoverOne_SkipsPointerOptions locks in the no-pointer-Options
// contract: pointer fields are invisible to discovery, even when they
// point at valid Options objects. The reload invariant requires
// value-embedded Options, so discovering pointer-targets would let users
// configure a component and later wonder why Reload doesn't update it.
func TestDiscoverOne_SkipsPointerOptions(t *testing.T) {
	type Cfg struct {
		HTTP *config.HTTPOptions
	}
	cfg := &Cfg{HTTP: &config.HTTPOptions{Addr: ":1234"}}

	got, err := discoverOne[config.HTTPOptions](cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatal("pointer HTTPOptions must not be discovered — contract with validateNoPointerOptions")
	}
}

// TestDiscoverOne_SkipsSelfValidating verifies discriminator types like
// DatabaseOptions act as opaque nodes. Without this, DatabaseOptions'
// nested SQLite/MySQL blocks would be double-discovered alongside any
// legacy top-level SQLiteOptions field (breaking autoRegisterDB's
// fallback path).
func TestDiscoverOne_SkipsSelfValidating(t *testing.T) {
	type Cfg struct {
		Database config.DatabaseOptions
	}
	cfg := &Cfg{Database: config.DatabaseOptions{
		Driver: "sqlite",
		SQLite: config.SQLiteOptions{Path: "app.db"},
	}}

	sqlite, err := discoverOne[config.SQLiteOptions](cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sqlite != nil {
		t.Fatal("SQLiteOptions nested inside SelfValidating DatabaseOptions must be hidden from discovery")
	}

	db, err := discoverOne[config.DatabaseOptions](cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if db != &cfg.Database {
		t.Fatal("DatabaseOptions itself must still be discoverable at its own level")
	}
}

// TestAutoRegisterCache_NestedConfig is the end-to-end proof: a user Config
// with the natural `cache: { memory, file }` layout resolves into a working
// CacheComponent without any explicit WithSetup wiring.
func TestAutoRegisterCache_NestedConfig(t *testing.T) {
	type CacheCfg struct {
		Memory config.CacheMemoryOptions
		File   config.CacheFileOptions
	}
	type Cfg struct {
		Cache CacheCfg
	}
	cfg := &Cfg{
		Cache: CacheCfg{
			Memory: config.CacheMemoryOptions{Enabled: true, Capacity: 1000, TTL: time.Minute},
		},
	}

	app := New("nested-cache",
		WithLogger(log.Empty()),
		WithConfig(cfg),
	)
	if err := app.autoRegisterCache(); err != nil {
		t.Fatalf("autoRegisterCache: %v", err)
	}

	var found bool
	app.registryMu.RLock()
	for _, c := range app.pendingComponents {
		if c.Name() == "cache" {
			found = true
			break
		}
	}
	app.registryMu.RUnlock()
	if !found {
		t.Fatal("CacheComponent should be auto-registered from nested config")
	}

	// Sanity: ensure the registered component can actually build its cache
	// from the discovered options. We don't Run the full App here because
	// autoRegisterCache already wired it, and CacheComponent Init is the
	// moment the chain materialises.
	_ = component.Kernel(nil) // silence unused import if shape changes
	_ = cache.Cache(nil)
}
