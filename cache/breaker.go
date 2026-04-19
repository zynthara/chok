package cache

import (
	"context"
	"sync"
	"time"
)

// BreakerOptions configures the circuit breaker wrapping a Cache backend.
type BreakerOptions struct {
	// Threshold is the number of consecutive failures before the circuit
	// opens. Default: 5.
	Threshold int

	// ResetTimeout is how long the circuit stays open before switching to
	// half-open and allowing a single probe request. Default: 30s.
	ResetTimeout time.Duration

	// HalfOpenSuccesses is the number of consecutive successful probes
	// required to close the breaker from half-open. A flapping backend
	// can otherwise oscillate (a single lucky probe closes the circuit,
	// the next real request fails and re-opens it). Default: 3, chosen
	// to dampen single-probe coincidences without delaying recovery
	// when the backend is genuinely healthy.
	HalfOpenSuccesses int

	// ProbeTimeout bounds the half-open probe independently of the
	// triggering request's context. Without this decoupling, a probe
	// inherits the request's possibly-exhausted deadline and fails with
	// context.DeadlineExceeded, which record() would treat as a backend
	// failure and re-open the breaker — keeping the circuit open even
	// though the backend may be healthy. Default: 2s, capped at
	// ResetTimeout.
	ProbeTimeout time.Duration
}

func (o *BreakerOptions) withDefaults() BreakerOptions {
	out := *o
	if out.Threshold <= 0 {
		out.Threshold = 5
	}
	if out.ResetTimeout <= 0 {
		out.ResetTimeout = 30 * time.Second
	}
	if out.HalfOpenSuccesses <= 0 {
		out.HalfOpenSuccesses = 3
	}
	if out.ProbeTimeout <= 0 {
		out.ProbeTimeout = 2 * time.Second
	}
	if out.ProbeTimeout > out.ResetTimeout {
		out.ProbeTimeout = out.ResetTimeout
	}
	return out
}

type breakerState int

const (
	breakerClosed   breakerState = iota // normal operation
	breakerOpen                         // failing fast, no calls to backend
	breakerHalfOpen                     // allowing one probe call
)

// breakerCache wraps a Cache with a circuit breaker. When the backend fails
// consecutively ≥ threshold times, the breaker opens:
//   - Get returns (nil, false, nil) — cache miss, no error
//   - Set / Delete silently skip — no error
//
// After resetTimeout the breaker enters half-open and allows a single probe.
// Success closes the breaker; failure reopens it.
type breakerCache struct {
	inner Cache
	opts  BreakerOptions

	mu             sync.Mutex
	state          breakerState
	failures       int
	successes      int // consecutive successes in half-open; resets on failure
	lastFailed     time.Time
	lastProbeStart time.Time // tracks when the half-open probe began
}

// WithBreaker wraps a Cache backend with a circuit breaker. Suitable for
// network-dependent backends (Redis) where sustained failures should not
// cascade latency to every request.
//
// When the circuit is open, the wrapped cache behaves as if every key is a
// miss — this lets a Chain cache fall through to lower tiers transparently.
func WithBreaker(c Cache, opts BreakerOptions) Cache {
	opts = opts.withDefaults()
	return &breakerCache{inner: c, opts: opts}
}

func (b *breakerCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	ok, isProbe := b.allow()
	if !ok {
		return nil, false, nil // open → miss
	}
	innerCtx, cancel := b.probeContext(ctx, isProbe)
	defer cancel()
	data, okInner, err := b.inner.Get(innerCtx, key)
	b.record(err)
	if err != nil {
		return nil, false, nil // degrade to miss, don't propagate
	}
	return data, okInner, nil
}

func (b *breakerCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	ok, isProbe := b.allow()
	if !ok {
		return nil // open → silent skip
	}
	innerCtx, cancel := b.probeContext(ctx, isProbe)
	defer cancel()
	err := b.inner.Set(innerCtx, key, value, ttl)
	b.record(err)
	return nil // fire-and-forget: don't propagate backend errors
}

func (b *breakerCache) Delete(ctx context.Context, key string) error {
	ok, isProbe := b.allow()
	if !ok {
		return nil // open → silent skip
	}
	innerCtx, cancel := b.probeContext(ctx, isProbe)
	defer cancel()
	err := b.inner.Delete(innerCtx, key)
	b.record(err)
	return nil // fire-and-forget
}

// probeContext returns ctx unchanged on a normal (closed-state) request.
// For the single half-open probe, it returns a detached context so the
// probe's health verdict isn't poisoned by a caller-level deadline that
// has nothing to do with the backend. Context values (trace-ids,
// request-ids) are preserved via WithoutCancel so log correlation still
// works; only the cancellation signal is severed.
func (b *breakerCache) probeContext(ctx context.Context, isProbe bool) (context.Context, context.CancelFunc) {
	if !isProbe {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), b.opts.ProbeTimeout)
}

func (b *breakerCache) Close() error {
	return b.inner.Close()
}

// allow reports whether the request should be forwarded to the inner
// cache, and whether this particular call is the half-open probe. Only
// one probe is authorised per reset cycle; all other calls while the
// breaker is open or half-open are rejected.
func (b *breakerCache) allow() (ok bool, isProbe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case breakerClosed:
		return true, false
	case breakerOpen:
		if time.Since(b.lastFailed) >= b.opts.ResetTimeout {
			b.state = breakerHalfOpen
			b.lastProbeStart = time.Now()
			return true, true
		}
		return false, false
	case breakerHalfOpen:
		// Only one probe at a time — additional requests are rejected
		// until the probe completes. This prevents a stampede of
		// requests hitting a still-recovering backend.
		// If the probe has been in-flight too long, give up and re-open
		// so the reset timer can restart.
		if time.Since(b.lastProbeStart) >= b.opts.ResetTimeout {
			b.state = breakerOpen
			b.lastFailed = time.Now()
		}
		return false, false
	}
	return true, false
}

// record tracks the outcome of a backend call.
func (b *breakerCache) record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err == nil {
		// Success. From closed we stay closed (and clear failure count).
		// From half-open we need HalfOpenSuccesses consecutive wins to
		// fully close — a flapping backend that produces one lucky probe
		// followed by failures would otherwise toggle on every probe.
		b.failures = 0
		switch b.state {
		case breakerHalfOpen:
			b.successes++
			if b.successes >= b.opts.HalfOpenSuccesses {
				b.state = breakerClosed
				b.successes = 0
			}
		default:
			b.state = breakerClosed
			b.successes = 0
		}
		return
	}

	// Failure: reset any half-open success streak and resume counting.
	b.successes = 0
	b.failures++
	b.lastFailed = time.Now()
	if b.failures >= b.opts.Threshold {
		b.state = breakerOpen
	}
}
