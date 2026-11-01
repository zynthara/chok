package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestRelay_LateCommitBehindWatermarkIsDelivered(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	inserted, release, committed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		committed <- c.d.RunInTx(ctx, func(tx context.Context) error {
			if err := c.enqueue(tx, "late", []byte("1")); err != nil {
				return err
			}
			close(inserted)
			<-release
			return nil
		})
	}()
	<-inserted
	time.Sleep(20 * time.Millisecond)
	hooked := false
	err := c.d.Unsafe(ctx).Callback().Query().After("gorm:query").Register("architecture:late_commit", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]message); ok && !hooked {
			hooked = true
			close(release)
			if err := <-committed; err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.d.Unsafe(ctx).Callback().Query().Remove("architecture:late_commit") }()
	calls := 0
	r, err := newRelay("late", func(context.Context, rawMessage) error { calls++; return nil }, c, time.Millisecond, 10)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := r.run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	w, err := c.states.load(ctx, "late")
	if err != nil {
		t.Fatal(err)
	}
	if !hooked || calls != 1 || !w.ok {
		t.Fatalf("hook=%v calls=%d watermark=%+v", hooked, calls, w)
	}

	enqueue(t, c, "other", "2")
	time.Sleep(5 * time.Millisecond)
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := c.cleanupOnce(ctx, []string{"late"}, time.Millisecond, 100)
	if err != nil {
		t.Fatal(err)
	}
	var remaining int64
	if err := c.d.Unsafe(ctx).Model(&message{}).Where("topic = ?", "late").Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if calls != 1 || remaining != 0 || n < 1 {
		t.Fatalf("calls=%d remaining=%d deleted=%d", calls, remaining, n)
	}

}

func TestDeadRemoval_DoesNotReviveOriginal(t *testing.T) {
	for _, op := range []string{"discard", "requeue"} {
		t.Run(op, func(t *testing.T) {
			c := openCore(t)
			ctx := t.Context()
			enqueue(t, c, "dead", "1")
			first, err := newRelay("dead", func(context.Context, rawMessage) error { return errors.New("poison") }, c, time.Hour, 10)
			if err != nil {
				t.Fatal(err)
			}
			first.retry.maxAttempts = 1
			if err := first.run(ctx); err != nil {
				t.Fatal(err)
			}
			j := &Jobs{core: c}
			dead, err := j.Dead(ctx, "dead", 0)
			if err != nil || len(dead) != 1 {
				t.Fatalf("%v %v", dead, err)
			}
			if op == "discard" {
				err = j.Discard(ctx, dead[0].ID)
			} else {
				err = j.Requeue(ctx, dead[0].ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			var ids []uint64
			restarted, err := newRelay("dead", func(_ context.Context, m rawMessage) error { ids = append(ids, m.ID); return nil }, c, time.Hour, 10)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.run(ctx); err != nil {
				t.Fatal(err)
			}
			if op == "discard" && len(ids) != 0 {
				t.Fatalf("discarded original revived: %v", ids)
			}
			if op == "requeue" && (len(ids) != 1 || ids[0] == dead[0].ID) {
				t.Fatalf("requeue deliveries: %v", ids)
			}

		})
	}
}

func TestRelay_CancellationDoesNotEraseBusinessFailure(t *testing.T) {
	c := openCore(t)
	enqueue(t, c, "cancel", "1")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r, err := newRelay("cancel", func(context.Context, rawMessage) error { cancel(); return errors.New("business failure") }, c, time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.run(ctx); err == nil {
		t.Fatal("expected failure")
	}
	var row message
	if err := c.d.Unsafe(t.Context()).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Attempts != 1 {
		t.Fatalf("attempts=%d", row.Attempts)
	}

}

func TestCleanup_PreservesUnregisteredTopic(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Millisecond)
	insertAt(t, c, now.Add(-2*time.Hour), 0, "future-worker", "1")
	insertAt(t, c, now, 0, "active", "2")
	calls := 0
	r, err := newRelay("active", func(context.Context, rawMessage) error { calls++; return nil }, c, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := c.cleanupOnce(ctx, []string{"active"}, time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	var orphan int64
	if err := c.d.Unsafe(ctx).Model(&message{}).Where("topic = ?", "future-worker").Count(&orphan).Error; err != nil {
		t.Fatal(err)
	}
	if calls != 1 || n != 0 || orphan != 1 {
		t.Fatalf("calls=%d deleted=%d orphan=%d", calls, n, orphan)
	}

}

func TestRelay_FailedAcknowledgementRedeliversSameID(t *testing.T) {
	c := openCore(t)
	enqueue(t, c, "ack", "1")
	fail := true
	if err := c.d.Unsafe(t.Context()).Callback().Update().Before("gorm:update").Register("test:ack_failure", func(tx *gorm.DB) {
		if fail {
			fail = false
			_ = tx.AddError(errors.New("ack unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.d.Unsafe(t.Context()).Callback().Update().Remove("test:ack_failure") }()
	var ids []uint64
	handler := func(_ context.Context, m rawMessage) error { ids = append(ids, m.ID); return nil }
	r, _ := newRelay("ack", handler, c, time.Hour, 10)
	if err := r.run(t.Context()); err == nil {
		t.Fatal("ack failure did not fail the sweep")
	}
	restarted, _ := newRelay("ack", handler, c, time.Hour, 10)
	if err := restarted.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("lost idempotency key: %v", ids)
	}
	if err := restarted.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("durable ack repeated: %v", ids)
	}
}
