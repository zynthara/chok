package identity

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zynthara/chok"
)

// maxLimiterEntries caps the in-memory size of the rate-limit table.
// Once reached, the limiter evicts the entry with the oldest last-seen
// attempt. This bounds memory under credential-stuffing attacks that
// probe millions of unique emails; the cleanup loop alone is O(N) and
// won't keep up with a sustained attack.
const maxLimiterEntries = 100_000

// cleanupWatermark triggers cleanup whenever the table grows past this
// fraction of the cap. Picked so cleanup runs before we hit the hard
// cap and start evicting live entries.
const cleanupWatermark = maxLimiterEntries * 3 / 4

// loginLimiter provides per-subject rate limiting for login attempts.
// Uses a simple sliding-window counter with automatic cleanup of stale
// entries and a hard size cap. Thread-safe.
//
// A client address's key is held to a limit of its own, ipThreshold:
// everyone behind one NAT or proxy shares the address, so it is wider by
// default than the threshold of an email or a user, and 0 counts no
// address at all.
//
// When the threshold is exceeded, the caller should return 429 Too Many
// Requests. The limiter does NOT perform exponential backoff or account
// lockout — those are orthogonal concerns that depend on the deployment
// context (WAF, CDN, etc.).
type loginLimiter struct {
	mu          sync.Mutex
	window      time.Duration
	threshold   int // the limit of every key but a client address's
	ipThreshold int // the limit of a client address's key; 0 counts none
	entries     map[string]*limiterEntry
	calls       int            // counter for probabilistic cleanup
	cleaning    atomic.Int32   // atomic flag: 1 when a background cleanup goroutine is running
	wg          sync.WaitGroup // counts in-flight cleanup goroutines for Close to await
	closed      bool           // set by Close; suppresses future cleanup launches
}

type limiterEntry struct {
	attempts []time.Time
	// lastSeen is the timestamp of the most recent attempt, used by
	// evictOldestLocked to pick a victim when the table is full.
	lastSeen time.Time
}

// newLoginLimiter holds a client address's keys to ipLimit and every
// other key to limit, within window.
func newLoginLimiter(window time.Duration, limit, ipLimit int) *loginLimiter {
	return &loginLimiter{
		window:      window,
		threshold:   limit,
		ipThreshold: ipLimit,
		entries:     make(map[string]*limiterEntry),
	}
}

// limiterKey pairs a human-readable name with the bucket value so the
// limiter can report which dimension triggered a 429 without leaking
// the raw value (email / IP) into logs. ip marks the key of a client
// address, which ipKey makes.
type limiterKey struct {
	Name  string
	Value string
	ip    bool
}

// ipKey is a client address's key in the dimension name: it is held to
// login_rate_limit_per_ip, not login_rate_limit.
func ipKey(name, value string) limiterKey { return limiterKey{Name: name, Value: value, ip: true} }

// bucket is the table key: the dimension and the value, so an email and
// an address never share a bucket.
func (k limiterKey) bucket() string { return k.Name + "\x00" + k.Value }

// counted reports whether the limiter keeps key at all: a key without a
// value never counts, nor does a client address while ipThreshold is 0.
func (l *loginLimiter) counted(key limiterKey) bool {
	return key.Value != "" && (!key.ip || l.ipThreshold > 0)
}

// check returns true if every supplied key is within its limit:
// ipThreshold for a client address, threshold for any other key. A key
// the limiter does not count is ignored — an empty value, so callers can
// pass an optional IP unconditionally, or an address while ipThreshold
// is 0 — and a call of such keys alone returns true (no-op) so missing
// metadata never over-limits.
//
// When the limit is exceeded, the second return value is the Name of
// the dimension that triggered the rejection (e.g. "email" or
// "client_ip"). Callers can log this to make 429 spikes attributable
// to credential-stuffing vs. IP-scanning patterns without recording
// the PII itself.
func (l *loginLimiter) check(keys ...limiterKey) (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.window)
	for _, key := range keys {
		if !l.counted(key) {
			continue
		}
		e, ok := l.entries[key.bucket()]
		if !ok {
			continue
		}
		count := 0
		for _, t := range e.attempts {
			if t.After(cutoff) {
				count++
			}
		}
		limit := l.threshold
		if key.ip {
			limit = l.ipThreshold
		}
		if count >= limit {
			return false, key.Name
		}
	}
	return true, ""
}

