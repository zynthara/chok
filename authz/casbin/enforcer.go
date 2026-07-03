package casbin

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	casbinv3 "github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/persist"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/log"
)

// auditHook wraps the per-policy-change callback so it can live behind
// an atomic.Pointer. We need pointer-shaped storage because Casbin's
// callback type is a func value (not directly storable as
// atomic.Pointer[func]). The wrapper lets Service-side reads use
// Load() while Close() does Store(nil) without ever holding a lock.
// The ctx parameter carries the mutating call's request context so the
// audit sink can attribute actor / trace (M4 7.E wiring).
type auditHook struct {
	fn func(ctx context.Context, action, role, obj, act string)
}

// Engine wraps a Casbin SyncedEnforcer + adapter + optional
// Watcher into the chok-shaped Authorizer / DomainAuthorizer / Service
// trio. One process holds at most one of these (the authz module wires
// it), and SyncedEnforcer's internal RWMutex makes concurrent
// Authorize() calls safe.
//
// The struct also satisfies io.Closer so the authz module's Close can
// release the Watcher subscription on App.Stop.
//
// This package deliberately does not import chok/v2/authz: the
// interface satisfaction is structural, and the compile-time
// assertions live in the authz package (which imports this engine
// room) — that one-way street is what lets authz.Module() exist.
//
// auditFn is read by every Service mutation method without taking any
// lock — atomic.Pointer.Load is the synchronisation primitive. mu is
// only used to serialise Close itself with a peer in-flight Close
// (defensive; the module's Close is called once by the kernel).
type Engine struct {
	enforcer *casbinv3.SyncedEnforcer
	adapter  persist.Adapter
	watcher  persist.Watcher // nil when RedisWatcher disabled
	auditFn  atomic.Pointer[auditHook]
	logger   log.Logger // never nil; defaults to log.Empty()
	mu       sync.Mutex // serialises Close
}

// NewEngine constructs the runtime engine against gdb: adapter (no
// DDL — casbin_rule creation belongs to the authz module's Migrate,
// SPEC §5.3) + enforcer + eager LoadPolicy. The caller guarantees the
// casbin_rule table exists; a missing table surfaces here as a
// LoadPolicy error, which is exactly the fail-closed startup failure
// the migrate-off contract wants.
func NewEngine(modelText string, gdb *gorm.DB, logger log.Logger) (*Engine, error) {
	adapter, err := newGormAdapter(gdb)
	if err != nil {
		return nil, err
	}
	return newAuthorizer(modelText, adapter, logger)
}

// newAuthorizer constructs the runtime *Engine from a
// pre-loaded model + adapter. NewEngine owns the blessed wiring; this
// helper keeps the construction sequence factor-out-able for tests.
//
// logger is used for peer-triggered LoadPolicy failures (see
// withWatcher) and any future best-effort paths. nil collapses to
// log.Empty() so test callers don't have to thread a logger.
func newAuthorizer(modelText string, adapter persist.Adapter, logger log.Logger) (*Engine, error) {
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		return nil, fmt.Errorf("casbin: parse model: %w", err)
	}
	enf, err := casbinv3.NewSyncedEnforcer(m, adapter)
	if err != nil {
		return nil, fmt.Errorf("casbin: new enforcer: %w", err)
	}
	// Eagerly load policies so the first Authorize() call doesn't
	// block on adapter I/O. NewSyncedEnforcer already loads, but
	// LoadPolicy is idempotent and surfaces post-construction
	// reload bugs early.
	if err := enf.LoadPolicy(); err != nil {
		return nil, fmt.Errorf("casbin: load policy: %w", err)
	}
	if logger == nil {
		logger = log.Empty()
	}
	return &Engine{
		enforcer: enf,
		adapter:  adapter,
		logger:   logger,
	}, nil
}

// withWatcher attaches the Casbin Watcher (Redis pub/sub or similar)
// so policy changes broadcast to peer instances. Builder calls this
// after enforcer construction; tests that don't exercise multi-pod
// sync skip it.
//
// SetWatcher overwrites any prior watcher inside the enforcer; we
// stash a copy on the receiver so Close can release the subscriber
// goroutine before the enforcer becomes unreachable. nil w is a
// no-op so tests can swap watchers off without special-casing.
//
// Casbin v3 SetWatcher injects a default callback (enforcer.go:252)
// of the form `func(string) { _ = e.LoadPolicy() }` which silently
// discards reload errors. We immediately overwrite it with a chok-
// owned wrapper so a peer-triggered LoadPolicy failure (DB blip,
// adapter parse error) shows up in the operator's log instead of
// vanishing. Without this, a watcher fan-out where every peer
// LoadPolicy fails would look identical to a healthy system from
// the publish side.
func (a *Engine) withWatcher(w persist.Watcher) error {
	if w == nil {
		return nil
	}
	a.watcher = w
	if err := a.enforcer.SetWatcher(w); err != nil {
		return err
	}
	// Best-effort: if the concrete watcher exposes a reload-failure
	// counter (chok's *redisWatcher does), bump it inside the
	// wrapper so a third-party Service-level dashboard can observe
	// the same number the watcher reports via Stats().
	type reloadFailureRecorder interface{ recordReloadFailure() }
	rec, _ := w.(reloadFailureRecorder)
	return w.SetUpdateCallback(func(payload string) {
		if err := a.enforcer.LoadPolicy(); err != nil {
			if rec != nil {
				rec.recordReloadFailure()
			}
			a.logger.Error("authz/casbin: peer-triggered LoadPolicy failed",
				"payload", payload,
				"error", err.Error(),
			)
		}
	})
}

