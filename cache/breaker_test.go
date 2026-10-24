package cache

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// fakeLayer stands in for Redis behind the breaker.
type fakeLayer struct {
	mu      sync.Mutex
	data    []byte
	found   bool
	err     error
	block   chan struct{} // when set, calls wait for it to close or for ctx
	lastCtx context.Context
	calls   atomic.Int32
}

func (f *fakeLayer) call(ctx context.Context) error {
	f.calls.Add(1)
	f.mu.Lock()
	f.lastCtx = ctx
	block, err := f.block, f.err
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeLayer) get(ctx context.Context, _ string) ([]byte, bool, error) {
	if err := f.call(ctx); err != nil {
		return nil, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data, f.found, nil
}

func (f *fakeLayer) set(ctx context.Context, _ string, _ []byte, _ time.Duration) error {
	return f.call(ctx)
}

func (f *fakeLayer) del(ctx context.Context, _ string) error { return f.call(ctx) }

func (f *fakeLayer) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeLayer) answer(data string) {
	f.mu.Lock()
	f.err, f.data, f.found = nil, []byte(data), true
	f.mu.Unlock()
}

var errDown = errors.New("redis down")

func testBreaker(inner layer, threshold int, reset time.Duration, successes int) *breaker {
	return newBreaker(inner, threshold, reset, successes, 2*time.Second, slog.New(slog.DiscardHandler))
}

func (b *breaker) current() breakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func TestBreaker_ClosedState_Passthrough(t *testing.T) {
	inner := &fakeLayer{data: []byte("hello"), found: true}
	b := testBreaker(inner, 3, time.Second, 3)
	data, ok, err := b.get(t.Context(), "k")
	if err != nil || !ok || string(data) != "hello" {
		t.Fatalf("closed breaker should pass through: data=%s ok=%v err=%v", data, ok, err)
	}
}

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	inner := &fakeLayer{err: errDown}
	b := testBreaker(inner, 3, time.Hour, 3)
	for range 3 {
		_, _, _ = b.get(t.Context(), "k")
	}
	calls := inner.calls.Load()
	data, ok, err := b.get(t.Context(), "k")
	if err != nil || ok || data != nil {
		t.Fatalf("open breaker returns a silent miss: %q %v %v", data, ok, err)
	}
	if inner.calls.Load() != calls || b.current() != open {
		t.Fatal("an open breaker does not call Redis")
	}
}

func TestBreaker_SetSkipsWhenOpen(t *testing.T) {
	inner := &fakeLayer{err: errDown}
	b := testBreaker(inner, 2, time.Hour, 3)
	for range 2 {
		_ = b.set(t.Context(), "k", []byte("v"), 0)
	}
	calls := inner.calls.Load()
	if err := b.set(t.Context(), "k", []byte("v"), 0); err != nil {
		t.Fatalf("open breaker Set returns nil, got %v", err)
	}
	if inner.calls.Load() != calls {
		t.Fatal("open breaker does not call Redis on Set")
	}
}

func TestBreaker_DeleteSkipsWhenOpen(t *testing.T) {
	inner := &fakeLayer{err: errDown}
	b := testBreaker(inner, 2, time.Hour, 3)
	for range 2 {
		_ = b.del(t.Context(), "k")
	}
	calls := inner.calls.Load()
	if err := b.del(t.Context(), "k"); err != nil {
		t.Fatalf("open breaker Delete returns nil, got %v", err)
	}
	if inner.calls.Load() != calls {
		t.Fatal("open breaker does not call Redis on Delete")
	}
}

func TestBreaker_GetDoesNotPropagateError(t *testing.T) {
	b := testBreaker(&fakeLayer{err: errDown}, 10, time.Hour, 3)
	// Even before the circuit opens, errors degrade to misses.
	if _, ok, err := b.get(t.Context(), "k"); err != nil || ok {
		t.Fatalf("a failed Get is a silent miss: %v %v", ok, err)
	}
}

func TestBreaker_HalfOpen_SuccessCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &fakeLayer{err: errDown}
		b := testBreaker(inner, 2, 10*time.Second, 1)
		for range 2 {
			_, _, _ = b.get(t.Context(), "k")
		}
		time.Sleep(10 * time.Second)
		inner.answer("recovered")
		if data, ok, err := b.get(t.Context(), "k"); err != nil || !ok || string(data) != "recovered" {
			t.Fatalf("the probe's answer: %q %v %v", data, ok, err)
		}
		if b.current() != closed {
			t.Fatal("one successful probe closes it when one is asked for")
		}
		calls := inner.calls.Load()
		if _, ok, _ := b.get(t.Context(), "k"); !ok || inner.calls.Load() != calls+1 {
			t.Fatal("a closed breaker passes calls through")
		}
	})
}

// TestBreaker_HalfOpen_RequiresMultipleSuccesses: with the default of
// three, one lucky probe does not close the breaker; the next probes go
// one after another, without another reset_timeout each (v2 made each
// wait two of them, so closing took five), and a failure in between
// starts over.
func TestBreaker_HalfOpen_RequiresMultipleSuccesses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &fakeLayer{err: errDown}
		b := testBreaker(inner, 2, 10*time.Second, 3)
		for range 2 {
			_, _, _ = b.get(t.Context(), "k")
		}
		time.Sleep(10 * time.Second)
		inner.answer("ok")
		for i := range 2 {
			if _, ok, _ := b.get(t.Context(), "k"); !ok || b.current() != halfOpen {
				t.Fatalf("probe %d: hit %v, state %v; want a hit and still half-open", i+1, ok, b.current())
			}
		}
		inner.fail(errDown)
		_, _, _ = b.get(t.Context(), "k")
		if b.current() != open {
			t.Fatal("a failed probe opens it again")
		}
		time.Sleep(10 * time.Second)
		inner.answer("ok")
		for i := range 3 {
			if _, ok, _ := b.get(t.Context(), "k"); !ok {
				t.Fatalf("probe %d is a hit", i+1)
			}
		}
		if b.current() != closed {
			t.Fatalf("three successful probes in a row close it, state %v", b.current())
		}
	})
}

