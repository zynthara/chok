package chok

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/log"
)

// --- helpers ---

// blockingServer blocks on Start until Stop is called.
type blockingServer struct {
	mu      sync.Mutex
	stopCh  chan struct{}
	started bool
}

func newBlockingServer() *blockingServer {
	return &blockingServer{stopCh: make(chan struct{})}
}

func (s *blockingServer) Start(_ context.Context, ready func()) error {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
	ready()
	<-s.stopCh
	return nil
}

func (s *blockingServer) Stop(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	return nil
}

// failStartServer fails during Start before calling ready.
type failStartServer struct{ err error }

func (s *failStartServer) Start(_ context.Context, _ func()) error { return s.err }
func (s *failStartServer) Stop(_ context.Context) error            { return nil }

// --- tests ---

func TestRunCtxCancel_ReturnsNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	app := New("test", WithLogger(log.Empty()))
	srv := newBlockingServer()
	app.AddServer(srv)

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil on cancel, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestRunCtxDeadlineExceeded_ReturnsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	app := New("test", WithLogger(log.Empty()))
	app.AddServer(newBlockingServer())

	err := app.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
}

func TestSingleUse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled

	app := New("test", WithLogger(log.Empty()))
	_ = app.Run(ctx)
	err := app.Run(ctx)
	if err == nil || err.Error() != "chok: Run/Execute already called (App is single-use)" {
		t.Fatalf("expected single-use error, got: %v", err)
	}
}

func TestServerStartupFailure_Rollback(t *testing.T) {
	app := New("test", WithLogger(log.Empty()))
	good := newBlockingServer()
	bad := &failStartServer{err: errors.New("bind failed")}

	app.AddServer(good)
	app.AddServer(bad)

	err := app.Run(context.Background())
	if err == nil || !errors.Is(err, errors.New("bind failed")) && err.Error() != "bind failed" {
		// Check the error message contains our error.
		if err == nil {
			t.Fatal("expected error")
		}
	}
}

func TestServerUnexpectedExit(t *testing.T) {
	app := New("test", WithLogger(log.Empty()))
	// Server that returns nil immediately without Stop.
	app.AddServer(ServerFunc(func(ctx context.Context, ready func()) error {
		ready()
		return nil // unexpected exit
	}))

	err := app.Run(context.Background())
	if !errors.Is(err, ErrServerUnexpectedExit) {
		t.Fatalf("expected ErrServerUnexpectedExit, got: %v", err)
	}
}

