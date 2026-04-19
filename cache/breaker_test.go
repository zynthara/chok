package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCache is a test double for cache.Cache.
type fakeCache struct {
	getData  []byte
	getFound bool
	err      error
	calls    atomic.Int32
}

func (f *fakeCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	f.calls.Add(1)
	return f.getData, f.getFound, f.err
}
func (f *fakeCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	f.calls.Add(1)
	return f.err
}
func (f *fakeCache) Delete(ctx context.Context, key string) error {
	f.calls.Add(1)
	return f.err
}
func (f *fakeCache) Close() error { return nil }

func TestBreaker_ClosedState_Passthrough(t *testing.T) {
	inner := &fakeCache{getData: []byte("hello"), getFound: true}
	b := WithBreaker(inner, BreakerOptions{Threshold: 3, ResetTimeout: time.Second})

	data, ok, err := b.Get(context.Background(), "k")
	if err != nil || !ok || string(data) != "hello" {
		t.Fatalf("closed breaker should pass through: data=%s ok=%v err=%v", data, ok, err)
	}
}

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	inner := &fakeCache{err: errors.New("redis down")}
	b := WithBreaker(inner, BreakerOptions{Threshold: 3, ResetTimeout: time.Hour})

	// Trigger threshold failures.
	for range 3 {
		b.Get(context.Background(), "k")
	}
	calls := inner.calls.Load()

	// Next call should NOT reach inner (circuit open).
	data, ok, err := b.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("open breaker should not return error, got %v", err)
	}
	if ok {
		t.Fatal("open breaker should return miss")
	}
	if data != nil {
		t.Fatal("open breaker should return nil data")
	}
	if inner.calls.Load() != calls {
		t.Fatal("open breaker should not call inner")
	}
}

func TestBreaker_SetSkipsWhenOpen(t *testing.T) {
	inner := &fakeCache{err: errors.New("redis down")}
	b := WithBreaker(inner, BreakerOptions{Threshold: 2, ResetTimeout: time.Hour})

	// Open the circuit.
	for range 2 {
		b.Set(context.Background(), "k", []byte("v"), 0)
	}
	calls := inner.calls.Load()

	// Set should be silently skipped.
	err := b.Set(context.Background(), "k", []byte("v"), 0)
	if err != nil {
		t.Fatalf("open breaker Set should return nil, got %v", err)
	}
	if inner.calls.Load() != calls {
		t.Fatal("open breaker should not call inner on Set")
	}
}

func TestBreaker_HalfOpen_SuccessCloses(t *testing.T) {
	inner := &fakeCache{err: errors.New("redis down")}
	// HalfOpenSuccesses=1 keeps this test focused on the close
	// transition; the multi-probe flap-damping default is exercised
	// by TestBreaker_HalfOpen_RequiresMultipleSuccesses.
	b := WithBreaker(inner, BreakerOptions{
		Threshold:         2,
		ResetTimeout:      10 * time.Millisecond,
		HalfOpenSuccesses: 1,
	})

	// Open the circuit.
	for range 2 {
		b.Get(context.Background(), "k")
	}

	// Wait for reset timeout.
	time.Sleep(20 * time.Millisecond)

	// Next call is a probe — make it succeed.
	inner.err = nil
	inner.getData = []byte("recovered")
	inner.getFound = true

	data, ok, err := b.Get(context.Background(), "k")
	if err != nil || !ok || string(data) != "recovered" {
		t.Fatalf("half-open probe success should return data: data=%s ok=%v err=%v", data, ok, err)
	}

	// Circuit should be closed — next call should pass through.
	data, ok, err = b.Get(context.Background(), "k")
	if err != nil || !ok || string(data) != "recovered" {
		t.Fatalf("closed breaker should pass through: data=%s ok=%v err=%v", data, ok, err)
	}
}

// TestBreaker_HalfOpen_RequiresMultipleSuccesses verifies the new
// default (HalfOpenSuccesses=3): a single successful probe is not
// enough to close the breaker — three consecutive successes are
// required, dampening the lucky-probe flap-back failure mode that
// occurs when the backend is genuinely unhealthy but hands out one
// or two coincidental wins.
func TestBreaker_HalfOpen_RequiresMultipleSuccesses(t *testing.T) {
	inner := &fakeCache{err: errors.New("redis down")}
	b := WithBreaker(inner, BreakerOptions{
		Threshold:    2,
		ResetTimeout: 10 * time.Millisecond,
	})

	for range 2 {
		b.Get(context.Background(), "k")
	}
	time.Sleep(20 * time.Millisecond)

	inner.err = nil
	inner.getData = []byte("ok")
	inner.getFound = true

	for i := range 3 {
		if _, _, err := b.Get(context.Background(), "k"); err != nil {
			t.Fatalf("probe %d returned error: %v", i, err)
		}
	}
}

func TestBreaker_HalfOpen_FailureReopens(t *testing.T) {
	inner := &fakeCache{err: errors.New("redis down")}
	b := WithBreaker(inner, BreakerOptions{Threshold: 2, ResetTimeout: 10 * time.Millisecond})

	// Open the circuit.
	for range 2 {
		b.Get(context.Background(), "k")
	}

	// Wait for reset timeout.
	time.Sleep(20 * time.Millisecond)

	// Probe call — still failing.
	calls := inner.calls.Load()
	b.Get(context.Background(), "k")
	if inner.calls.Load() <= calls {
		t.Fatal("half-open should allow one probe")
	}
	calls = inner.calls.Load()

	// Circuit should be open again — next call should not reach inner.
	b.Get(context.Background(), "k")
	if inner.calls.Load() != calls {
		t.Fatal("re-opened breaker should not call inner")
	}
}

func TestBreaker_DeleteSkipsWhenOpen(t *testing.T) {
	inner := &fakeCache{err: errors.New("redis down")}
	b := WithBreaker(inner, BreakerOptions{Threshold: 2, ResetTimeout: time.Hour})

	for range 2 {
		b.Delete(context.Background(), "k")
	}
	calls := inner.calls.Load()

	err := b.Delete(context.Background(), "k")
	if err != nil {
		t.Fatalf("open breaker Delete should return nil, got %v", err)
	}
	if inner.calls.Load() != calls {
		t.Fatal("open breaker should not call inner on Delete")
	}
}

func TestBreaker_GetDoesNotPropagateError(t *testing.T) {
	inner := &fakeCache{err: errors.New("redis down")}
	b := WithBreaker(inner, BreakerOptions{Threshold: 10, ResetTimeout: time.Hour})

	// Even before the circuit opens, errors degrade to miss (not propagated).
	_, ok, err := b.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("breaker should not propagate Get error, got %v", err)
	}
	if ok {
		t.Fatal("failed Get should return miss")
	}
}
