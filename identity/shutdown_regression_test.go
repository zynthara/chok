package identity

import (
	"context"
	"testing"
	"time"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/internal/dbtest"
)

func TestAudit_CloseUnblocksBackpressuredLog(t *testing.T) {
	if dbtest.Driver() != "sqlite" {
		t.Skip("requires the single SQLite writer")
	}
	idn := newIdentity(t, Config{Audit: AuditConfig{Enabled: true, AsyncBuffer: 1}})
	a := idn.audit
	logctx, stopLog := context.WithCancel(t.Context())
	defer stopLog()
	logsDone := make(chan struct{})
	closed := make(chan error, 1)
	err := idn.d.RunInTx(t.Context(), func(context.Context) error {
		go func() {
			defer close(logsDone)
			for range 105 {
				a.Log(logctx, Event{Action: "probe"})
			}
		}()
		pool, _ := idn.d.Unsafe(t.Context()).DB()
		until := time.Now().Add(2 * time.Second)
		blocked := false
		for time.Now().Before(until) {
			if len(a.in) == 1 && pool.Stats().WaitCount > 0 {
				if a.mu.TryLock() {
					a.mu.Unlock()
				} else {
					blocked = true
					break
				}
			}
			time.Sleep(time.Millisecond)
		}
		if !blocked {
			stopLog()
			t.Error("could not saturate audit queue")
			return nil
		}
		closeCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		go func() { closed <- a.close(closeCtx) }()
		select {
		case err := <-closed:
			if err == nil {
				t.Error("unfinished audit close must report timeout")
			}
		case <-time.After(200 * time.Millisecond):
			t.Error("Close exceeded deadline while Log held a read lock")
		}
		stopLog()
		<-logsDone
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

}

func TestAudit_ListRequiresAuthorization(t *testing.T) {
	idn := auditOn(t, AuditConfig{})
	if _, err := idn.audit.ListFromQuery(t.Context(), nil); err == nil {
		t.Fatal("anonymous audit reader admitted")
	}
	ctx := chok.WithPrincipal(t.Context(), chok.Principal{UserID: "usr_reader"})
	if _, err := idn.audit.ListFromQuery(ctx, nil); err == nil {
		t.Fatal("unauthorized audit reader admitted")
	}
	if _, err := idn.audit.ListFromQuery(auditReader(t.Context()), nil); err != nil {
		t.Fatal(err)
	}
}
