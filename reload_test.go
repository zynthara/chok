package chok

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/log"
)

// reloadableComponent records Reload invocations. It is a minimal
// Component that also implements Reloadable so we can verify dispatch.
type reloadableComponent struct {
	name        string
	reloadCount atomic.Int32
	reloadErr   error
	closeCalled atomic.Bool
}

func (r *reloadableComponent) Name() string      { return r.name }
func (r *reloadableComponent) ConfigKey() string { return r.name }
func (r *reloadableComponent) Init(ctx context.Context, k component.Kernel) error {
	return nil
}
func (r *reloadableComponent) Close(ctx context.Context) error {
	r.closeCalled.Store(true)
	return nil
}
func (r *reloadableComponent) Reload(ctx context.Context) error {
	r.reloadCount.Add(1)
	return r.reloadErr
}

// --- App.Reload direct invocation ---

func TestReload_DispatchesToRegistry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := &reloadableComponent{name: "r"}
	app := New("test", WithLogger(log.Empty()))
	app.Register(c)

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)

	if err := app.Reload(context.Background()); err != nil {
		cancel()
		<-done
		t.Fatalf("Reload returned error: %v", err)
	}
	if c.reloadCount.Load() != 1 {
		cancel()
		<-done
		t.Fatalf("component Reload not called; count = %d", c.reloadCount.Load())
	}
	cancel()
	<-done
}

func TestReload_InvokesUserReloadFn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var userCalled atomic.Int32
	app := New("test",
		WithLogger(log.Empty()),
		WithReloadFunc(func(context.Context) error {
			userCalled.Add(1)
			return nil
		}),
	)

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)

	if err := app.Reload(context.Background()); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	if userCalled.Load() != 1 {
		cancel()
		<-done
		t.Fatalf("user reload fn not called; count = %d", userCalled.Load())
	}
	cancel()
	<-done
}

func TestReload_OrdersConfigThenRegistryThenUser(t *testing.T) {
	// A failing component Reload should abort before the user callback
	// runs, so the user fn does NOT see the new config — signalling the
	// reload aborted.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := &reloadableComponent{name: "r", reloadErr: errors.New("boom")}
	var userCalled atomic.Int32
	app := New("test",
		WithLogger(log.Empty()),
		WithReloadFunc(func(context.Context) error {
			userCalled.Add(1)
			return nil
		}),
	)
	app.Register(c)

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)

	err := app.Reload(context.Background())
	if err == nil {
		cancel()
		<-done
		t.Fatal("Reload should return error when component Reload fails")
	}
	if userCalled.Load() != 0 {
		cancel()
		<-done
		t.Fatalf("user reload fn should not run after component failure; count = %d", userCalled.Load())
	}
	cancel()
	<-done
}

// --- Immutable config reload ---

type reloadValidatableConfig struct {
	Name string `mapstructure:"name"`
	Port int    `mapstructure:"port"`
}

func (c *reloadValidatableConfig) Validate() error {
	if c.Port < 0 {
		return errors.New("port must be non-negative")
	}
	return nil
}

func TestReloadConfig_Immutable_PreservesOldOnValidationFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")

	// Initial valid config.
	if err := os.WriteFile(path, []byte("name: original\nport: 8080\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &reloadValidatableConfig{}
	app := New("test",
		WithLogger(log.Empty()),
		WithConfig(cfg, path),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan struct{})
	app.On(component.EventAfterStart, func(context.Context) error {
		close(ready)
		return nil
	})

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	<-ready // wait for startup to complete before reading config

	// Verify initial load.
	if cfg.Name != "original" || cfg.Port != 8080 {
		cancel()
		<-done
		t.Fatalf("initial config wrong: name=%q port=%d", cfg.Name, cfg.Port)
	}

	// Write invalid config (port < 0 fails Validate).
	if err := os.WriteFile(path, []byte("name: changed\nport: -1\n"), 0644); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}

	// ReloadConfig should fail — and the live config must be untouched.
	if _, _, err := app.ReloadConfig(); err == nil {
		cancel()
		<-done
		t.Fatal("ReloadConfig should fail for invalid config")
	}

	if cfg.Name != "original" {
		cancel()
		<-done
		t.Fatalf("config was partially updated: name=%q (expected original)", cfg.Name)
	}
	if cfg.Port != 8080 {
		cancel()
		<-done
		t.Fatalf("config was partially updated: port=%d (expected 8080)", cfg.Port)
	}

	// Write valid config — reload should succeed and update.
	if err := os.WriteFile(path, []byte("name: updated\nport: 9090\n"), 0644); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}

	if _, _, err := app.ReloadConfig(); err != nil {
		cancel()
		<-done
		t.Fatalf("ReloadConfig should succeed: %v", err)
	}
	if cfg.Name != "updated" || cfg.Port != 9090 {
		cancel()
		<-done
		t.Fatalf("config not updated after valid reload: name=%q port=%d", cfg.Name, cfg.Port)
	}

	cancel()
	<-done
}

