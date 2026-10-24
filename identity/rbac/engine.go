package rbac

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	casbinv3 "github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/persist"
	goredis "github.com/redis/go-redis/v9"
)

// auditHook wraps the per-mutation callback so it can live behind an
// atomic.Pointer: mutations read it without a lock while close clears it.
// ctx is the mutating call's, so the audit log can name the actor.
type auditHook struct {
	fn func(ctx context.Context, action, role, obj, act string)
}

// engine is a Casbin SyncedEnforcer on this package's gorm adapter, with
// an optional Redis watcher and an optional audit hook. SyncedEnforcer's
// RWMutex makes concurrent decisions safe.
type engine struct {
	enforcer *casbinv3.SyncedEnforcer
	adapter  persist.Adapter
	watcher  persist.Watcher // nil without the Watcher option
	audit    atomic.Pointer[auditHook]
	logger   *slog.Logger
	mu       sync.Mutex // serialises close
}

// newEngine loads every policy eagerly, so the first decision does not
// wait on the database and a missing table fails here.
func newEngine(modelText string, adapter persist.Adapter, logger *slog.Logger) (*engine, error) {
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		return nil, fmt.Errorf("rbac: parse the model: %w", err)
	}
	// NewSyncedEnforcer loads the policies already: a missing table fails
	// here.
	enf, err := casbinv3.NewSyncedEnforcer(m, adapter)
	if err != nil {
		return nil, fmt.Errorf("rbac: load the policies: %w", err)
	}
	if err := enf.LoadPolicy(); err != nil {
		return nil, fmt.Errorf("rbac: load the policies: %w", err)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &engine{enforcer: enf, adapter: adapter, logger: logger}, nil
}

// withWatcher attaches w so policy changes reach the other replicas.
// Casbin's SetWatcher installs a callback that drops reload errors; it is
// replaced at once by one that logs them and counts them on the watcher.
func (e *engine) withWatcher(w persist.Watcher) error {
	if w == nil {
		return nil
	}
	e.watcher = w
	if err := e.enforcer.SetWatcher(w); err != nil {
		return err
	}
	type reloadFailureRecorder interface{ recordReloadFailure() }
	rec, _ := w.(reloadFailureRecorder)
	return w.SetUpdateCallback(func(payload string) {
		if err := e.enforcer.LoadPolicy(); err != nil {
			if rec != nil {
				rec.recordReloadFailure()
			}
			e.logger.Error("rbac: reloading the policies a peer changed failed", "payload", payload, "error", err)
		}
	})
}

// watcherStats is the watcher's counters, zero without one.
func (e *engine) watcherStats() watcherStats {
	rw, ok := e.watcher.(*redisWatcher)
	if !ok || rw == nil {
		return watcherStats{}
	}
	return rw.Stats()
}

// attachRedisWatcher subscribes the Redis watcher on channel; on a failed
// attach the subscription is released.
func (e *engine) attachRedisWatcher(ctx context.Context, client *goredis.Client, channel string) error {
	w, err := newRedisWatcher(ctx, client, channel, withWatcherLogger(e.logger))
	if err != nil {
		return fmt.Errorf("rbac: redis watcher: %w", err)
	}
	if err := e.withWatcher(w); err != nil {
		w.Close()
		return fmt.Errorf("rbac: attach the redis watcher: %w", err)
	}
	return nil
}

// attachAudit installs the mutation callback; nil clears it. It fires
// after a mutation that changed something; a failure inside it never
// undoes the change.
func (e *engine) attachAudit(fn func(ctx context.Context, action, role, obj, act string)) {
	if fn == nil {
		e.audit.Store(nil)
		return
	}
	e.audit.Store(&auditHook{fn: fn})
}

func (e *engine) fireAudit(ctx context.Context, action, role, obj, act string) {
	if h := e.audit.Load(); h != nil && h.fn != nil {
		h.fn(ctx, action, role, obj, act)
	}
}

// authorize decides globally: domain "*" is the global scope, which
// global policies (p.dom = "*") match.
func (e *engine) authorize(sub, obj, act string) (bool, error) {
	return e.enforcer.Enforce(sub, globalDomain, obj, act)
}

// authorizeInDomain decides in a tenant; "" means global.
func (e *engine) authorizeInDomain(sub, dom, obj, act string) (bool, error) {
	return e.enforcer.Enforce(sub, normalizeDomain(dom), obj, act)
}

// grantRoleBatch is BootstrapAdmin's fast path: one AddPolicies, which
// the adapter writes as one INSERT.
func (e *engine) grantRoleBatch(ctx context.Context, role string, perms []Permission) error {
	if len(perms) == 0 {
		return nil
	}
	rules := make([][]string, 0, len(perms))
	for _, p := range perms {
		rules = append(rules, []string{role, globalDomain, p.Object, p.Action})
	}
	if _, err := e.enforcer.AddPolicies(rules); err != nil {
		return fmt.Errorf("rbac: grant %s: %w", role, err)
	}
	for _, p := range perms {
		e.fireAudit(ctx, "GrantRole", role, p.Object, p.Action)
	}
	return nil
}

// close releases the watcher's subscription and clears the audit hook,
// so shutdown never races a policy callback.
func (e *engine) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.watcher != nil {
		e.watcher.Close()
		e.watcher = nil
	}
	e.audit.Store(nil)
}
