package rbac

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/persist"
	"github.com/redis/go-redis/v9"
)

type temporarilyFailingAdapter struct {
	persist.Adapter
	fail atomic.Bool
}

func (a *temporarilyFailingAdapter) LoadPolicy(m model.Model) error {
	if a.fail.Load() {
		return context.DeadlineExceeded
	}
	return a.Adapter.LoadPolicy(m)
}

func TestWatcher_PeriodicReloadRepairsMissedRevokeAndFailedReload(t *testing.T) {
	adapter, err := newGormAdapter(newAdapterDB(t))
	if err != nil {
		t.Fatal(err)
	}
	a, err := newEngine(rbacWithDomainsModel, adapter, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if err := (&PolicyService{e: a}).GrantRole(t.Context(), "reader", "book", "read"); err != nil {
		t.Fatal(err)
	}
	flaky := &temporarilyFailingAdapter{Adapter: adapter}
	b, err := newEngine(rbacWithDomainsModel, flaky, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	w, err := newRedisWatcher(t.Context(), newWatcherClient(t), "convergence", func(w *redisWatcher) { w.reloadInterval = 10 * time.Millisecond })
	if err != nil {
		t.Fatal(err)
	}
	if err := b.withWatcher(w); err != nil {
		w.Close()
		t.Fatal(err)
	}
	flaky.fail.Store(true)
	if !awaitCondition(t, time.Second, func() bool { return w.Stats().ReloadFailures > 0 }) {
		t.Fatal("periodic failure was not recorded")
	}
	// A has no watcher: this commit deliberately sends no notification.
	if err := (&PolicyService{e: a}).RevokeRole(t.Context(), "reader", "book", "read"); err != nil {
		t.Fatal(err)
	}
	flaky.fail.Store(false)
	if !awaitCondition(t, time.Second, func() bool {
		allow, err := b.decide("usr_reader", []string{"reader"}, "", "book", "read")
		return err == nil && !allow
	}) {
		t.Fatal("missed revocation did not converge")
	}
}

func TestWatcher_ReconnectReloadsWithoutWaitingForPeriodicTick(t *testing.T) {
	adapter, err := newGormAdapter(newAdapterDB(t))
	if err != nil {
		t.Fatal(err)
	}
	a, err := newEngine(rbacWithDomainsModel, adapter, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if err := (&PolicyService{e: a}).GrantRole(t.Context(), "reader", "book", "read"); err != nil {
		t.Fatal(err)
	}
	b, err := newEngine(rbacWithDomainsModel, adapter, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialTimeout: 50 * time.Millisecond, ReadTimeout: 50 * time.Millisecond, WriteTimeout: 50 * time.Millisecond})
	defer func() { _ = client.Close() }()
	w, err := newRedisWatcher(t.Context(), client, "reconnect", func(w *redisWatcher) { w.reloadInterval = time.Hour })
	if err != nil {
		t.Fatal(err)
	}
	if err := b.withWatcher(w); err != nil {
		w.Close()
		t.Fatal(err)
	}
	mr.Close()
	// Change the shared DB while no notification can reach B.
	if err := (&PolicyService{e: a}).RevokeRole(t.Context(), "reader", "book", "read"); err != nil {
		t.Fatal(err)
	}
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	if !awaitCondition(t, 3*time.Second, func() bool {
		allow, err := b.decide("usr_reader", []string{"reader"}, "", "book", "read")
		return err == nil && !allow
	}) {
		t.Fatal("reconnect left a stale policy until the periodic tick")
	}
}
