package casbin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/casbin/casbin/v3/persist"
	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/rid"
)

// redisWatcher is chok's persist.Watcher implementation over the
// existing parts.RedisComponent. Casbin's enforcer.AddPolicy /
// RemovePolicy / SavePolicy paths call Watcher.Update after a
// successful adapter write; peer instances subscribed to the same
// channel reload their policy.
//
// We deliberately implement only the base persist.Watcher
// (SetUpdateCallback / Update / Close) — not WatcherEx or
// UpdatableWatcher. Casbin v3.10.0's notification path falls back to
// Update() when the per-mutation methods aren't available
// (internal_api.go:392 / :437 / :456), and the receive-side callback
// is enforcer.LoadPolicy which reloads the entire model regardless.
//
// Concurrency model (round-1 review fix):
//
//   - receiveLoop drains pubsub.Channel and signals reloadLoop via a
//     size-1 coalescing channel. It NEVER calls the user callback
//     directly — a slow LoadPolicy would otherwise pin the receive
//     goroutine and let go-redis silently drop messages on a full
//     inbound buffer.
//
//   - reloadLoop owns callback invocation. Bursts of N notifications
//     coalesce into at most 2 callback fires (one in-flight + one
//     queued), which is correct because LoadPolicy is full-reload —
//     duplicate reloads add no information.
//
//   - Update is best-effort fire-and-forget: a failed Publish is
//     logged but does NOT propagate up. Casbin's enforcer.AddPolicy
//     would otherwise return Update's error verbatim
//     (casbin/v3 internal_api.go:394) and surface a transient Redis
//     hiccup as a 500 to the HTTP caller — and the client retry
//     would see Casbin's duplicate-row return (false, nil) without
//     re-attempting the notify, leaving peers silently divergent.
//     The package doc-comment promises "fire-and-forget … at-most-
//     once"; we honour it here.
//
// Self-publish suppression: every Update tags its payload with the
// watcher's instance ID, and receiveLoop drops messages matching its
// own ID before signalling reload.
//
// Lifecycle: both goroutines run under a cancellable context separate
// from the chok Init context (which is bounded by a startup timeout).
// Close cancels that context, closes the underlying *redis.PubSub to
// unblock receiveLoop's Channel() drain, then waits for both
// goroutines to exit before clearing the callback. After Close
// returns no further callbacks can fire.
//
// Failure-mode contract (operators should know):
//
//   - Publish failures (Update path) are best-effort: logged + counted
//     in WatcherStats.PublishFailures and Update returns nil. Casbin's
//     internal_api.go:394 would otherwise propagate the error to the
//     mutation caller despite a successful DB commit, and a client
//     retry would see the duplicate row and never re-attempt notify
//     — peer pods would silently diverge. Best-effort is the only
//     correct stance for at-most-once pub/sub semantics.
//
//   - Reload failures (peer-triggered LoadPolicy returning error) are
//     logged at Error and counted in WatcherStats.ReloadFailures. The
//     subscriber stays alive; the next peer publish will re-attempt.
//     Operators running across slow links should configure a periodic
//     SyncedEnforcer.LoadPolicy as backstop — Redis pub/sub is
//     at-most-once, so a watcher alone cannot guarantee convergence
//     under sustained Redis loss.
//
//   - Callback panics are NOT recovered. In Go an unrecovered panic
//     in a goroutine kills the entire process, which is the correct
//     fail-fast behaviour: a panicking LoadPolicy means an adapter
//     parse bug or model arity drift, not a transient. Adding
//     `defer recover()` here would mask real defects. SPEC §7.4's
//     "Watcher subscriber goroutine 在 App.Stop 之前可靠运转" should
//     be read as "barring fail-fast bugs"; this is documented at the
//     contract level, not enforced via recover.
type redisWatcher struct {
	client     *goredis.Client
	channel    string
	instanceID string
	logger     log.Logger

	pubsub *goredis.PubSub
	cancel context.CancelFunc

	// recvDone closes when receiveLoop exits.
	// reloadDone closes when reloadLoop exits.
	recvDone   chan struct{}
	reloadDone chan struct{}

	// reload is a size-1 buffered channel: a full chan means a
	// reload is already pending and any further notification is
	// coalesced. LoadPolicy is full-reload so duplicate fires add
	// no information.
	reload chan struct{}

	publishTimeout time.Duration

	mu       sync.RWMutex
	callback func(string)

	closed atomic.Bool

	// Best-effort counters. Read via Stats() for ops dashboards
	// (eventually wired to chok metrics; for now they're
	// log-correlation aids in incident review).
	publishFailures atomic.Uint64
	reloadFailures  atomic.Uint64
}

