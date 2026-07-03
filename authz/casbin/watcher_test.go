package casbin

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/v2/rid"
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
// *Engine would still be reachable through the
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

// TestRedisWatcher_CoalescesBurstReloads pins the round-1 review fix
// to perspective #3 (pubsub buffer overflow). receiveLoop and
// reloadLoop are decoupled via a size-1 reload chan; a burst of N
// peer publishes while reloadLoop is parked in cb produces at most
// 2 callback fires (the in-flight one + one coalesced). LoadPolicy
// is full-reload so coalescing is correctness-preserving — duplicate
// fires read the same DB rows and waste a round-trip.
func TestRedisWatcher_CoalescesBurstReloads(t *testing.T) {
	c := newWatcherClient(t)
	wA, _ := newRedisWatcher(context.Background(), c, "test:authz:policy")
	defer wA.Close()
	wB, _ := newRedisWatcher(context.Background(), c, "test:authz:policy")
	defer wB.Close()

	var fired atomic.Int32
	release := make(chan struct{})
	defer close(release) // unblock any parked callback before the watcher Close drains
	if err := wB.SetUpdateCallback(func(string) {
		fired.Add(1)
		<-release
	}); err != nil {
		t.Fatal(err)
	}

	// Burst publishes — receiveLoop signals reloadLoop for each, but
	// reloadLoop is parked in cb after the first signal so the rest
	// coalesce into the size-1 reload chan.
	const burst = 8
	for range burst {
		if err := wA.Update(); err != nil {
			t.Fatal(err)
		}
	}

	// First fire arrives quickly and parks in <-release.
	if !awaitCondition(t, time.Second, func() bool { return fired.Load() >= 1 }) {
		t.Fatalf("first callback never fired (fired=%d)", fired.Load())
	}
	// Even with all 8 publishes already drained into receiveLoop,
	// reloadLoop is parked in cb and only 1 extra signal can sit in
	// the size-1 reload chan. Verify the in-flight observation is
	// stable.
	time.Sleep(100 * time.Millisecond)
	if got := fired.Load(); got != 1 {
		t.Errorf("expected fired==1 while reloadLoop parked in cb, got %d", got)
	}

	// Releasing once lets cb#1 return; reloadLoop picks the queued
	// signal and runs cb#2. The remaining 6 publishes were coalesced
	// into that single queued signal — they do NOT produce additional
	// callbacks.
	release <- struct{}{}
	if !awaitCondition(t, time.Second, func() bool { return fired.Load() == 2 }) {
		t.Fatalf("coalesced callback never fired (fired=%d)", fired.Load())
	}
	// Allow extra time and confirm no third fire materialises.
	time.Sleep(100 * time.Millisecond)
	if got := fired.Load(); got != 2 {
		t.Errorf("expected exactly 2 coalesced fires for %d publishes, got %d", burst, got)
	}
	// Drain cb#2's <-release so deferred close(release) doesn't race
	// against an already-blocked sender.
	release <- struct{}{}
}