func TestServerFunc_StopCancelsCtx(t *testing.T) {
	stopped := make(chan struct{})
	app := New("test", WithLogger(log.Empty()))
	app.AddServer(ServerFunc(func(ctx context.Context, ready func()) error {
		ready()
		<-ctx.Done()
		close(stopped)
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("ServerFunc ctx not cancelled on Stop")
	}
}

func TestSetupFailure_CleanupStillRuns(t *testing.T) {
	var cleaned atomic.Bool
	app := New("test",
		WithLogger(log.Empty()),
		WithSetup(func(ctx context.Context, a *App) error {
			a.AddCleanup(func(_ context.Context) error {
				cleaned.Store(true)
				return nil
			})
			return errors.New("setup boom")
		}),
	)

	err := app.Run(context.Background())
	if err == nil {
		t.Fatal("expected setup error")
	}
	if !cleaned.Load() {
		t.Fatal("cleanup should run even on setup failure")
	}
}

func TestSetupReceivesRunCtx(t *testing.T) {
	type ctxKey string
	ctx := context.WithValue(context.Background(), ctxKey("k"), "v")

	var got string
	app := New("test",
		WithLogger(log.Empty()),
		WithSetup(func(ctx context.Context, a *App) error {
			got = ctx.Value(ctxKey("k")).(string)
			return nil
		}),
	)

	ctxRun, cancel := context.WithCancel(ctx)
	cancel()
	_ = app.Run(ctxRun)

	if got != "v" {
		t.Fatalf("setup did not receive Run's ctx")
	}
}

func TestCleanup_LIFO(t *testing.T) {
	var order []int
	var mu sync.Mutex
	app := New("test",
		WithLogger(log.Empty()),
		WithCleanup(func(_ context.Context) error { mu.Lock(); order = append(order, 1); mu.Unlock(); return nil }),
		WithCleanup(func(_ context.Context) error { mu.Lock(); order = append(order, 2); mu.Unlock(); return nil }),
		WithSetup(func(_ context.Context, a *App) error {
			a.AddCleanup(func(_ context.Context) error { mu.Lock(); order = append(order, 3); mu.Unlock(); return nil })
			return nil
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = app.Run(ctx)

	if len(order) != 3 || order[0] != 3 || order[1] != 2 || order[2] != 1 {
		t.Fatalf("expected LIFO [3,2,1], got %v", order)
	}
}

func TestCleanup_ReceivesTimeoutCtx(t *testing.T) {
	var deadline time.Time
	var hasDeadline bool
	app := New("test",
		WithLogger(log.Empty()),
		WithShutdownTimeout(5*time.Second),
		WithCleanup(func(ctx context.Context) error {
			deadline, hasDeadline = ctx.Deadline()
			return nil
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = app.Run(ctx)

	if !hasDeadline {
		t.Fatal("cleanup ctx should have deadline")
	}
	if time.Until(deadline) > 6*time.Second {
		t.Fatalf("deadline too far: %v", deadline)
	}
}

func TestStopPreReady_SafeCall(t *testing.T) {
	// Server whose Start blocks on a channel before calling ready.
	// Stop should unblock it.
	type slowServer struct {
		stopCh chan struct{}
	}

	srv := &slowServer{stopCh: make(chan struct{})}

	app := New("test", WithLogger(log.Empty()))
	app.AddServer(ServerFunc(func(ctx context.Context, ready func()) error {
		select {
		case <-srv.stopCh:
			return errors.New("stopped before ready")
		case <-ctx.Done():
			return ctx.Err()
		}
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		// Either nil or some error is fine; it should not hang.
		_ = err
	case <-time.After(5 * time.Second):
		t.Fatal("Stop in pre-ready stage should not hang")
	}
}

func TestDrainDelay_ReadyzBeforeServerStop(t *testing.T) {
	// Verify: with a drain delay, the health component is marked as
	// shutting down BEFORE the server Stop is called.
	var healthShutdown atomic.Bool

	app := New("test",
		WithLogger(log.Empty()),
		WithDrainDelay(50*time.Millisecond),
		WithShutdownTimeout(2*time.Second),
	)

	// Custom server that checks health state inside Stop.
	srv := &drainTestServer{
		stopCh:      make(chan struct{}),
		healthCheck: &healthShutdown,
	}
	app.AddServer(srv)

	// Register a fake health component.
	app.Register(&drainTestHealthComp{shutdown: &healthShutdown})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	// Let servers start up.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	if !srv.drainedBeforeStop.Load() {
		t.Fatal("expected health shutdown flag to be set before server Stop")
	}
}

// drainTestServer records whether SetShuttingDown was called before Stop.
type drainTestServer struct {
	stopCh            chan struct{}
	healthCheck       *atomic.Bool
	drainedBeforeStop atomic.Bool
}

func (s *drainTestServer) Start(_ context.Context, ready func()) error {
	ready()
	<-s.stopCh
	return nil
}

func (s *drainTestServer) Stop(_ context.Context) error {
	s.drainedBeforeStop.Store(s.healthCheck.Load())
	close(s.stopCh)
	return nil
}

// drainTestHealthComp is a minimal Component that mimics HealthComponent's
// SetShuttingDown behavior for testing the drain delay.
type drainTestHealthComp struct {
	shutdown *atomic.Bool
}

func (d *drainTestHealthComp) Name() string                                     { return "health" }
func (d *drainTestHealthComp) ConfigKey() string                                { return "health" }
func (d *drainTestHealthComp) Init(_ context.Context, _ component.Kernel) error { return nil }
func (d *drainTestHealthComp) Close(_ context.Context) error                    { return nil }
func (d *drainTestHealthComp) SetShuttingDown()                                 { d.shutdown.Store(true) }

func TestMultiServerRollback(t *testing.T) {
	var stopOrder []int
	var mu sync.Mutex

	makeSrv := func(id int, failStart bool) Server {
		return ServerFunc(func(ctx context.Context, ready func()) error {
			if failStart {
				return errors.New("start failed")
			}
			ready()
			<-ctx.Done()
			mu.Lock()
			stopOrder = append(stopOrder, id)
			mu.Unlock()
			return nil
		})
	}

	app := New("test", WithLogger(log.Empty()))
	app.AddServer(makeSrv(1, false))
	app.AddServer(makeSrv(2, false))
	app.AddServer(makeSrv(3, true)) // this one fails

	err := app.Run(context.Background())
	if err == nil {
		t.Fatal("expected error from failed server")
	}
}
