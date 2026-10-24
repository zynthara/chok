package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/choktest"
	"github.com/zynthara/chok/v3/internal/conf"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newCache builds a Cache that the test closes.
func newCache(t *testing.T, cfg Config) *Cache {
	t.Helper()
	c, err := New(cfg, Logger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(t.Context()) })
	return c
}

func memoryOnly() Config {
	var cfg Config
	cfg.Memory.Enabled = true
	return cfg
}

// withRedis is cfg with the redis layer on a fresh miniredis.
func withRedis(t *testing.T, cfg Config) (Config, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	cfg.Redis.Enabled = true
	cfg.Redis.Addr = mr.Addr()
	return cfg, mr
}

// chainOf is a Cache over the given layers, the first being the memory
// layer the others' hits fill.
func chainOf(t *testing.T, layers ...*memory) *Cache {
	t.Helper()
	c := &Cache{logger: quiet(), memory: layers[0]}
	for _, l := range layers {
		c.layers = append(c.layers, l)
		t.Cleanup(l.close)
	}
	return c
}

func newMemoryLayer(t *testing.T) *memory {
	t.Helper()
	m, err := newMemory(100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestChain_L1MissL2Hit_BackfillsL1(t *testing.T) {
	l1, l2 := newMemoryLayer(t), newMemoryLayer(t)
	ctx := t.Context()
	_ = l2.set(ctx, "k1", []byte("v1"), time.Minute)
	c := chainOf(t, l1, l2)
	data, ok, err := c.Get(ctx, "k1")
	if err != nil || !ok || string(data) != "v1" {
		t.Fatalf("expected v1 from L2, got %q %v %v", data, ok, err)
	}
	if data, ok, _ := l1.get(ctx, "k1"); !ok || string(data) != "v1" {
		t.Fatalf("L1 is filled from L2: %q %v", data, ok)
	}
}

func TestChain_WriteThrough(t *testing.T) {
	l1, l2 := newMemoryLayer(t), newMemoryLayer(t)
	ctx := t.Context()
	if err := chainOf(t, l1, l2).Set(ctx, "k1", []byte("v1"), time.Minute); err != nil {
		t.Fatal(err)
	}
	for i, l := range []*memory{l1, l2} {
		if data, ok, _ := l.get(ctx, "k1"); !ok || string(data) != "v1" {
			t.Fatalf("layer %d: expected v1, got %q %v", i, data, ok)
		}
	}
}

func TestChain_DeleteThrough(t *testing.T) {
	l1, l2 := newMemoryLayer(t), newMemoryLayer(t)
	ctx := t.Context()
	c := chainOf(t, l1, l2)
	_ = c.Set(ctx, "k1", []byte("v1"), time.Minute)
	if err := c.Delete(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	for i, l := range []*memory{l1, l2} {
		if _, ok, _ := l.get(ctx, "k1"); ok {
			t.Fatalf("layer %d: expected a miss after Delete", i)
		}
	}
}

func TestChain_AllMiss(t *testing.T) {
	c := chainOf(t, newMemoryLayer(t), newMemoryLayer(t))
	if _, ok, err := c.Get(t.Context(), "nonexistent"); err != nil || ok {
		t.Fatalf("expected a miss from every layer: %v %v", ok, err)
	}
}

// TestNew_NoLayersIsAnError: a Cache must cache somewhere; v2 built a nil
// cache, or an empty chain that missed every time.
func TestNew_NoLayersIsAnError(t *testing.T) {
	if _, err := New(Config{}); err == nil || !strings.Contains(err.Error(), "no layer") {
		t.Fatalf("no layer: %v", err)
	}
	var cfg Config
	cfg.Breaker.Enabled = true
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "breaker") {
		t.Fatalf("a breaker with nothing behind it: %v", err)
	}
}

func TestNew_MemoryOnly(t *testing.T) {
	c := newCache(t, memoryOnly())
	ctx := t.Context()
	if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if data, ok, err := c.Get(ctx, "k"); err != nil || !ok || string(data) != "v" {
		t.Fatalf("Get = %q %v %v", data, ok, err)
	}
	if c.Redis() != nil || c.Health(ctx) != nil || c.Name() != "cache" {
		t.Fatal("memory only: no client, healthy, named cache")
	}
}

func TestNew_MemoryAndRedis(t *testing.T) {
	cfg, mr := withRedis(t, memoryOnly())
	c := newCache(t, cfg)
	ctx := t.Context()
	if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	// Write-through reaches Redis, not just memory.
	if got, err := mr.Get("k"); err != nil || got != "v" {
		t.Fatalf("redis has %q, %v", got, err)
	}
	// A value only Redis has fills the memory layer.
	if err := mr.Set("shared", "from another replica"); err != nil {
		t.Fatal(err)
	}
	if data, ok, err := c.Get(ctx, "shared"); err != nil || !ok || string(data) != "from another replica" {
		t.Fatalf("Get = %q %v %v", data, ok, err)
	}
	if data, ok, _ := c.memory.get(ctx, "shared"); !ok || string(data) != "from another replica" {
		t.Fatal("the Redis hit filled the memory layer")
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(ctx, "k"); ok || mr.Exists("k") {
		t.Fatal("Delete clears both layers")
	}
}

func TestNew_RedisTTLIsTheLayerDefault(t *testing.T) {
	cfg, mr := withRedis(t, Config{})
	cfg.Redis.TTL = time.Minute
	c := newCache(t, cfg)
	ctx := t.Context()
	if err := c.Set(ctx, "default", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(ctx, "own", []byte("v"), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if ttl := mr.TTL("default"); ttl != time.Minute {
		t.Fatalf("TTL 0 takes redis.ttl: %s", ttl)
	}
	if ttl := mr.TTL("own"); ttl != 5*time.Second {
		t.Fatalf("an explicit TTL wins: %s", ttl)
	}
}

func TestNew_BreakerDegradesRedisFailuresToMisses(t *testing.T) {
	cfg, mr := withRedis(t, Config{})
	cfg.Breaker.Enabled = true
	cfg.Breaker.Threshold = 1
	c := newCache(t, cfg)
	ctx := t.Context()
	if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	// Redis dies: the breaker absorbs it, and the cache misses instead of
	// failing on the hot path.
	mr.Close()
	for i := range 2 {
		if _, hit, err := c.Get(ctx, "k"); err != nil || hit {
			t.Fatalf("Get %d after Redis died: hit %v, err %v; want a silent miss", i+1, hit, err)
		}
	}
	if err := c.Set(ctx, "k", []byte("v2"), 0); err != nil {
		t.Fatalf("Set with the breaker open: %v", err)
	}
	if err := c.Health(ctx); err != nil {
		t.Fatalf("with the breaker a dead Redis is not unhealthy: %v", err)
	}
}

// TestHealth_UnreachableRedis: New does not dial, so a Redis that is down
// does not fail it; Health reports it, unless the breaker is on.
func TestHealth_UnreachableRedis(t *testing.T) {
	cfg, mr := withRedis(t, Config{})
	mr.Close()
	c := newCache(t, cfg)
	if err := c.Health(t.Context()); err == nil || !strings.Contains(err.Error(), "redis") {
		t.Fatalf("Health reports the unreachable Redis: %v", err)
	}
	cfg.Breaker.Enabled = true
	if err := newCache(t, cfg).Health(t.Context()); err != nil {
		t.Fatalf("with the breaker Health is nil: %v", err)
	}
}

func TestRedis_ExposesTheWorkingClient(t *testing.T) {
	cfg, mr := withRedis(t, Config{})
	c := newCache(t, cfg)
	ctx := t.Context()
	client := c.Redis()
	if client == nil {
		t.Fatal("the redis layer's client")
	}
	if err := client.Set(ctx, "direct", "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if got, err := mr.Get("direct"); err != nil || got != "v" {
		t.Fatalf("the client talks to the configured Redis: %q %v", got, err)
	}
	if err := c.Health(ctx); err != nil {
		t.Fatalf("Health with Redis up: %v", err)
	}
}

func TestRedis_NilWithoutTheLayer(t *testing.T) {
	if c := newCache(t, memoryOnly()); c.Redis() != nil {
		t.Fatal("no redis layer, no client")
	}
}

func TestClose_ClosesTheClientOnce(t *testing.T) {
	cfg, _ := withRedis(t, memoryOnly())
	c, err := New(cfg, Logger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatalf("Close is idempotent: %v", err)
	}
	if err := c.Redis().Ping(t.Context()).Err(); err == nil {
		t.Fatal("Close closed the client")
	}
}

// TestGet_ValueIsTheCallersCopy: neither the slice given to Set nor the
// one Get returns aliases what the memory layer keeps.
func TestGet_ValueIsTheCallersCopy(t *testing.T) {
	c := newCache(t, memoryOnly())
	ctx := t.Context()
	val := []byte("value")
	_ = c.Set(ctx, "k", val, 0)
	val[0] = 'X'
	got, _, _ := c.Get(ctx, "k")
	got[1] = 'Y'
	if again, _, _ := c.Get(ctx, "k"); string(again) != "value" {
		t.Fatalf("the cached value changed: %q", again)
	}
}

func TestSet_RejectsANegativeTTL(t *testing.T) {
	c := newCache(t, memoryOnly())
	if err := c.Set(t.Context(), "k", []byte("v"), -time.Second); err == nil {
		t.Fatal("a negative TTL is an error, not \"no expiry\"")
	}
	if _, err := c.GetOrLoad(t.Context(), "k", -time.Second, func(context.Context) ([]byte, error) { return nil, nil }); err == nil {
		t.Fatal("GetOrLoad too")
	}
	if _, err := c.GetOrLoad(t.Context(), "k", 0, nil); err == nil {
		t.Fatal("a nil load function is an error")
	}
}

// TestGetOrLoad_SharesOneLoad: concurrent misses of one key run the load
// once; every caller gets its result, which is then cached.
func TestGetOrLoad_SharesOneLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := New(memoryOnly(), Logger(quiet()))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close(t.Context()) }()
		var loads atomic.Int32
		release := make(chan struct{})
		load := func(context.Context) ([]byte, error) {
			loads.Add(1)
			<-release
			return []byte("loaded"), nil
		}
		var wg sync.WaitGroup
		results := make([]string, 10)
		for i := range results {
			wg.Go(func() {
				v, err := c.GetOrLoad(t.Context(), "k", time.Minute, load)
				if err != nil {
					t.Error(err)
				}
				results[i] = string(v)
			})
		}
		synctest.Wait() // one caller in load, nine waiting for it
		close(release)
		wg.Wait()
		if loads.Load() != 1 {
			t.Fatalf("%d loads for one key", loads.Load())
		}
		for i, r := range results {
			if r != "loaded" {
				t.Fatalf("caller %d got %q", i, r)
			}
		}
		if v, err := c.GetOrLoad(t.Context(), "k", time.Minute, load); err != nil || string(v) != "loaded" || loads.Load() != 1 {
			t.Fatalf("the loaded value is cached: %q %v, %d loads", v, err, loads.Load())
		}
	})
}

// TestGetOrLoad_WaiterStopsOnItsContext: a caller waiting for another's
// load gives up when its own context ends.
func TestGetOrLoad_WaiterStopsOnItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := New(memoryOnly(), Logger(quiet()))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close(t.Context()) }()
		release := make(chan struct{})
		leader := make(chan error, 1)
		go func() {
			_, err := c.GetOrLoad(t.Context(), "k", 0, func(context.Context) ([]byte, error) {
				<-release
				return []byte("v"), nil
			})
			leader <- err
		}()
		synctest.Wait()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		start := time.Now()
		if _, err := c.GetOrLoad(ctx, "k", 0, func(context.Context) ([]byte, error) { return nil, errors.New("not called") }); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the waiter's deadline: %v", err)
		}
		if waited := time.Since(start); waited != time.Second {
			t.Fatalf("the waiter waited %s", waited)
		}
		close(release)
		if err := <-leader; err != nil {
			t.Fatalf("the leader's load: %v", err)
		}
	})
}

