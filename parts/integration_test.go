package parts

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zynthara/chok/auth/jwt"
	"github.com/zynthara/chok/authz"
	"github.com/zynthara/chok/cache"
	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/log"
)

// integrationCfg is a minimal app-wide config used by the full-Registry
// integration test below. Each top-level field is what a resolver will
// reach into.
type integrationCfg struct {
	Log     *config.SlogOptions
	Redis   *config.RedisOptions
	Swagger *SwaggerSettings
}

// TestPhase3_FullRegistry spins up every Component this package ships
// inside one component.Registry, verifies topological ordering is
// respected, health aggregation works, and Stop returns cleanly.
//
// Components wired:
//   - log       (no deps)
//   - redis     (no deps, disabled — nil resolver so no real socket needed)
//   - db        (no deps, in-memory sqlite)
//   - cache     (dep: redis, memory-only here)
//   - jwt       (no deps)
//   - authz     (no deps)
//   - scheduler (dep: log)
//   - swagger   (no deps)
//   - account   (dep: db, log)
//
// Registration order is deliberately scrambled so the topo sort has to
// do real work to put db + log before account, and redis before cache.
func TestPhase3_FullRegistry(t *testing.T) {
	cfg := &integrationCfg{
		Log: &config.SlogOptions{
			Level:  "info",
			Format: "json",
			Output: []string{"stdout"},
		},
		Redis:   nil, // disabled path — exercises the "no socket" mode
		Swagger: &SwaggerSettings{Enabled: true, Title: "integration", Version: "1"},
	}
	reg := component.New(cfg, log.Empty())

	// Register scrambled: dependents before dependencies so topo sort
	// has to reorder.
	reg.Register(NewAccountComponent(accountBuilder(), "/auth"))
	reg.Register(NewSchedulerComponent(context.Background(), time.Second))
	reg.Register(NewCacheComponent(func(k component.Kernel) (cache.Cache, error) {
		return cache.NewMemory(&cache.MemoryOptions{Capacity: 100, TTL: time.Minute})
	}))
	reg.Register(NewSwaggerComponent(func(a any) *SwaggerSettings {
		return a.(*integrationCfg).Swagger
	}))
	reg.Register(NewAuthzComponent(func(component.Kernel) (authz.Authorizer, error) {
		return authz.AuthorizerFunc(func(context.Context, string, string, string) (bool, error) {
			return true, nil
		}), nil
	}))
	reg.Register(NewJWTComponent("jwt", func(component.Kernel) (*jwt.Manager, error) {
		return jwt.NewManager(jwt.Options{SigningKey: accountTestKey})
	}))
	reg.Register(NewLoggerComponent(func(a any) *config.SlogOptions {
		return a.(*integrationCfg).Log
	}))
	reg.Register(NewRedisComponent(func(a any) *config.RedisOptions {
		return a.(*integrationCfg).Redis
	}))
	reg.Register(NewDBComponent(func(component.Kernel) (*gorm.DB, error) {
		return gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	}))

	ctx := context.Background()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Every component should be reachable via Get.
	for _, name := range []string{"log", "redis", "db", "cache", "jwt", "authz", "scheduler", "swagger", "account"} {
		if reg.Get(name) == nil {
			t.Errorf("Get(%q) returned nil after Start", name)
		}
	}

	// Spot-check that dependency objects are live.
	if reg.Get("db").(*DBComponent).DB() == nil {
		t.Error("db component has nil *gorm.DB after Start")
	}
	if reg.Get("account").(*AccountComponent).Module() == nil {
		t.Error("account module nil after Start")
	}

	// Health aggregation: all OK expected (redis disabled → OK, db ping → OK).
	rep := reg.Health(ctx)
	if rep.Status != component.HealthOK {
		t.Errorf("expected OK aggregate, got %q; detail: %+v", rep.Status, rep.Components)
	}

	// Reload must not explode on the Reloadable components (only log
	// participates; the rest are skipped).
	if err := reg.Reload(ctx); err != nil {
		t.Errorf("Reload failed: %v", err)
	}

	if err := reg.Stop(ctx); err != nil {
		t.Errorf("Stop failed: %v", err)
	}
	// Double-stop idempotent.
	if err := reg.Stop(ctx); err != nil {
		t.Errorf("second Stop should be nil, got %v", err)
	}
}

// TestPhase3_AccountInitFailsWithoutDB verifies the dependency-by-name
// contract: account requires "db" by topo, but Init additionally asserts
// the component is actually resolvable. The registry won't resolve it
// unless it was registered.
func TestPhase3_AccountInitFailsWithoutDB(t *testing.T) {
	// Construct a registry where account declares "db, log" but we
	// register only log. Registry.Start should fail with unknown-dep.
	reg := component.New(nil, log.Empty())
	reg.Register(NewLoggerComponent(func(any) *config.SlogOptions { return nil }))
	reg.Register(NewAccountComponent(accountBuilder(), "/auth"))

	err := reg.Start(context.Background())
	if err == nil {
		t.Fatal("Start should fail when account's 'db' dependency is not registered")
	}
}
