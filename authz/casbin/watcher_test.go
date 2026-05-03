package casbin

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

// newWatcherClient spins up an in-process miniredis and returns a
// connected go-redis client. miniredis.RunT registers t.Cleanup
// internally, so the server tears down with the test.
func newWatcherClient(t *testing.T) *goredis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	c := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// awaitCondition spins on cond up to total, returning whether the
// condition became true. Used for "peer received the publish" checks
// — sleep-based assertions would either flake under CI load or pad
// the test runtime unnecessarily.
func awaitCondition(t *testing.T, total time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestRedisWatcher_PublishTriggersPeerCallback exercises the core
// multi-instance contract from SPEC §9.3: pod A's policy mutation
// must trigger pod B's reload-callback. Two watchers share a
// miniredis; A.Update() lands on B's callback.
func TestRedisWatcher_PublishTriggersPeerCallback(t *testing.T) {
	c := newWatcherClient(t)
	wA, err := newRedisWatcher(context.Background(), c, "test:authz:policy")
	if err != nil {
		t.Fatal(err)
	}
	defer wA.Close()
	wB, err := newRedisWatcher(context.Background(), c, "test:authz:policy")
	if err != nil {
		t.Fatal(err)
	}
	defer wB.Close()

	var bSeen atomic.Int32
	if err := wB.SetUpdateCallback(func(string) { bSeen.Add(1) }); err != nil {
		t.Fatal(err)
	}

	if err := wA.Update(); err != nil {
		t.Fatalf("A.Update: %v", err)
	}
	if !awaitCondition(t, time.Second, func() bool { return bSeen.Load() == 1 }) {
		t.Fatalf("B never saw A's publish (seen=%d)", bSeen.Load())
	}
}

// TestRedisWatcher_SuppressesSelfPublish verifies the publishing
// pod's own subscriber drops its own messages — without this every
// AddPolicy on pod A would also trigger A's LoadPolicy, doubling the
// adapter round-trips for no benefit.
func TestRedisWatcher_SuppressesSelfPublish(t *testing.T) {
	c := newWatcherClient(t)
	w, err := newRedisWatcher(context.Background(), c, "test:authz:policy")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	var seen atomic.Int32
	if err := w.SetUpdateCallback(func(string) { seen.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if err := w.Update(); err != nil {
		t.Fatal(err)
	}
	// Give a peer-side dispatch window roughly equal to the positive
	// test's award window, then assert nothing fired.
	time.Sleep(150 * time.Millisecond)
	if got := seen.Load(); got != 0 {
		t.Errorf("self-publish suppression failed: callback fired %d times", got)
	}
}

// TestRedisWatcher_MultipleSubscribersAllReceive verifies fan-out:
// when A publishes, both B and C see the message. Pins the
// subscribe-pool semantics under more realistic multi-pod load.
func TestRedisWatcher_MultipleSubscribersAllReceive(t *testing.T) {
	c := newWatcherClient(t)
	wA, _ := newRedisWatcher(context.Background(), c, "test:authz:policy")
	defer wA.Close()
	wB, _ := newRedisWatcher(context.Background(), c, "test:authz:policy")
	defer wB.Close()
	wC, _ := newRedisWatcher(context.Background(), c, "test:authz:policy")
	defer wC.Close()

	var bSeen, cSeen atomic.Int32
	_ = wB.SetUpdateCallback(func(string) { bSeen.Add(1) })
	_ = wC.SetUpdateCallback(func(string) { cSeen.Add(1) })

	if err := wA.Update(); err != nil {
		t.Fatal(err)
	}
	if !awaitCondition(t, time.Second, func() bool {
		return bSeen.Load() == 1 && cSeen.Load() == 1
	}) {
		t.Fatalf("fan-out incomplete: B=%d C=%d", bSeen.Load(), cSeen.Load())
	}
}

// TestRedisWatcher_CloseStopsCallback covers the SPEC §7.4 shutdown
// contract: after Close returns the subscriber goroutine has exited
// and the callback can no longer fire. Without this guarantee the
// *casbinAuthorizer would still be reachable through the
// goroutine's closure and never get GC'd, and a late peer publish
// arriving mid-shutdown could call LoadPolicy on a torn-down
// enforcer.
func TestRedisWatcher_CloseStopsCallback(t *testing.T) {
	c := newWatcherClient(t)
	wA, _ := newRedisWatcher(context.Background(), c, "test:authz:policy")
	defer wA.Close()
	wB, _ := newRedisWatcher(context.Background(), c, "test:authz:policy")

	var bSeen atomic.Int32
	_ = wB.SetUpdateCallback(func(string) { bSeen.Add(1) })

	wB.Close()

	if err := wA.Update(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := bSeen.Load(); got != 0 {
		t.Errorf("callback fired %d times after Close — drain semantics broken", got)
	}
}

// TestRedisWatcher_CloseIsIdempotent pins the registry teardown
// contract: Close may be called more than once (defensive double-
// close in nested cleanup paths) without panicking on a re-cancel of
// the already-cancelled context or a re-close of the pubsub.
func TestRedisWatcher_CloseIsIdempotent(t *testing.T) {
	c := newWatcherClient(t)
	w, err := newRedisWatcher(context.Background(), c, "test:authz:policy")
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	w.Close() // must not panic
}

// TestRedisWatcher_UpdateAfterClose_Errors pins the post-close write
// path: callers (Casbin's enforcer post-mutation hook) must receive
// a structured error rather than a panic when the watcher is gone.
func TestRedisWatcher_UpdateAfterClose_Errors(t *testing.T) {
	c := newWatcherClient(t)
	w, _ := newRedisWatcher(context.Background(), c, "test:authz:policy")
	w.Close()
	if err := w.Update(); err == nil {
		t.Fatal("Update after Close must error")
	}
}

// TestRedisWatcher_SetCallbackAfterClose_Errors guards against late
// re-wiring: if some teardown path tries to install a new callback
// after Close it should fail-fast rather than silently swallow the
// callback that will never fire.
func TestRedisWatcher_SetCallbackAfterClose_Errors(t *testing.T) {
	c := newWatcherClient(t)
	w, _ := newRedisWatcher(context.Background(), c, "test:authz:policy")
	w.Close()
	if err := w.SetUpdateCallback(func(string) {}); err == nil {
		t.Fatal("SetUpdateCallback after Close must error")
	}
}

// TestRedisWatcher_NilClient_Rejected covers the constructor's
// defensive check — a misconfigured Builder would otherwise produce
// a watcher whose Update / Close panic on the first call.
func TestRedisWatcher_NilClient_Rejected(t *testing.T) {
	if _, err := newRedisWatcher(context.Background(), nil, "test:authz:policy"); err == nil {
		t.Fatal("nil client should be rejected at construction")
	}
}

// TestRedisWatcher_EmptyChannel_Rejected mirrors the SPEC default-
// fallback path: chok normalises to "chok:authz:policy" before
// reaching newRedisWatcher, so an empty channel here means a
// programming error upstream.
func TestRedisWatcher_EmptyChannel_Rejected(t *testing.T) {
	c := newWatcherClient(t)
	if _, err := newRedisWatcher(context.Background(), c, ""); err == nil {
		t.Fatal("empty channel should be rejected at construction")
	}
}

// TestRedisWatcher_CallbackSwap covers the runtime re-wire case:
// SyncedEnforcer is constructed with one callback, but a custom
// integration could swap to a different reload routine. The next
// peer publish must dispatch to the new callback, not the old.
func TestRedisWatcher_CallbackSwap(t *testing.T) {
	c := newWatcherClient(t)
	wA, _ := newRedisWatcher(context.Background(), c, "test:authz:policy")
	defer wA.Close()
	wB, _ := newRedisWatcher(context.Background(), c, "test:authz:policy")
	defer wB.Close()

	var oldFired, newFired atomic.Int32
	_ = wB.SetUpdateCallback(func(string) { oldFired.Add(1) })
	_ = wB.SetUpdateCallback(func(string) { newFired.Add(1) })

	if err := wA.Update(); err != nil {
		t.Fatal(err)
	}
	if !awaitCondition(t, time.Second, func() bool { return newFired.Load() == 1 }) {
		t.Fatalf("new callback never fired (old=%d new=%d)", oldFired.Load(), newFired.Load())
	}
	if got := oldFired.Load(); got != 0 {
		t.Errorf("old callback fired %d times after swap", got)
	}
}
