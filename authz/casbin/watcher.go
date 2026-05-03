package casbin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/casbin/casbin/v3/persist"
	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/rid"
)

// redisWatcher is chok's persist.Watcher implementation over the
// existing parts.RedisComponent. Casbin's enforcer.AddPolicy /
// RemovePolicy / SavePolicy paths call Watcher.Update after a
// successful adapter write; peer instances subscribed to the same
// channel receive the publish and reload their policy.
//
// We deliberately implement only the base persist.Watcher
// (SetUpdateCallback / Update / Close) — not WatcherEx or
// UpdatableWatcher. Casbin v3.10.0's notification path falls back to
// Update() when the per-mutation methods aren't available
// (internal_api.go:392 / :437 / :456), and the receive-side callback
// is enforcer.LoadPolicy which reloads the entire model regardless.
// Going incremental only matters when policy mutation rate is high
// enough that full LoadPolicy on every change becomes a bottleneck;
// the simple base contract has fewer moving parts to break.
//
// Self-publish suppression: every Update tags its payload with the
// watcher's instance ID, and the subscriber loop drops messages
// matching its own ID. Without this the publishing pod would also
// reload its own policy on every write — correct but wasteful.
//
// Lifecycle: the subscriber goroutine runs under a cancellable
// context separate from the chok Init context (which is bounded by
// a startup timeout). Close cancels that context, closes the
// underlying *redis.PubSub to unblock the Channel() drain, then
// waits for the goroutine to exit before returning. After Close
// returns no further callbacks can fire — *casbinAuthorizer.Close
// relies on this so the enforcer becomes safe to GC (otherwise the
// subscriber goroutine would hold a live reference via the closure).
type redisWatcher struct {
	client     *goredis.Client
	channel    string
	instanceID string

	pubsub *goredis.PubSub
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.RWMutex
	callback func(string)

	closed atomic.Bool
}

// newRedisWatcher subscribes to channel on client and starts the
// subscriber goroutine. Returns ready-to-use *redisWatcher or a
// structured error when the subscription handshake fails.
//
// ctx is used only for the initial Subscribe + Receive handshake;
// the subscriber goroutine runs under a fresh context-with-cancel
// derived from context.Background so a bounded Init timeout cannot
// kill the long-running listener.
func newRedisWatcher(ctx context.Context, client *goredis.Client, channel string) (*redisWatcher, error) {
	if client == nil {
		return nil, errors.New("authz/casbin watcher: nil redis client")
	}
	if channel == "" {
		return nil, errors.New("authz/casbin watcher: empty channel name")
	}

	pubsub := client.Subscribe(ctx, channel)
	// The first message Receive returns is a *Subscription
	// confirmation (not a payload). Waiting for it ensures the
	// subscription is registered server-side before Update can race
	// against an unsubscribed peer.
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, fmt.Errorf("authz/casbin watcher: subscribe %q: %w", channel, err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	w := &redisWatcher{
		client:     client,
		channel:    channel,
		instanceID: rid.New("ciw"),
		pubsub:     pubsub,
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	go w.run(runCtx)
	return w, nil
}

// run is the subscriber loop. Exits when either the runtime context
// is cancelled (Close path) or the pubsub channel closes (peer
// disconnect / pubsub.Close).
//
// On every received message we re-take the callback under RLock so a
// concurrent SetUpdateCallback during steady-state operation cannot
// race against the dispatch.
func (w *redisWatcher) run(ctx context.Context) {
	defer close(w.done)
	ch := w.pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if msg.Payload == w.instanceID {
				continue // self-publish; ignore
			}
			w.mu.RLock()
			cb := w.callback
			w.mu.RUnlock()
			if cb != nil {
				cb(msg.Payload)
			}
		}
	}
}

// SetUpdateCallback implements persist.Watcher. SyncedEnforcer wires
// this to its LoadPolicy on enforcer construction; chok callers that
// inject a custom Authorizer can override.
//
// Storing under Lock + reading under RLock makes a callback swap
// safe against an in-flight dispatch (next message will see the new
// callback; the in-flight one finishes against the old).
func (w *redisWatcher) SetUpdateCallback(fn func(string)) error {
	if w.closed.Load() {
		return errors.New("authz/casbin watcher: SetUpdateCallback after Close")
	}
	w.mu.Lock()
	w.callback = fn
	w.mu.Unlock()
	return nil
}

// Update implements persist.Watcher. Casbin calls this after every
// successful policy mutation; the payload is our instance ID so the
// subscriber loop on the publishing pod can suppress the round-trip.
//
// We use context.Background for Publish because the call is fire-
// and-forget: the publisher doesn't wait for peers to ack, and a
// timed-out request context would either block until the deadline
// or leave the message unsent — the former wastes the goroutine,
// the latter loses the notification. Redis pub/sub itself is
// at-most-once; if the publish drops nothing recovers it short of
// the operator running a periodic LoadPolicy.
func (w *redisWatcher) Update() error {
	if w.closed.Load() {
		return errors.New("authz/casbin watcher: Update after Close")
	}
	if err := w.client.Publish(context.Background(), w.channel, w.instanceID).Err(); err != nil {
		return fmt.Errorf("authz/casbin watcher publish: %w", err)
	}
	return nil
}

// Close implements persist.Watcher. Idempotent; safe to call from
// multiple goroutines (the atomic CAS guards single-shot teardown).
//
// Order matters: cancel the runtime context first so the subscriber
// loop's select sees Done() before any straggling pubsub message,
// then Close the pubsub to unblock Channel() if the loop happened to
// be parked on the receive case, then wait for goroutine exit before
// clearing the callback. After this returns the *casbinAuthorizer
// can be GC'd safely (no goroutine holds it via the callback
// closure).
//
// persist.Watcher.Close has a void signature, so any Pubsub.Close
// error here is swallowed — the goroutine drain is the actual
// shutdown semantics callers care about.
func (w *redisWatcher) Close() {
	if !w.closed.CompareAndSwap(false, true) {
		return
	}
	w.cancel()
	_ = w.pubsub.Close()
	<-w.done
	w.mu.Lock()
	w.callback = nil
	w.mu.Unlock()
}

// Compile-time interface assertion. Drift in persist.Watcher would
// otherwise only surface when SyncedEnforcer.SetWatcher type-checks
// at runtime.
var _ persist.Watcher = (*redisWatcher)(nil)