// WatcherStats is a snapshot of the watcher's best-effort counters.
// Returned by *redisWatcher.Stats() and the Service-level
// AuthzWatcherStats() escape hatch. Numbers reflect lifetime totals
// since watcher construction; resets only on process restart so
// alerting should be on rate-of-change, not absolute value.
type WatcherStats struct {
	// PublishFailures counts Update() Publish errors (Redis blip,
	// timeout). Each increment is paired with a logger.Warn entry
	// — peers may be stale until the next mutation.
	PublishFailures uint64

	// ReloadFailures counts peer-triggered LoadPolicy errors. Each
	// increment is paired with a logger.Error entry. Sustained
	// non-zero rate means the receive side is healthy but the local
	// adapter / DB cannot reload.
	ReloadFailures uint64
}

// watcherOption configures redisWatcher at construction. Package-
// private so the surface stays narrow; Builder threads the options
// it cares about (logger), tests use the same hook to override the
// publish timeout.
type watcherOption func(*redisWatcher)

// withWatcherLogger threads chok's per-component Logger into the
// watcher so publish failures and (eventually) overflow drops are
// visible in the operator's log stream rather than printed to
// go-redis' internal.Logger which no chok subsystem captures.
func withWatcherLogger(l log.Logger) watcherOption {
	return func(w *redisWatcher) {
		if l != nil {
			w.logger = l
		}
	}
}

// withWatcherPublishTimeout overrides the default publish deadline.
// Tests use a smaller value to exercise the swallow-on-error path
// without waiting the production default.
func withWatcherPublishTimeout(d time.Duration) watcherOption {
	return func(w *redisWatcher) {
		if d > 0 {
			w.publishTimeout = d
		}
	}
}

const (
	// watcherChannelBufferSize is the inbound message buffer for
	// pubsub.Channel(). go-redis' library default is 100; bulk
	// Bootstrap (SPEC §4.2 admin + N permissions in one batch) can
	// fan that out faster than a slow LoadPolicy drains, and the
	// library response on overflow is to wait chanSendTimeout then
	// drop. 1024 is generous enough for typical Bootstrap fan-in.
	watcherChannelBufferSize = 1024

	// watcherChannelSendTimeout shortens go-redis' default 60s
	// timeout for handing a received message to msgCh. A stuck
	// consumer is now observable in seconds — the warning is
	// logged via go-redis' internal.Logger which operators won't
	// see unless they specifically wire it up, but the receive
	// loop itself recovers in 2s rather than 60s.
	watcherChannelSendTimeout = 2 * time.Second

	// watcherPublishTimeout caps Update's blocking time on the
	// hot request path. Chok's RedisOptions.WriteTimeout default
	// is 500ms but operators can raise it; this floor ensures the
	// authz-mutation handler never stalls more than 1s on a
	// Redis hiccup.
	watcherPublishTimeout = 1 * time.Second
)

// newRedisWatcher subscribes to channel on client and starts the
// receive + reload goroutines. ctx is used only for the initial
// Subscribe + Receive handshake; the long-running goroutines run
// under context.Background-derived ctx so a bounded Init timeout
// can't kill them.
func newRedisWatcher(ctx context.Context, client *goredis.Client, channel string, opts ...watcherOption) (*redisWatcher, error) {
	if client == nil {
		return nil, errors.New("authz/casbin watcher: nil redis client")
	}
	if channel == "" {
		return nil, errors.New("authz/casbin watcher: empty channel name")
	}

	pubsub := client.Subscribe(ctx, channel)
	// First Receive returns the *Subscription confirmation; waiting
	// for it ensures the subscription is live server-side before
	// Update can race against an unsubscribed peer.
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, fmt.Errorf("authz/casbin watcher: subscribe %q: %w", channel, err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	w := &redisWatcher{
		client:         client,
		channel:        channel,
		instanceID:     rid.New("ciw"),
		logger:         log.Empty(),
		pubsub:         pubsub,
		cancel:         cancel,
		recvDone:       make(chan struct{}),
		reloadDone:     make(chan struct{}),
		reload:         make(chan struct{}, 1),
		publishTimeout: watcherPublishTimeout,
	}
	for _, opt := range opts {
		opt(w)
	}

	go w.receiveLoop(runCtx)
	go w.reloadLoop(runCtx)
	return w, nil
}

// receiveLoop drains the redis pubsub channel and forwards non-self
// notifications to reloadLoop. Coalescing is delegated to the size-1
// reload channel: a burst of notifications while reloadLoop is busy
// produces at most one queued reload.
func (w *redisWatcher) receiveLoop(ctx context.Context) {
	defer close(w.recvDone)
	ch := w.pubsub.Channel(
		goredis.WithChannelSize(watcherChannelBufferSize),
		goredis.WithChannelSendTimeout(watcherChannelSendTimeout),
	)
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if msg.Payload == w.instanceID {
				continue
			}
			w.signalReload()
		}
	}
}

