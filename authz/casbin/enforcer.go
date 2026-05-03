package casbin

import (
	"context"
	"fmt"
	"io"
	"sync"

	casbinv3 "github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/persist"

	"github.com/zynthara/chok/authz"
)

// casbinAuthorizer wraps a Casbin SyncedEnforcer + adapter + optional
// Watcher into the chok-shaped Authorizer / DomainAuthorizer / Service
// trio. One process holds at most one of these (chok wires it through
// parts.AuthzComponent), and SyncedEnforcer's internal RWMutex makes
// concurrent Authorize() calls safe.
//
// The struct also satisfies io.Closer so AuthzComponent.Close can
// release the Watcher subscription on App.Stop.
type casbinAuthorizer struct {
	enforcer *casbinv3.SyncedEnforcer
	adapter  persist.Adapter
	watcher  persist.Watcher // nil when RedisWatcher disabled
	auditFn  func(action, role, obj, act string)
	mu       sync.Mutex // guards Close-ordering against concurrent policy edits
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
func (a *casbinAuthorizer) withWatcher(w persist.Watcher) error {
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
func (a *casbinAuthorizer) withAuditHook(fn func(action, role, obj, act string)) {
	a.auditFn = fn
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

// Close releases the Watcher subscriber + clears the audit hook so
// shutdown doesn't race against in-flight policy callbacks.
//
// Casbin's persist.Watcher.Close() returns void (the upstream
// interface predates error-returning Close conventions); we still
// run it, then nil out our reference to break the cycle.
func (a *casbinAuthorizer) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.watcher != nil {
		a.watcher.Close()
		a.watcher = nil
	}
	a.auditFn = nil
	// SyncedEnforcer doesn't have a Close; gorm-adapter's connection
	// is owned by chok's DBComponent. Nothing else to release.
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
)
