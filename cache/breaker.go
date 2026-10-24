package cache

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type breakerState int

const (
	closed   breakerState = iota // calls go through
	open                         // calls are misses and skipped writes
	halfOpen                     // one probe at a time goes through
)

// breaker guards the redis layer. After threshold failures in a row it
// opens: reads are misses and writes are skipped, without touching Redis,
// for reset. Then it is half-open: one call at a time goes through as a
// probe, with its own timeout, and successes probes in a row close it;
// a failed probe opens it again. Errors never reach the caller: a Redis
// in trouble degrades to cache misses. A call that fails because its
// caller's context ended says nothing about Redis and is not counted.
type breaker struct {
	inner        layer
	threshold    int
	reset        time.Duration
	successes    int
	probeTimeout time.Duration
	logger       *slog.Logger

	mu       sync.Mutex
	state    breakerState
	failures int       // in a row, while closed
	streak   int       // successful probes in a row, while half-open
	openedAt time.Time // when it last opened
	probing  bool      // a probe is in flight
	probeAt  time.Time // when it started
}

func newBreaker(inner layer, threshold int, reset time.Duration, successes int, probeTimeout time.Duration, logger *slog.Logger) *breaker {
	return &breaker{
		inner: inner, threshold: threshold, reset: reset, successes: successes,
		probeTimeout: min(probeTimeout, reset), logger: logger,
	}
}

func (b *breaker) get(ctx context.Context, key string) ([]byte, bool, error) {
	probe, ok := b.allow()
	if !ok {
		return nil, false, nil
	}
	cctx, cancel := b.callContext(ctx, probe)
	defer cancel()
	v, hit, err := b.inner.get(cctx, key)
	b.record(ctx, err, probe)
	if err != nil {
		return nil, false, nil
	}
	return v, hit, nil
}

func (b *breaker) set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	probe, ok := b.allow()
	if !ok {
		return nil
	}
	cctx, cancel := b.callContext(ctx, probe)
	defer cancel()
	b.record(ctx, b.inner.set(cctx, key, val, ttl), probe)
	return nil
}

func (b *breaker) del(ctx context.Context, key string) error {
	probe, ok := b.allow()
	if !ok {
		return nil
	}
	cctx, cancel := b.callContext(ctx, probe)
	defer cancel()
	b.record(ctx, b.inner.del(cctx, key), probe)
	return nil
}

// callContext is ctx for a call made while closed. A probe gets its own
// timeout instead of the caller's deadline, which may be nearly spent and
// would fail the probe for no fault of Redis; it keeps ctx's values.
func (b *breaker) callContext(ctx context.Context, probe bool) (context.Context, context.CancelFunc) {
	if !probe {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), b.probeTimeout)
}

// allow reports whether a call may go to Redis, and whether it is the
// probe.
func (b *breaker) allow() (probe, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	switch b.state {
	case closed:
		return false, true
	case open:
		if now.Sub(b.openedAt) < b.reset {
			return false, false
		}
		b.state, b.streak, b.probing = halfOpen, 0, false
	}
	// half-open
	if b.probing {
		if now.Sub(b.probeAt) < b.reset {
			return false, false
		}
		// The probe in flight has hung for a whole reset period: count it
		// failed and start over.
		b.reopen(now)
		return false, false
	}
	b.probing, b.probeAt = true, now
	return true, true
}

// record counts a call's outcome: any call's while closed, only the
// probe's while half-open, none while open (calls that started before it
// opened).
func (b *breaker) record(ctx context.Context, err error, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probing = false
	}
	if err != nil && !probe && ctx.Err() != nil {
		return // the caller gave up; Redis is not to blame
	}
	switch {
	case b.state == closed && err == nil:
		b.failures = 0
	case b.state == closed:
		b.failures++
		if b.failures >= b.threshold {
			b.state, b.openedAt = open, time.Now()
			b.logger.WarnContext(ctx, "cache: redis breaker opened: serving misses",
				"failures", b.failures, "reset_timeout", b.reset, "error", err)
		}
	case b.state == halfOpen && probe && err == nil:
		b.streak++
		if b.streak >= b.successes {
			b.state, b.failures, b.streak = closed, 0, 0
			b.logger.InfoContext(ctx, "cache: redis breaker closed: redis answers again")
		}
	case b.state == halfOpen && probe:
		b.reopen(time.Now())
		b.logger.DebugContext(ctx, "cache: redis breaker probe failed; open again", "error", err)
	}
}

func (b *breaker) reopen(now time.Time) {
	b.state, b.openedAt, b.streak, b.probing = open, now, 0, false
}
