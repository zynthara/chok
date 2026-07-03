package chok

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
)

// --- test modules ---------------------------------------------------------

type testComp struct {
	kind    string
	initRan *atomic.Bool
}

func (c *testComp) Describe() kernel.Descriptor { return kernel.Descriptor{Kind: c.kind} }
func (c *testComp) Init(ctx context.Context, k kernel.Kernel) error {
	if c.initRan != nil {
		c.initRan.Store(true)
	}
	return nil
}
func (c *testComp) Close(ctx context.Context) error { return nil }

// blockingServer is the v2 shape of v1's test server: Serve blocks
// until the kernel cancels it during shutdown.
type blockingServer struct {
	testComp
}

func (s *blockingServer) Serve(ctx context.Context, ready func()) error {
	ready()
	<-ctx.Done()
	return nil
}

func newBlockingServer() *blockingServer {
	return &blockingServer{testComp: testComp{kind: "blocksrv"}}
}

func waitTrue(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout: " + msg)
}

// --- Run semantics (ports chok_test.go themes) ------------------------------

func TestRunCtxCancel_ReturnsNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	app := New("test", WithLogger(log.Empty()), Use(newBlockingServer()))

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ctx cancel is an orderly stop, want nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestRunCtxDeadlineExceeded_ReturnsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	app := New("test", WithLogger(log.Empty()), Use(newBlockingServer()))

	err := app.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline expiry must surface as error, got %v", err)
	}
}

func TestSingleUse(t *testing.T) {
	app := New("test", WithLogger(log.Empty()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = app.Run(ctx)
	if err := app.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "single-use") {
		t.Fatalf("second Run must fail: %v", err)
	}
}

// --- assembly semantics (Use / Override) -------------------------------------

func TestUse_DuplicateKey_FailsRun(t *testing.T) {
	app := New("test", WithLogger(log.Empty()),
		Use(&testComp{kind: "db"}, &testComp{kind: "db"}))
	err := app.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate Use must fail startup, got %v", err)
	}
}

func TestOverride_ReplacesModule(t *testing.T) {
	var origRan, overrideRan atomic.Bool
	orig := &testComp{kind: "db", initRan: &origRan}
	repl := &testComp{kind: "db", initRan: &overrideRan}

	ctx, cancel := context.WithCancel(context.Background())
	app := New("test", WithLogger(log.Empty()), Use(orig), Override(repl))
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	waitTrue(t, overrideRan.Load, "override module must init")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if origRan.Load() {
		t.Fatal("overridden module must not init")
	}
}

func TestOverride_MissingKey_FailsRun(t *testing.T) {
	app := New("test", WithLogger(log.Empty()),
		Use(&testComp{kind: "db"}),
		Override(&testComp{kind: "dbx"})) // typo'd kind
	err := app.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "matches no assembled module") {
		t.Fatalf("Override on missing key must fail, got %v", err)
	}
}

// --- Section[T] ----------------------------------------------------------------

type bizCfg struct {
	Name  string `mapstructure:"name" default:"anon"`
	Limit int    `mapstructure:"limit" default:"10"`
}

func TestSection_TypedHandle(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(cfg, []byte("biz:\n  name: alice\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app := New("test", WithLogger(log.Empty()), WithConfigFile(cfg), Use(newBlockingServer()))
	h := Section[bizCfg](app, "biz")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	waitTrue(t, func() bool { return app.Kernel() != nil && app.storeRef() != nil }, "app assembling")
	time.Sleep(50 * time.Millisecond)

	got := h.Get()
	if got.Name != "alice" {
		t.Fatalf("yaml value must land: %+v", got)
	}
	if got.Limit != 10 {
		t.Fatalf("default tag must apply to business sections: %+v", got)
	}

	// Registration after Run panics — the loader's type set is sealed.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Section after Run must panic")
			}
		}()
		Section[bizCfg](app, "late")
	}()

	cancel()
	<-done
}

