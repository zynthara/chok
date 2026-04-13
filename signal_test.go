package chok

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zynthara/chok/log"
)

func TestSignal_SIGTERM_GracefulShutdown(t *testing.T) {
	app := New("test", WithLogger(log.Empty()))
	app.AddServer(newBlockingServer())

	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), WithSignals()) }()

	time.Sleep(50 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGTERM)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SIGTERM should return nil, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for SIGTERM shutdown")
	}
}

func TestSignal_SIGINT_GracefulShutdown(t *testing.T) {
	app := New("test", WithLogger(log.Empty()))
	app.AddServer(newBlockingServer())

	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), WithSignals()) }()

	time.Sleep(50 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGINT)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SIGINT should return nil, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestSignal_SIGQUIT_FastShutdown(t *testing.T) {
	app := New("test", WithLogger(log.Empty()))
	app.AddServer(newBlockingServer())

	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), WithSignals()) }()

	time.Sleep(50 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGQUIT)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SIGQUIT should return nil, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestSignal_SIGHUP_ReloadNotConfigured(t *testing.T) {
	app := New("test", WithLogger(log.Empty()))
	app.AddServer(newBlockingServer())

	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), WithSignals()) }()

	time.Sleep(50 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	// Should not exit — send SIGTERM to stop.
	time.Sleep(50 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGTERM)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestSignal_SIGHUP_ReloadCalled(t *testing.T) {
	var called atomic.Bool
	app := New("test",
		WithLogger(log.Empty()),
		WithReloadFunc(func(_ context.Context) error {
			called.Store(true)
			return nil
		}),
	)
	app.AddServer(newBlockingServer())

	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), WithSignals()) }()

	time.Sleep(50 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(100 * time.Millisecond)

	if !called.Load() {
		t.Fatal("reload func should have been called")
	}

	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	<-done
}

// regression: SIGHUP during an in-progress reload must be ignored (design:
// "上一次未完成时新信号被忽略"). Two quick HUPs → only one reload.
func TestSignal_SIGHUP_IgnoreDuringReload(t *testing.T) {
	var count atomic.Int32
	app := New("test",
		WithLogger(log.Empty()),
		WithReloadFunc(func(_ context.Context) error {
			count.Add(1)
			time.Sleep(200 * time.Millisecond) // slow reload
			return nil
		}),
	)
	app.AddServer(newBlockingServer())

	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), WithSignals()) }()

	time.Sleep(50 * time.Millisecond)
	// Send two HUPs quickly — second arrives during first reload.
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(10 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(300 * time.Millisecond)

	if c := count.Load(); c != 1 {
		t.Fatalf("expected 1 reload (second SIGHUP during reload should be ignored), got %d", c)
	}

	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	<-done
}

func TestRunDefault_NoSignalHandling(t *testing.T) {
	// Run() without WithSignals() should not handle signals — only ctx drives exit.
	ctx, cancel := context.WithCancel(context.Background())
	app := New("test", WithLogger(log.Empty()))
	app.AddServer(newBlockingServer())

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

// regression: sequential HUPs with no concurrent reload must both be processed.
// TryLock is the sole dedup mechanism, not channel capacity.
func TestSignal_SIGHUP_SequentialDelivery(t *testing.T) {
	var count atomic.Int32
	app := New("test",
		WithLogger(log.Empty()),
		WithReloadFunc(func(_ context.Context) error {
			count.Add(1)
			return nil // fast reload — no overlap
		}),
	)
	app.AddServer(newBlockingServer())

	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), WithSignals()) }()

	time.Sleep(50 * time.Millisecond)

	// Two HUPs with enough gap for the first reload to complete.
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(100 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(100 * time.Millisecond)

	if c := count.Load(); c != 2 {
		t.Fatalf("expected 2 sequential reloads, got %d", c)
	}

	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	<-done
}

// regression: no-server scenario must handle repeated SIGHUPs without
// leaking signal watchers (was recursive, now a loop).
func TestSignal_SIGHUP_NoServers_RepeatedReload(t *testing.T) {
	var count atomic.Int32
	app := New("test",
		WithLogger(log.Empty()),
		WithReloadFunc(func(_ context.Context) error {
			count.Add(1)
			return nil
		}),
	)
	// No servers registered — exercises waitNoServers path.

	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), WithSignals()) }()

	time.Sleep(50 * time.Millisecond)

	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(100 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(100 * time.Millisecond)

	if c := count.Load(); c != 2 {
		t.Fatalf("expected 2 reloads with no servers, got %d", c)
	}

	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	<-done
}

func TestReloadTimeout_OnlyLogs(t *testing.T) {
	app := New("test",
		WithLogger(log.Empty()),
		WithReloadTimeout(50*time.Millisecond),
		WithReloadFunc(func(ctx context.Context) error {
			// Block longer than timeout.
			<-ctx.Done()
			return ctx.Err()
		}),
	)
	app.AddServer(newBlockingServer())

	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), WithSignals()) }()

	time.Sleep(50 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(200 * time.Millisecond) // wait for reload to timeout

	// App should still be running.
	syscall.Kill(os.Getpid(), syscall.SIGTERM)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}