func TestGetOrLoad_LoadErrorIsNotCached(t *testing.T) {
	c := newCache(t, memoryOnly())
	var loads int
	boom := errors.New("database down")
	load := func(context.Context) ([]byte, error) {
		loads++
		if loads == 1 {
			return nil, boom
		}
		return []byte("v"), nil
	}
	if _, err := c.GetOrLoad(t.Context(), "k", 0, load); !errors.Is(err, boom) {
		t.Fatalf("the load's error comes back as it is: %v", err)
	}
	if _, ok, _ := c.Get(t.Context(), "k"); ok {
		t.Fatal("a failed load stores nothing")
	}
	if v, err := c.GetOrLoad(t.Context(), "k", 0, load); err != nil || string(v) != "v" || loads != 2 {
		t.Fatalf("the next call loads again: %q %v, %d loads", v, err, loads)
	}
}

func TestConfig_Validate(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Config)
		want string
	}{
		"breaker without redis": {func(c *Config) { c.Memory.Enabled, c.Breaker.Enabled = true, true }, "breaker guards the redis layer"},
		"negative capacity":     {func(c *Config) { c.Memory.Enabled, c.Memory.Capacity = true, -1 }, "memory.capacity"},
		"empty addr":            {func(c *Config) { c.Redis.Enabled, c.Redis.Addr = true, "" }, "redis.addr"},
		"negative db":           {func(c *Config) { c.Redis.Enabled, c.Redis.DB = true, -1 }, "redis.db"},
		"negative dial_timeout": {func(c *Config) { c.Redis.Enabled, c.Redis.DialTimeout = true, -time.Second }, "redis.dial_timeout"},
		"negative threshold":    {func(c *Config) { c.Redis.Enabled, c.Breaker.Enabled, c.Breaker.Threshold = true, true, -1 }, "breaker.threshold"},
	} {
		c := Config{}.withDefaults()
		tc.edit(&c)
		if err := conf.Check(&c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error naming %q", name, err, tc.want)
		}
	}
	// A layer that is off is not checked.
	c := Config{}
	c.Redis.Addr, c.Redis.DB = "", -5
	if err := conf.Check(&c); err != nil {
		t.Fatalf("redis off: %v", err)
	}
}