func TestSection_GetBeforeRun_Panics(t *testing.T) {
	app := New("test", WithLogger(log.Empty()))
	h := Section[bizCfg](app, "biz")
	defer func() {
		if recover() == nil {
			t.Fatal("Get before Run must panic")
		}
	}()
	_ = h.Get()
}

// --- logger ownership ------------------------------------------------------------

type closableLogger struct {
	log.Logger
	closed atomic.Bool
}

func (c *closableLogger) Close() error {
	c.closed.Store(true)
	return nil
}

func TestWithLogger_CallerOwnsLifecycle(t *testing.T) {
	cl := &closableLogger{Logger: log.Empty()}
	ctx, cancel := context.WithCancel(context.Background())
	app := New("test", WithLogger(cl), Use(newBlockingServer()))
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if cl.closed.Load() {
		t.Fatal("injected logger must not be closed by the App")
	}
}

// --- reload plumbing (WithReloadFunc gating at App level) --------------------------

func TestReload_InvokesUserReloadFn(t *testing.T) {
	var called atomic.Bool
	app := New("test", WithLogger(log.Empty()),
		WithReloadFunc(func(context.Context) error { called.Store(true); return nil }),
		Use(newBlockingServer()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	waitTrue(t, func() bool { return app.Kernel() != nil }, "assembled")
	time.Sleep(50 * time.Millisecond)

	if err := app.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !called.Load() {
		t.Fatal("WithReloadFunc must run as the reload's last stage")
	}
	cancel()
	<-done
}

// --- signals (ports signal_test.go; sequential — they signal the process) ----------

func startSignalApp(t *testing.T, opts ...Option) chan error {
	t.Helper()
	base := []Option{WithLogger(log.Empty()), Use(newBlockingServer())}
	app := New("test", append(base, opts...)...)
	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), WithSignals()) }()
	time.Sleep(80 * time.Millisecond) // let startup + signal watcher arm
	return done
}

func awaitNil(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown must return nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for shutdown")
	}
}

func TestSignal_SIGTERM_GracefulShutdown(t *testing.T) {
	done := startSignalApp(t)
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	awaitNil(t, done)
}

func TestSignal_SIGINT_GracefulShutdown(t *testing.T) {
	done := startSignalApp(t)
	_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	awaitNil(t, done)
}

func TestSignal_SIGQUIT_FastShutdown(t *testing.T) {
	done := startSignalApp(t)
	_ = syscall.Kill(os.Getpid(), syscall.SIGQUIT)
	awaitNil(t, done)
}

func TestSignal_SIGHUP_ReloadNotConfigured_KeepsRunning(t *testing.T) {
	done := startSignalApp(t)
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(80 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("SIGHUP must not exit the app: %v", err)
	default:
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	awaitNil(t, done)
}

func TestSignal_SIGHUP_ReloadCalled(t *testing.T) {
	var called atomic.Bool
	done := startSignalApp(t, WithReloadFunc(func(context.Context) error {
		called.Store(true)
		return nil
	}))
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	waitTrue(t, called.Load, "reload func on SIGHUP")
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	awaitNil(t, done)
}

func TestSignal_SIGHUP_IgnoreDuringReload(t *testing.T) {
	var count atomic.Int32
	done := startSignalApp(t, WithReloadFunc(func(context.Context) error {
		count.Add(1)
		time.Sleep(200 * time.Millisecond)
		return nil
	}))
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(20 * time.Millisecond)
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP) // lands mid-reload → dropped
	time.Sleep(350 * time.Millisecond)
	if c := count.Load(); c != 1 {
		t.Fatalf("second SIGHUP during a reload must be ignored, got %d reloads", c)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	awaitNil(t, done)
}

func TestSignal_SIGHUP_SequentialDelivery(t *testing.T) {
	var count atomic.Int32
	done := startSignalApp(t, WithReloadFunc(func(context.Context) error {
		count.Add(1)
		return nil
	}))
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(150 * time.Millisecond)
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(150 * time.Millisecond)
	if c := count.Load(); c != 2 {
		t.Fatalf("sequential SIGHUPs must both reload, got %d", c)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	awaitNil(t, done)
}

func TestSignal_SIGHUP_NoServers_RepeatedReload(t *testing.T) {
	var count atomic.Int32
	app := New("test", WithLogger(log.Empty()),
		WithReloadFunc(func(context.Context) error { count.Add(1); return nil }))
	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), WithSignals()) }()
	time.Sleep(80 * time.Millisecond)

	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(120 * time.Millisecond)
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(120 * time.Millisecond)
	if c := count.Load(); c != 2 {
		t.Fatalf("no-server app must keep reloading, got %d", c)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	awaitNil(t, done)
}