// TestRedisWatcher_UpdateSwallowsPublishErrors pins the round-1
// review fix to the High finding: Casbin's enforcer.AddPolicy returns
// Watcher.Update's error verbatim (internal_api.go:394), and chok's
// Service propagates it as a 500 to the HTTP caller despite the DB
// row already being committed. Update() now logs and returns nil so
// a transient Redis hiccup doesn't poison an otherwise-successful
// mutation. Peer pods may stay stale until the next mutation, which
// is the documented at-most-once semantic.
func TestRedisWatcher_UpdateSwallowsPublishErrors(t *testing.T) {
	mr := miniredis.RunT(t)
	c := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	w, err := newRedisWatcher(context.Background(), c, "test:authz:policy",
		withWatcherPublishTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Kill the redis backend; subsequent Publish must fail at the
	// transport layer. Update is required to swallow the error so
	// the Service-level mutation isn't poisoned.
	mr.Close()

	if err := w.Update(); err != nil {
		t.Errorf("Update should swallow publish errors as best-effort, got %v", err)
	}
}

// TestRedisWatcher_UpdateRespectsPublishTimeout verifies the bounded
// Publish timeout caps hot-path latency. miniredis publishes are
// near-instant, so we only assert that Update returns within a
// generous wall-clock budget — the real value of this test is
// pinning the option wiring (withWatcherPublishTimeout actually
// reaches client.Publish via context.WithTimeout in Update).
func TestRedisWatcher_UpdateRespectsPublishTimeout(t *testing.T) {
	c := newWatcherClient(t)
	w, err := newRedisWatcher(context.Background(), c, "test:authz:policy",
		withWatcherPublishTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	start := time.Now()
	if err := w.Update(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Update took %v on miniredis (publish timeout 200ms) — option not wired?", elapsed)
	}
}

// TestRedisWatcher_SetCallbackVsClose_ConcurrentRace exercises the
// round-1 review fix for the SetUpdateCallback / Close TOCTOU. The
// previous code read closed.Load() *outside* the mutex, allowing a
// concurrent Close to fully complete between the load and the lock
// acquire — leaving a non-nil callback on a watcher whose run
// goroutines had already exited. The fix moves the closed check
// inside the lock; this test runs many goroutines racing the two
// methods and asserts the post-state invariant: after Close
// returns and any concurrent SetUpdateCallback calls return,
// w.callback must be nil.
func TestRedisWatcher_SetCallbackVsClose_ConcurrentRace(t *testing.T) {
	for trial := range 50 {
		c := newWatcherClient(t)
		w, err := newRedisWatcher(context.Background(), c, "test:authz:policy")
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// Best-effort install; we don't care about the return.
			_ = w.SetUpdateCallback(func(string) {})
		}()
		go func() {
			defer wg.Done()
			w.Close()
		}()
		wg.Wait()

		w.mu.Lock()
		cb := w.callback
		w.mu.Unlock()
		if cb != nil {
			t.Fatalf("trial %d: callback non-nil after concurrent Close — race window still open", trial)
		}
	}
}

// TestRedisWatcher_IntegrationRealRedis is gated on REDIS_TEST_ADDR;
// when set, exercises the watcher against a real go-redis backend so
// the buffer-overflow drop path, real reconnection semantics, and
// network-RTT publish behaviour get coverage that miniredis can't
// provide. Skipped silently in CI runs without the env var.
//
// Sub-cases:
//
//   - "publish_round_trip": baseline A.Update → B.callback delivery
//   - "reconnect_after_kill": forcibly close every PubSub connection
//     server-side via CLIENT KILL, then verify a subsequent Update
//     still reaches the peer once go-redis auto-reconnects. This
//     pins the round-1 review concern that miniredis can't model
//     server-side connection drops.
func TestRedisWatcher_IntegrationRealRedis(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR not set; skipping real-Redis integration")
	}
	c := goredis.NewClient(&goredis.Options{Addr: addr})
	t.Cleanup(func() { _ = c.Close() })

	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx).Err(); err != nil {
		t.Skipf("REDIS_TEST_ADDR (%s) unreachable: %v", addr, err)
	}

	// Per-run channel keeps parallel test invocations on a shared
	// Redis instance from cross-firing each other's callbacks.
	channel := "test:authz:policy:" + rid.New("itest")
	wA, err := newRedisWatcher(context.Background(), c, channel)
	if err != nil {
		t.Fatal(err)
	}
	defer wA.Close()
	wB, err := newRedisWatcher(context.Background(), c, channel)
	if err != nil {
		t.Fatal(err)
	}
	defer wB.Close()

	var seen atomic.Int32
	if err := wB.SetUpdateCallback(func(string) { seen.Add(1) }); err != nil {
		t.Fatal(err)
	}

	t.Run("publish_round_trip", func(t *testing.T) {
		base := seen.Load()
		if err := wA.Update(); err != nil {
			t.Fatal(err)
		}
		// Real-Redis RTT: 5s budget covers high-latency cross-AZ links.
		if !awaitCondition(t, 5*time.Second, func() bool { return seen.Load() == base+1 }) {
			t.Fatalf("real-Redis publish never reached peer (seen=%d, want=%d)", seen.Load(), base+1)
		}
	})

	t.Run("reconnect_after_kill", func(t *testing.T) {
		// Boot a side client to issue CLIENT KILL TYPE pubsub on the
		// real server. This severs every active SUBSCRIBE connection
		// — go-redis v9's PubSub drives an automatic reconnect on the
		// next operation.
		admin := goredis.NewClient(&goredis.Options{Addr: addr})
		t.Cleanup(func() { _ = admin.Close() })

		killCtx, killCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer killCancel()
		if _, err := admin.Do(killCtx, "CLIENT", "KILL", "TYPE", "pubsub").Result(); err != nil {
			t.Fatalf("CLIENT KILL TYPE pubsub: %v", err)
		}

		// Give go-redis a moment to notice and reconnect on the next
		// PubSub operation. The library is lazy — reconnect happens
		// when receiveLoop's pubsub.Channel() either errors or is
		// drained for a new message.
		base := seen.Load()
		// Fire a few publishes spaced out; the watcher only needs ONE
		// to land post-reconnect for full sync semantics.
		var lastErr error
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if err := wA.Update(); err != nil {
				lastErr = err
			}
			if seen.Load() > base {
				return // success
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("after CLIENT KILL pubsub, no publish landed within deadline (seen base=%d now=%d, last update err=%v)",
			base, seen.Load(), lastErr)
	})
}

// TestRedisWatcher_StatsTracksPublishFailures covers the round-3
// observability addition: a publish error increments
// WatcherStats.PublishFailures. miniredis's mr.Close() makes the
// next Publish fail at the transport layer, exercising the
// best-effort log + counter path without coupling to a real Redis.
func TestRedisWatcher_StatsTracksPublishFailures(t *testing.T) {
	mr := miniredis.RunT(t)
	c := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	w, err := newRedisWatcher(context.Background(), c, "test:authz:policy",
		withWatcherPublishTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Sanity: counter starts at 0.
	if got := w.Stats(); got.PublishFailures != 0 {
		t.Fatalf("baseline PublishFailures = %d, want 0", got.PublishFailures)
	}

	mr.Close() // force every subsequent Publish to fail

	if err := w.Update(); err != nil {
		t.Errorf("Update should swallow publish errors, got %v", err)
	}
	if got := w.Stats(); got.PublishFailures != 1 {
		t.Errorf("after one failed publish, PublishFailures = %d, want 1", got.PublishFailures)
	}
	// Second failed publish: counter monotonic.
	_ = w.Update()
	if got := w.Stats(); got.PublishFailures != 2 {
		t.Errorf("after two failed publishes, PublishFailures = %d, want 2", got.PublishFailures)
	}
}