func TestConfig_LoadsWithDefaults(t *testing.T) {
	type appConfig struct {
		chok.Base
		Cache Config `yaml:"cache"`
	}
	t.Setenv("CCT_CACHE_MEMORY_ENABLED", "true")
	t.Setenv("CCT_CACHE_REDIS_PASSWORD", "hunter2-redis")
	cfg, err := chok.Load[appConfig]("cct", chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := memoryOnly().withDefaults()
	want.Redis.Password = "hunter2-redis"
	if cfg.Cache != want {
		t.Fatalf("loaded %+v, want %+v", cfg.Cache, want)
	}
	t.Setenv("CCT_CACHE_BREAKER_ENABLED", "true")
	if _, err := chok.Load[appConfig]("cct", chok.Args(nil)); err == nil || !strings.Contains(err.Error(), "breaker") {
		t.Fatalf("a breaker without the redis layer fails the load: %v", err)
	}
}

// TestConfig_KeepsThePasswordOut: the dump --check-config prints and the
// fmt verbs mask the Redis password.
func TestConfig_KeepsThePasswordOut(t *testing.T) {
	var cfg Config
	cfg.Redis.Password = "super-secret-credential"
	dump, err := conf.Dump(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{string(dump), fmt.Sprint(cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), cfg.String(), cfg.GoString()} {
		if strings.Contains(s, "super-secret-credential") {
			t.Fatalf("the password leaked: %s", s)
		}
	}
	if !strings.Contains(string(dump), "password: '***'") {
		t.Fatalf("the dump masks it: %s", dump)
	}
}

// TestApp_RegistersTheCache: Use takes the Cache as a Healther, a Closer
// and a Namer.
func TestApp_RegistersTheCache(t *testing.T) {
	cfg, _ := withRedis(t, memoryOnly())
	c := newCache(t, cfg)
	client := choktest.App(t, chok.Base{}, func(app *chok.App) { app.Use(c) })
	var health struct {
		Components []struct {
			Component string `json:"component"`
			Status    string `json:"status"`
		} `json:"components"`
	}
	if code := client.GetJSON("/healthz", &health); code != http.StatusOK || len(health.Components) != 1 ||
		health.Components[0].Component != "cache" || health.Components[0].Status != "up" {
		t.Fatalf("/healthz = %d %+v", code, health)
	}
}

// TestRedisLog_GoesToSlog: go-redis's own messages, such as a dial that
// failed, are logged through slog.Default at warn instead of to stderr.
func TestRedisLog_GoesToSlog(t *testing.T) {
	var out bytes.Buffer
	before := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(before)
	cfg, mr := withRedis(t, Config{})
	mr.Close()
	c := newCache(t, cfg)
	if _, _, err := c.Get(t.Context(), "k"); err == nil {
		t.Fatal("a dead Redis without the breaker fails the Get")
	}
	if got := out.String(); !strings.Contains(got, `"level":"WARN","msg":"redis: connection pool: failed to dial`) ||
		!strings.Contains(got, `"logger":"go-redis"`) {
		t.Fatalf("go-redis's message in slog: %s", got)
	}
}