func TestRunDefault_NoSignalHandling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	app := New("test", WithLogger(log.Empty()), Use(newBlockingServer()))
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	awaitNil(t, done)
}

func TestReloadTimeout_OnlyLogs(t *testing.T) {
	done := startSignalApp(t,
		WithReloadTimeout(50*time.Millisecond),
		WithReloadFunc(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}))
	_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(200 * time.Millisecond) // reload times out; app must survive
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	awaitNil(t, done)
}

// --- config watch (ports reload_test.go watcher themes) ---------------------------

func TestWithConfigWatch_TriggersReloadOnFileChange(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(cfg, []byte("biz:\n  name: v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var count atomic.Int32
	app := New("test", WithLogger(log.Empty()), WithConfigFile(cfg),
		WithReloadFunc(func(context.Context) error { count.Add(1); return nil }),
		Use(newBlockingServer()))
	h := Section[bizCfg](app, "biz")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, WithConfigWatch()) }()
	waitTrue(t, func() bool { return app.Kernel() != nil }, "assembled")
	time.Sleep(150 * time.Millisecond) // watcher armed + hash seeded

	if err := os.WriteFile(cfg, []byte("biz:\n  name: v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitTrue(t, func() bool { return count.Load() >= 1 }, "file change must trigger reload")
	waitTrue(t, func() bool { return h.Get().Name == "v2" }, "new snapshot visible")

	cancel()
	<-done
}

func TestWithConfigWatch_NoPath_IsNoOp(t *testing.T) {
	t.Chdir(t.TempDir()) // nothing auto-detected
	ctx, cancel := context.WithCancel(context.Background())
	app := New("test", WithLogger(log.Empty()), Use(newBlockingServer()))
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, WithConfigWatch()) }()
	time.Sleep(80 * time.Millisecond)
	cancel()
	awaitNil(t, done)
}

// --- WithErrorMapper handshake ----------------------------------------------

// mapperSink is an assembled component that consumes the per-App
// error-mapper registry (the web module's role in production). The
// attach happens on the Run goroutine while the test polls — hence
// the atomic holder.
type mapperSink struct {
	testComp
	got atomic.Pointer[apierr.MapperRegistry]
}

func (m *mapperSink) AttachErrorMappers(reg *apierr.MapperRegistry) { m.got.Store(reg) }

// TestWithErrorMapper_ReachesAttachConsumer pins the assembly
// handshake the blog rebuild exposed as missing: WithErrorMapper was
// a silent no-op because nothing ever called AttachErrorMappers on
// the assembled components. Regression for the M5 fix.
func TestWithErrorMapper_ReachesAttachConsumer(t *testing.T) {
	sink := &mapperSink{testComp: testComp{kind: "mapper-sink"}}
	mapped := apierr.ErrConflict.WithMessage("mapped")
	app := New("mapper-test",
		Use(sink),
		WithErrorMapper(func(err error) *apierr.Error { return mapped }),
	)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx) }()
	waitTrue(t, func() bool { return sink.got.Load() != nil }, "AttachErrorMappers never called during assembly")

	if got := sink.got.Load().Resolve(errors.New("anything")); got == nil || got.Message != "mapped" {
		t.Fatalf("attached registry must carry the WithErrorMapper mapper, got %v", got)
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