// record adds an attempt for every supplied key the limiter counts; one
// it does not, such as an address while ipThreshold is 0, gets no entry.
// Login calls it only on an authentication failure, so successful logins
// don't consume the budget; throttle calls it on every call of the routes
// that count each one. Keying on both email and IP means an attacker
// rotating emails still exhausts the IP-keyed bucket (and vice versa).
//
// Cleanup is offloaded to a background goroutine when the table grows
// past the watermark so the login critical path stays fast under
// credential-stuffing. The atomic `cleaning` flag prevents multiple
// concurrent cleanups from piling up while the table is hot.
func (l *loginLimiter) record(keys ...limiterKey) {
	l.mu.Lock()
	now := time.Now()
	for _, key := range keys {
		if !l.counted(key) {
			continue
		}
		l.recordLocked(key.bucket(), now)
	}
	l.calls++
	needsCleanup := len(l.entries) >= cleanupWatermark || l.calls%100 == 0
	closed := l.closed
	l.mu.Unlock()

	if closed || !needsCleanup {
		return
	}
	if l.cleaning.CompareAndSwap(0, 1) {
		// Add to the WaitGroup before launching so a racing Close()
		// observes the in-flight goroutine and waits for it. Adding
		// after `go` would let Close()'s Wait return before the
		// goroutine started executing.
		l.wg.Add(1)
		go l.runCleanup()
	}
}

// runCleanup runs cleanupLocked in the background. The atomic flag
// ensures only one cleanup goroutine exists at a time; subsequent
// triggers are dropped (the next record() will retry once this one
// finishes). Sequential cleanups are sufficient because each one
// removes every expired entry visible at that moment.
//
// The wg counter is incremented by record() before launch so Close()
// can wait for any in-flight cleanup before returning — without that
// barrier a graceful shutdown could leave the goroutine half-way
// through map iteration when the process exits.
func (l *loginLimiter) runCleanup() {
	defer l.wg.Done()
	defer l.cleaning.Store(0)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanupLocked(time.Now())
}

// Close blocks until any in-flight background cleanup goroutine
// finishes. Safe to call multiple times. After Close returns, future
// record() calls still work but will not spawn new cleanups (the
// closed flag short-circuits the launch). The limiter remains usable
// for in-memory checks if a Module is reused after Close, but
// production code should treat it as terminal.
func (l *loginLimiter) Close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.wg.Wait()
}

// recordLocked bumps a single key's attempt list. Caller must hold l.mu.
func (l *loginLimiter) recordLocked(key string, now time.Time) {
	e, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= maxLimiterEntries {
			l.evictOldestLocked()
		}
		e = &limiterEntry{}
		l.entries[key] = e
	}
	cutoff := now.Add(-l.window)
	valid := e.attempts[:0]
	for _, t := range e.attempts {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	e.attempts = append(valid, now)
	e.lastSeen = now
}

// cleanupLocked removes entries that have no recent attempts. Must be
// called with l.mu held. Invoked on watermark or every N calls to
// prevent unbounded memory growth without requiring a background timer.
func (l *loginLimiter) cleanupLocked(now time.Time) {
	cutoff := now.Add(-l.window)
	for key, e := range l.entries {
		allExpired := true
		for _, t := range e.attempts {
			if t.After(cutoff) {
				allExpired = false
				break
			}
		}
		if allExpired {
			delete(l.entries, key)
		}
	}
}

// evictOldestLocked removes the entry with the oldest lastSeen. Must be
// called with l.mu held. Called when the table hits maxLimiterEntries
// to keep memory bounded even when every insert is a fresh subject
// (credential-stuffing signature).
func (l *loginLimiter) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for key, e := range l.entries {
		if first || e.lastSeen.Before(oldestTime) {
			oldestKey = key
			oldestTime = e.lastSeen
			first = false
		}
	}
	if !first {
		delete(l.entries, oldestKey)
	}
}

// throttle applies the login throttle to a route that counts every call,
// not only the failures: the call counts against each key — its Name
// picks the bucket, so a route with names of its own shares none — and is
// refused once a key has reached its limit within login_rate_window:
// login_rate_limit_per_ip for a client address's key, login_rate_limit
// for any other. The refusal is a 429 saying too many what were asked
// for, with retry_after the whole window; it counts against no key, and
// its warning names the dimension that was full, never the value. With
// the throttle off every call passes.
func (idn *Identity) throttle(ctx context.Context, what string, keys ...limiterKey) error {
	if idn.limiter == nil {
		return nil
	}
	if ok, by := idn.limiter.check(keys...); !ok {
		chok.LoggerFrom(ctx).WarnContext(ctx, "identity: "+what+" throttled", "triggered_by", by)
		return chok.ErrRateLimited.Msgf("too many %s asked for; try again later", what).With("retry_after", max(int(idn.cfg.LoginRateWindow/time.Second), 1))
	}
	idn.limiter.record(keys...)
	return nil
}