// --- fsnotify file watcher ---

func TestWithConfigWatch_TriggersReloadOnFileChange(t *testing.T) {
	// Write a minimal YAML file and let App.Run watch it.
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(path, []byte("name: initial\n"), 0644); err != nil {
		t.Fatal(err)
	}

	type cfg struct {
		Name string `mapstructure:"name"`
	}
	cfgPtr := &cfg{}

	c := &reloadableComponent{name: "r"}
	app := New("test",
		WithLogger(log.Empty()),
		WithConfig(cfgPtr, path),
	)
	app.Register(c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, WithConfigWatch()) }()

	// Wait for Run to have started the registry + watcher.
	deadline := time.Now().Add(time.Second)
	for app.Registry() == nil {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("registry never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Small extra pause for fsnotify goroutine to actually Add the dir.
	time.Sleep(50 * time.Millisecond)

	// Touch the file — overwrite with different content so fsnotify
	// sees a Write.
	if err := os.WriteFile(path, []byte("name: updated\n"), 0644); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}

	// Wait up to 1s for the debounced reload to fire.
	deadline = time.Now().Add(time.Second)
	for c.reloadCount.Load() == 0 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("component Reload was never triggered after file change")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	<-done

	if cfgPtr.Name != "updated" {
		t.Fatalf("config pointer did not refresh; got %q want %q", cfgPtr.Name, "updated")
	}
}

func TestWithConfigWatch_NoPath_IsNoOp(t *testing.T) {
	// No config path → WithConfigWatch logs a warning and does nothing.
	// Run completes on ctx cancel just like without the option.
	ctx, cancel := context.WithCancel(context.Background())

	app := New("test", WithLogger(log.Empty()))

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, WithConfigWatch()) }()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after cancel")
	}
}

// blockingReloadable holds Reload until the test releases it, letting
// the test create a guaranteed overlap window between two App.Reload
// calls.
type blockingReloadable struct {
	name    string
	release chan struct{}
	hits    atomic.Int32
}

func (b *blockingReloadable) Name() string      { return b.name }
func (b *blockingReloadable) ConfigKey() string { return b.name }
func (b *blockingReloadable) Init(context.Context, component.Kernel) error {
	return nil
}
func (b *blockingReloadable) Close(context.Context) error { return nil }
func (b *blockingReloadable) Reload(ctx context.Context) error {
	b.hits.Add(1)
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestReload_CoalescesConcurrentCalls verifies the H1 fix: a second
// Reload arriving while another is in flight returns
// ErrReloadInProgress immediately instead of queueing behind the
// reload mutex. Coalescing is benign because the in-flight reload
// already re-reads the latest config — queuing just adds latency.
func TestReload_CoalescesConcurrentCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := &blockingReloadable{name: "r", release: make(chan struct{})}
	app := New("test", WithLogger(log.Empty()))
	app.Register(c)

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	// Wait for registry.Start to complete so Reload has a registry.
	for range 50 {
		if app.Registry() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	first := make(chan error, 1)
	go func() { first <- app.Reload(context.Background()) }()

	// Wait until the first Reload has the lock — observed via the
	// blocking component's Reload being called.
	for range 50 {
		if c.hits.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.hits.Load() != 1 {
		t.Fatal("first Reload did not enter the registry dispatch")
	}

	if err := app.Reload(context.Background()); !errors.Is(err, ErrReloadInProgress) {
		t.Fatalf("second Reload should return ErrReloadInProgress, got %v", err)
	}

	close(c.release)
	if err := <-first; err != nil {
		t.Fatalf("first Reload returned error: %v", err)
	}

	cancel()
	<-done
}