func TestBreaker_HalfOpen_FailureReopens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &fakeLayer{err: errDown}
		b := testBreaker(inner, 2, 10*time.Second, 3)
		for range 2 {
			_, _, _ = b.get(t.Context(), "k")
		}
		time.Sleep(10 * time.Second)
		calls := inner.calls.Load()
		_, _, _ = b.get(t.Context(), "k")
		if inner.calls.Load() != calls+1 {
			t.Fatal("half-open lets one probe through")
		}
		calls = inner.calls.Load()
		_, _, _ = b.get(t.Context(), "k")
		if inner.calls.Load() != calls || b.current() != open {
			t.Fatal("the failed probe opened it again: Redis is left alone")
		}
		time.Sleep(10 * time.Second)
		_, _, _ = b.get(t.Context(), "k")
		if inner.calls.Load() != calls+1 {
			t.Fatal("after another reset_timeout the next probe goes")
		}
	})
}

// TestBreaker_HalfOpenAdmitsOneProbeAtATime: while a probe is in flight
// the other calls are misses that do not reach Redis.
func TestBreaker_HalfOpenAdmitsOneProbeAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &fakeLayer{err: errDown}
		b := testBreaker(inner, 1, 10*time.Second, 1)
		_, _, _ = b.get(t.Context(), "k")
		time.Sleep(10 * time.Second)
		release := make(chan struct{})
		inner.mu.Lock()
		inner.err, inner.data, inner.found, inner.block = nil, []byte("v"), true, release
		inner.mu.Unlock()
		probed := make(chan bool)
		go func() {
			_, ok, _ := b.get(t.Context(), "k")
			probed <- ok
		}()
		synctest.Wait() // the probe waits in Redis
		calls := inner.calls.Load()
		if _, ok, err := b.get(t.Context(), "k"); ok || err != nil || inner.calls.Load() != calls {
			t.Fatal("a second call during the probe is a miss without Redis")
		}
		close(release)
		if !<-probed || b.current() != closed {
			t.Fatal("the probe succeeds and closes it")
		}
	})
}

// TestBreaker_HungProbeReopens: a probe still in flight after a whole
// reset_timeout (Redis ignoring its deadline) counts as failed.
func TestBreaker_HungProbeReopens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hang := make(chan struct{})
		inner := &fakeLayer{err: errDown}
		b := newBreaker(&hangingLayer{fakeLayer: inner, hang: hang}, 1, 10*time.Second, 1, 2*time.Second, slog.New(slog.DiscardHandler))
		_, _, _ = b.get(t.Context(), "k")
		time.Sleep(10 * time.Second)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _, _ = b.get(t.Context(), "k") // the probe hangs
		}()
		synctest.Wait()
		time.Sleep(10 * time.Second)
		if _, ok, _ := b.get(t.Context(), "k"); ok || b.current() != open {
			t.Fatalf("the hung probe reopened it: state %v", b.current())
		}
		close(hang)
		<-done
	})
}

// hangingLayer ignores the context of its calls until hang closes.
type hangingLayer struct {
	*fakeLayer
	hang chan struct{}
}

func (h *hangingLayer) get(ctx context.Context, key string) ([]byte, bool, error) {
	if h.calls.Load() > 0 {
		h.calls.Add(1)
		<-h.hang
		return nil, false, errDown
	}
	return h.fakeLayer.get(ctx, key)
}

// TestBreaker_ProbeHasItsOwnTimeout: the probe runs under probe_timeout,
// not under the caller's nearly spent deadline, and keeps its values.
func TestBreaker_ProbeHasItsOwnTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &fakeLayer{err: errDown}
		b := testBreaker(inner, 1, 10*time.Second, 1)
		_, _, _ = b.get(t.Context(), "k")
		time.Sleep(10 * time.Second)
		inner.mu.Lock()
		inner.err, inner.block = nil, make(chan struct{}) // Redis never answers
		inner.mu.Unlock()
		type key struct{}
		ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), key{}, "request-7"), 10*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, _, _ = b.get(ctx, "k")
		if took := time.Since(start); took != 2*time.Second {
			t.Fatalf("the probe ran %s; want probe_timeout, 2s, not the caller's 10ms", took)
		}
		inner.mu.Lock()
		probeCtx := inner.lastCtx
		inner.mu.Unlock()
		if probeCtx.Value(key{}) != "request-7" {
			t.Fatal("the probe keeps the caller's values")
		}
		if b.current() != open {
			t.Fatal("a probe that timed out failed")
		}
	})
}

// TestBreaker_CallerCancellationIsNotAFailure: calls that fail because
// their caller gave up do not open the breaker.
func TestBreaker_CallerCancellationIsNotAFailure(t *testing.T) {
	inner := &fakeLayer{block: make(chan struct{})}
	b := testBreaker(inner, 2, time.Hour, 3)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 5 {
		_, _, _ = b.get(ctx, "k")
	}
	if b.current() != closed {
		t.Fatal("canceled callers opened the breaker")
	}
	inner.mu.Lock()
	inner.block, inner.err = nil, errDown
	inner.mu.Unlock()
	for range 2 {
		_, _, _ = b.get(t.Context(), "k")
	}
	if b.current() != open {
		t.Fatal("Redis's own failures still open it")
	}
}