// WatcherStats returns a snapshot of the underlying watcher's
// best-effort counters, or zero-value when no watcher is attached
// (RedisWatcher disabled). Service callers can use this to surface
// pub/sub health on /healthz or scrape into Prometheus without
// reaching into the casbin package internals.
func (a *Engine) WatcherStats() WatcherStats {
	rw, ok := a.watcher.(*redisWatcher)
	if !ok || rw == nil {
		return WatcherStats{}
	}
	return rw.Stats()
}

// AttachAuditHook attaches a per-policy-change callback. The authz
// module wires it when audit_enabled is true — before bootstrap
// seeding, so even the seed grants are audited (7.E). The hook fires
// after a successful policy mutation; failures inside the callback
// are the sink's business and never undo the policy change
// (best-effort audit, the v1-documented trade-off). nil clears the
// hook. Storage uses atomic.Pointer so mutations never take a lock.
func (a *Engine) AttachAuditHook(fn func(ctx context.Context, action, role, obj, act string)) {
	if fn == nil {
		a.auditFn.Store(nil)
		return
	}
	a.auditFn.Store(&auditHook{fn: fn})
}

// AttachRedisWatcher attaches the chok-shipped Casbin Watcher backed
// by the given shared client, so policy edits broadcast to peer
// instances (multi-pod sync). The channel names the pub/sub topic.
// On attach failure the freshly-subscribed watcher is released so
// its goroutines don't outlive a failed startup.
func (a *Engine) AttachRedisWatcher(ctx context.Context, client *goredis.Client, channel string) error {
	w, err := newRedisWatcher(ctx, client, channel, withWatcherLogger(a.logger))
	if err != nil {
		return fmt.Errorf("casbin: redis watcher: %w", err)
	}
	if err := a.withWatcher(w); err != nil {
		w.Close()
		return fmt.Errorf("casbin: redis watcher attach: %w", err)
	}
	return nil
}

// fireAudit invokes the audit hook if one is installed. Reads
// auditFn via atomic.Load so it stays race-free against Close
// without forcing every Service mutation to hold a.mu.
func (a *Engine) fireAudit(ctx context.Context, action, role, obj, act string) {
	if h := a.auditFn.Load(); h != nil && h.fn != nil {
		h.fn(ctx, action, role, obj, act)
	}
}

// Authorize implements authz.Authorizer. The single-tenant call
// funnels into the four-tuple matcher with domain="*" so global
// policies (p.dom="*") match transparently.
func (a *Engine) Authorize(_ context.Context, sub, obj, act string) (bool, error) {
	return a.enforcer.Enforce(sub, globalDomain, obj, act)
}

// AuthorizeInDomain implements authz.DomainAuthorizer. Empty domain
// is normalised to "*" so callers passing "" get the same behaviour
// as Authorize.
func (a *Engine) AuthorizeInDomain(_ context.Context, sub, dom, obj, act string) (bool, error) {
	return a.enforcer.Enforce(sub, normalizeDomain(dom), obj, act)
}

// grantRoleBatch is the package-internal fast path Bootstrap takes
// when the Service is the chok-shipped *Engine. It walks
// AddPolicies once instead of N AddPolicy round-trips.
//
// Casbin's enforcer.AddPolicies routes through persist.BatchAdapter
// when the adapter implements it (chok's gormAdapter does), so 100
// permissions become a single INSERT.
func (a *Engine) grantRoleBatch(ctx context.Context, role string, perms []Permission) error {
	if len(perms) == 0 {
		return nil
	}
	rules := make([][]string, 0, len(perms))
	for _, p := range perms {
		rules = append(rules, []string{role, globalDomain, p.Object, p.Action})
	}
	if _, err := a.enforcer.AddPolicies(rules); err != nil {
		return fmt.Errorf("authz/casbin grantRoleBatch: %w", err)
	}
	for _, p := range perms {
		a.fireAudit(ctx, "GrantRole", role, p.Object, p.Action)
	}
	return nil
}

// Close releases the Watcher subscriber + clears the audit hook so
// shutdown doesn't race against in-flight policy callbacks.
//
// Casbin's persist.Watcher.Close() returns void (the upstream
// interface predates error-returning Close conventions); we still
// run it, then nil out our reference to break the cycle. auditFn
// is cleared via atomic.Store so concurrent fireAudit readers
// observe nil on their next Load.
func (a *Engine) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.watcher != nil {
		a.watcher.Close()
		a.watcher = nil
	}
	a.auditFn.Store(nil)
	// SyncedEnforcer doesn't have a Close; the *gorm.DB connection is
	// owned by chok's DBComponent. Nothing else to release.
	return nil
}

// Compile-time interface assertions. SPEC §3 v0.3.3 / v0.3.4:
// any drift in the *Engine method set surfaces at build
// time rather than at runtime panic. The authz.Authorizer /
// authz.DomainAuthorizer assertions live in the authz package —
// this package must not import authz (one-way street, see Engine).
var (
	_ Service      = (*Engine)(nil)
	_ io.Closer    = (*Engine)(nil)
	_ batchGranter = (*Engine)(nil)
)
