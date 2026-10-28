package rbac

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
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
	// NewSyncedEnforcer loads the policies, once, through the adapter: a
	// missing table fails here.
	enf, err := casbinv3.NewSyncedEnforcer(m, adapter)
	if err != nil {
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

// adminRole is the built-in administrator role: a user whose roles hold
// it passes every decision, in every domain, without a policy.
const adminRole = "admin"

// decide admits a user with roles — the global roles identity keeps, the
// chok.Principal's Roles — to act on obj in dom ("" is the global scope):
// the role admin always; otherwise a policy casbin matches for the user
// (a direct grant, or a role bound in dom) or for any of the roles. A
// global policy counts in every domain.
func (e *engine) decide(userID string, roles []string, dom, obj, act string) (bool, error) {
	if slices.Contains(roles, adminRole) {
		return true, nil
	}
	dom = normalizeDomain(dom)
	for _, sub := range append([]string{userID}, roles...) {
		if sub == "" {
			continue
		}
		ok, err := e.enforcer.Enforce(sub, dom, obj, act)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
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
