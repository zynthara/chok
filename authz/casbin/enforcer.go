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

	"github.com/zynthara/chok/authz"
)

// auditHook wraps the per-policy-change callback so it can live behind
// an atomic.Pointer. We need pointer-shaped storage because Casbin's
// callback type is a func value (not directly storable as
// atomic.Pointer[func]). The wrapper lets Service-side reads use
// Load() while Close() does Store(nil) without ever holding a lock.
type auditHook struct {
	fn func(action, role, obj, act string)
}

// casbinAuthorizer wraps a Casbin SyncedEnforcer + adapter + optional
// Watcher into the chok-shaped Authorizer / DomainAuthorizer / Service
// trio. One process holds at most one of these (chok wires it through
// parts.AuthzComponent), and SyncedEnforcer's internal RWMutex makes
// concurrent Authorize() calls safe.
//
// The struct also satisfies io.Closer so AuthzComponent.Close can
// release the Watcher subscription on App.Stop.
//
// auditFn is read by every Service mutation method without taking any
// lock — atomic.Pointer.Load is the synchronisation primitive. mu is
// only used to serialise Close itself with a peer in-flight Close
// (defensive; AuthzComponent.Close is called once by registry.Stop).
type casbinAuthorizer struct {
	enforcer *casbinv3.SyncedEnforcer
	adapter  persist.Adapter
	watcher  persist.Watcher // nil when RedisWatcher disabled
	auditFn  atomic.Pointer[auditHook]
	mu       sync.Mutex // serialises Close
}

// newAuthorizer constructs the runtime *casbinAuthorizer from a
// pre-loaded model + adapter. Builder owns the wiring; this helper
// keeps the construction sequence factor-out-able for tests.
func newAuthorizer(modelText string, adapter persist.Adapter) (*casbinAuthorizer, error) {
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
	return &casbinAuthorizer{
		enforcer: enf,
		adapter:  adapter,
	}, nil
}

// withWatcher attaches the Casbin Watcher (Redis pub/sub or similar)
// so policy changes broadcast to peer instances. Builder calls this
// after enforcer construction; tests skip it for simplicity.
//
// Phase 6 stub: the Builder rejects RedisWatcher=true so this is
// currently unreachable. Kept as the wiring point for the watcher
// follow-up PR.
func (a *casbinAuthorizer) withWatcher(w persist.Watcher) error { //nolint:unused
	if w == nil {
		return nil
	}
	a.watcher = w
	return a.enforcer.SetWatcher(w)
}

// withAuditHook attaches a per-policy-change callback. Builder wires
// it when AuditEnabled is true. The hook fires after a successful
// policy mutation; failures inside the callback are logged but never
// undo the policy change (best-effort audit).
//
// Phase 6 stub: the Builder rejects AuditEnabled=true so this is
// currently unreachable. Kept as the wiring point for the audit
// follow-up PR. Storage uses atomic.Pointer so the eventual audit
// integration won't have to add synchronisation post hoc.
func (a *casbinAuthorizer) withAuditHook(fn func(action, role, obj, act string)) { //nolint:unused
	if fn == nil {
		a.auditFn.Store(nil)
		return
	}
	a.auditFn.Store(&auditHook{fn: fn})
}

// fireAudit invokes the audit hook if one is installed. Reads
// auditFn via atomic.Load so it stays race-free against Close
// without forcing every Service mutation to hold a.mu.
func (a *casbinAuthorizer) fireAudit(action, role, obj, act string) {
	if h := a.auditFn.Load(); h != nil && h.fn != nil {
		h.fn(action, role, obj, act)
	}
}

// Authorize implements authz.Authorizer. The single-tenant call
// funnels into the four-tuple matcher with domain="*" so global
// policies (p.dom="*") match transparently.
func (a *casbinAuthorizer) Authorize(_ context.Context, sub, obj, act string) (bool, error) {
	return a.enforcer.Enforce(sub, globalDomain, obj, act)
}

// AuthorizeInDomain implements authz.DomainAuthorizer. Empty domain
// is normalised to "*" so callers passing "" get the same behaviour
// as Authorize.
func (a *casbinAuthorizer) AuthorizeInDomain(_ context.Context, sub, dom, obj, act string) (bool, error) {
	return a.enforcer.Enforce(sub, normalizeDomain(dom), obj, act)
}

// grantRoleBatch is the package-internal fast path Bootstrap takes
// when the Service is the chok-shipped *casbinAuthorizer. It walks
// AddPolicies once instead of N AddPolicy round-trips.
//
// Casbin's enforcer.AddPolicies routes through persist.BatchAdapter
// when the adapter implements it (chok's gormAdapter does), so 100
// permissions become a single INSERT.
func (a *casbinAuthorizer) grantRoleBatch(_ context.Context, role string, perms []Permission) error {
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
		a.fireAudit("GrantRole", role, p.Object, p.Action)
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
func (a *casbinAuthorizer) Close() error {
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
// any drift in the *casbinAuthorizer method set surfaces at build
// time rather than at runtime panic.
var (
	_ authz.Authorizer       = (*casbinAuthorizer)(nil)
	_ authz.DomainAuthorizer = (*casbinAuthorizer)(nil)
	_ Service                = (*casbinAuthorizer)(nil)
	_ io.Closer              = (*casbinAuthorizer)(nil)
	_ batchGranter           = (*casbinAuthorizer)(nil)
)
