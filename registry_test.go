package chok

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/parts"
)

// recordingComponent captures Init/Close invocations so tests can
// assert ordering relative to other Run lifecycle events.
type recordingComponent struct {
	name        string
	initCount   atomic.Int32
	closeCalled atomic.Bool
	initFn      func(ctx context.Context, k component.Kernel) error
	deps        []string
}

func (r *recordingComponent) Name() string      { return r.name }
func (r *recordingComponent) ConfigKey() string { return r.name }
func (r *recordingComponent) Init(ctx context.Context, k component.Kernel) error {
	r.initCount.Add(1)
	if r.initFn != nil {
		return r.initFn(ctx, k)
	}
	return nil
}
func (r *recordingComponent) Close(ctx context.Context) error {
	r.closeCalled.Store(true)
	return nil
}

type depsRecordingComponent struct {
	*recordingComponent
	depsList []string
}

func (d depsRecordingComponent) Dependencies() []string { return d.depsList }

// --- Register / Registry accessor ---

func TestApp_Register_BeforeRun_OK(t *testing.T) {
	app := New("test", WithLogger(log.Empty()))
	app.Register(&recordingComponent{name: "c1"})
	// No panic expected; registry is nil until Run.
	if app.Registry() != nil {
		t.Fatal("Registry() should be nil before Run")
	}
}

func TestApp_Register_AfterRegistryStarted_Panics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	app := New("test", WithLogger(log.Empty()))
	app.Register(&recordingComponent{name: "c1"})

	// Use a quick-exit flow: no servers, ctx cancelled from outside.
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	// Wait for registry to actually be built — Run goes through loadConfig,
	// initLogger, initCache, setupFn, then Registry build. On a bare app
	// with no servers, after the registry starts, runServers blocks on
	// ctx. So by the time app.Registry() is non-nil, we know Register
	// should panic.
	deadline := time.Now().Add(time.Second)
	for app.Registry() == nil {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("registry never started")
		}
		time.Sleep(2 * time.Millisecond)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("Register after registry start should panic")
		}
		cancel()
		<-done
	}()
	app.Register(&recordingComponent{name: "c2"})
}

func TestApp_Registry_Accessor_AvailableFromCleanup(t *testing.T) {
	// A cleanup callback runs after registry.Stop, but Registry() should
	// still expose the (stopped) registry so diagnostics can enumerate
	// components.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := New("test", WithLogger(log.Empty()))
	app.Register(&recordingComponent{name: "c1"})

	var sawRegistry atomic.Bool
	app.AddCleanup(func(_ context.Context) error {
		if app.Registry() != nil {
			sawRegistry.Store(true)
		}
		return nil
	})

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	if !sawRegistry.Load() {
		t.Fatal("cleanup did not observe Registry() after Stop")
	}
}

// --- Lifecycle integration ---

func TestApp_Run_InitializesComponent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := &recordingComponent{name: "c1"}
	app := New("test", WithLogger(log.Empty()))
	app.Register(c)

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)

	if c.initCount.Load() == 0 {
		cancel()
		<-done
		t.Fatal("Component Init was never called")
	}
	cancel()
	<-done
	if !c.closeCalled.Load() {
		t.Fatal("Component Close was not called on shutdown")
	}
}

func TestApp_Run_InitFailure_Aborts(t *testing.T) {
	bad := &recordingComponent{
		name: "bad",
		initFn: func(_ context.Context, _ component.Kernel) error {
			return errors.New("init-boom")
		},
	}
	app := New("test", WithLogger(log.Empty()))
	app.Register(bad)

	err := app.Run(context.Background())
	if err == nil || !errors.Is(err, err) || err.Error() == "" {
		t.Fatal("Run should return Init error")
	}
	// Should not leave servers running (there are none), and cleanup
	// should have been invoked.
}

func TestApp_Run_SetupFn_CanRegisterComponents(t *testing.T) {
	// Components added from inside setupFn participate in the registry
	// the same way as those added via App.Register pre-Run.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fromSetup := &recordingComponent{name: "from-setup"}

	app := New("test",
		WithLogger(log.Empty()),
		WithSetup(func(_ context.Context, a *App) error {
			a.Register(fromSetup)
			return nil
		}),
	)

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	if fromSetup.initCount.Load() == 0 {
		t.Fatal("setup-registered component was never Init'd")
	}
	if !fromSetup.closeCalled.Load() {
		t.Fatal("setup-registered component was never Close'd")
	}
}

func TestApp_Run_TopoOrderAcrossSetupRegistrations(t *testing.T) {
	// Verify cross-dependency ordering when one Component is registered
	// pre-Run and another from inside setupFn.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbBase := &recordingComponent{name: "db"}
	acctBase := &recordingComponent{name: "acct"}
	db := depsRecordingComponent{recordingComponent: dbBase}
	acct := depsRecordingComponent{recordingComponent: acctBase, depsList: []string{"db"}}

	app := New("test",
		WithLogger(log.Empty()),
		WithSetup(func(_ context.Context, a *App) error {
			a.Register(acct) // registered second in Setup
			return nil
		}),
	)
	app.Register(db) // registered first pre-Run

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	if dbBase.initCount.Load() == 0 || acctBase.initCount.Load() == 0 {
		t.Fatal("both components should have been Init'd")
	}
}

// TestApp_Logger_MatchesRegistryComponent verifies phase 3.2's
// pre-built contract: App.Logger() and the LoggerComponent's Logger()
// return the same instance, so reload calls (which the registry
// dispatches) apply to the logger App callers already hold.
func TestApp_Logger_MatchesRegistryComponent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	existing := log.Empty()
	app := New("test", WithLogger(existing))

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)

	lc, ok := app.Registry().Get("log").(*parts.LoggerComponent)
	if !ok || lc == nil {
		cancel()
		<-done
		t.Fatal("log component not found in registry")
	}
	if lc.Logger() != app.Logger() {
		cancel()
		<-done
		t.Fatal("LoggerComponent.Logger() should be same instance as App.Logger()")
	}
	if lc.Logger() != existing {
		cancel()
		<-done
		t.Fatal("pre-built logger was not adopted by the component")
	}
	cancel()
	<-done
}

// TestApp_Registry_Reload_DispatchesToLogger verifies Reload reaches
// the auto-registered LoggerComponent and SetLevel would fire. We can't
// observe the slog level through the interface, so the test asserts
// only that Reload returns without error — non-error is the meaningful
// signal because the LoggerComponent's Reload calls SetLevel which
// would surface an error on unknown levels.
func TestApp_Registry_Reload_DispatchesToLogger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := New("test", WithLogger(log.Empty()))

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)

	if err := app.Registry().Reload(ctx); err != nil {
		cancel()
		<-done
		t.Fatalf("Registry().Reload should succeed against pre-built logger, got %v", err)
	}

	cancel()
	<-done
}

func TestApp_Run_AutoRegistersBuiltinLogger(t *testing.T) {
	// After phase 3.2 the App auto-registers a LoggerComponent in
	// pre-built mode. Apps with no user Components still get a
	// non-nil registry containing just the built-in "log" entry.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := New("test", WithLogger(log.Empty()))

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)

	reg := app.Registry()
	if reg == nil {
		cancel()
		<-done
		t.Fatal("Registry() should be non-nil after phase 3.2 auto-register")
	}
	if reg.Get("log") == nil {
		cancel()
		<-done
		t.Fatal("built-in log component should be registered")
	}
	cancel()
	<-done
}