// signalReload posts a non-blocking reload notification. If the
// reload channel is full a reload is already pending; coalescing
// the extra signal is correct because LoadPolicy reads the latest
// DB state — running it twice would just waste a round-trip.
func (w *redisWatcher) signalReload() {
	select {
	case w.reload <- struct{}{}:
	default:
	}
}

// reloadLoop invokes the user callback on every reload signal.
// Decoupled from receiveLoop so callback latency cannot stall
// pubsub draining.
func (w *redisWatcher) reloadLoop(ctx context.Context) {
	defer close(w.reloadDone)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.reload:
			w.mu.RLock()
			cb := w.callback
			w.mu.RUnlock()
			if cb == nil {
				continue
			}
			cb("")
		}
	}
}

// SetUpdateCallback implements persist.Watcher.
//
// Race-safe against Close: the closed.Load runs INSIDE w.mu so a
// concurrent Close (which also acquires mu to clear the callback)
// either fully precedes or fully follows this update. Without the
// in-lock recheck, an orderng of (a) setter sees closed=false →
// (b) Close runs to completion → (c) setter takes mu and writes a
// non-nil callback that no goroutine will ever invoke would silently
// drop the caller's intent.
func (w *redisWatcher) SetUpdateCallback(fn func(string)) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed.Load() {
		return errors.New("authz/casbin watcher: SetUpdateCallback after Close")
	}
	w.callback = fn
	return nil
}

// Update implements persist.Watcher. Best-effort fire-and-forget:
// publish failures are logged but never returned, because Casbin's
// internal_api.go:394 returns Update's error verbatim from
// AddPolicy and the chok Service propagates it to the HTTP caller.
// A transient Redis blip would otherwise turn into a 500 even
// though the DB row was committed; on retry Casbin sees the
// duplicate, returns (false, nil), and never re-attempts notify —
// peer pods stay silently divergent. Swallowing here keeps the
// at-most-once contract documented in the package comment.
//
// publishTimeout caps the round-trip independently of
// RedisOptions.WriteTimeout because authz mutations sit on the hot
// request path; we'd rather drop a publish than stall a handler.
func (w *redisWatcher) Update() error {
	if w.closed.Load() {
		return errors.New("authz/casbin watcher: Update after Close")
	}
	ctx, cancel := context.WithTimeout(context.Background(), w.publishTimeout)
	defer cancel()
	if err := w.client.Publish(ctx, w.channel, w.instanceID).Err(); err != nil {
		w.publishFailures.Add(1)
		w.logger.Warn("authz/casbin watcher: publish failed (best-effort, peers may stay stale until next mutation)",
			"channel", w.channel,
			"error", err.Error(),
		)
		return nil
	}
	return nil
}

// Stats returns a snapshot of the watcher's best-effort counters
// (publish + reload failure counts). Safe to call concurrently with
// Update / Close.
func (w *redisWatcher) Stats() WatcherStats {
	return WatcherStats{
		PublishFailures: w.publishFailures.Load(),
		ReloadFailures:  w.reloadFailures.Load(),
	}
}

// recordReloadFailure increments the reload-failure counter. Called
// by the chok-owned LoadPolicy wrapper installed by withWatcher
// (enforcer.go) so the wrapper doesn't need direct atomic access.
func (w *redisWatcher) recordReloadFailure() {
	w.reloadFailures.Add(1)
}

// Close implements persist.Watcher. Idempotent; safe to call from
// multiple goroutines (the atomic CAS guards single-shot teardown).
//
// Order: cancel runtime ctx first so both goroutines' selects see
// Done() before any straggling work, then Close pubsub to unblock
// receiveLoop if it's parked on the receive case, then wait for
// receiveLoop exit (no more reload signals will be sent) THEN
// reloadLoop exit (any in-flight callback has returned), then
// clear the callback. After this returns *casbinAuthorizer can be
// GC'd safely.
//
// persist.Watcher.Close has a void signature, so any pubsub.Close
// error is swallowed — the goroutine drain is the actual shutdown
// semantics callers care about.
func (w *redisWatcher) Close() {
	if !w.closed.CompareAndSwap(false, true) {
		return
	}
	w.cancel()
	_ = w.pubsub.Close()
	<-w.recvDone
	<-w.reloadDone
	w.mu.Lock()
	w.callback = nil
	w.mu.Unlock()
}

// Compile-time interface assertion. Drift in persist.Watcher would
// otherwise only surface when SyncedEnforcer.SetWatcher type-checks
// at runtime.
var _ persist.Watcher = (*redisWatcher)(nil)
