package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/scheduler"
	"github.com/zynthara/chok/v2/store"
)

func openCore(t *testing.T) *core {
	t.Helper()
	h := dbtest.Open(t)
	if err := MigrateSchema(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return newCore(h, log.Empty())
}

func enqueue(t *testing.T, c *core, topic, payload string) {
	t.Helper()
	err := c.h.RunInTx(context.Background(), func(txCtx context.Context) error {
		return c.Enqueue(txCtx, topic, []byte(payload))
	})
	if err != nil {
		t.Fatalf("enqueue %s/%s: %v", topic, payload, err)
	}
}

// insertAt writes a Record with a chosen created_at (and optional
// explicit id) directly — the tie/late-commit scenarios need positions
// Enqueue deliberately does not expose. Times are pre-truncated to the
// millisecond so every dialect round-trips them exactly.
func insertAt(t *testing.T, c *core, at time.Time, id uint, topic, payload string) {
	t.Helper()
	rec := Record{Topic: topic, Payload: []byte(payload)}
	rec.CreatedAt = at
	rec.ID = id
	if err := c.h.Unsafe(context.Background()).Create(&rec).Error; err != nil {
		t.Fatalf("insertAt: %v", err)
	}
}

// capture is a Handler that records deliveries and can be told to fail.
type capture struct {
	mu   sync.Mutex
	got  []Record
	fail func(Record) error
}

func (cp *capture) handle(_ context.Context, rec Record) error {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.fail != nil {
		if err := cp.fail(rec); err != nil {
			return err
		}
	}
	cp.got = append(cp.got, rec)
	return nil
}

func (cp *capture) payloads() []string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := make([]string, len(cp.got))
	for i, r := range cp.got {
		out[i] = string(r.Payload)
	}
	return out
}

func mkRelay(t *testing.T, c *core, name string, cp *capture, settle time.Duration, batch int, topics ...string) *relay[Record] {
	t.Helper()
	r, err := newRelay[Record](name, cp.handle, c, relayCfg{topics: topics}, settle, func() int { return batch })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func wantPayloads(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivered %v, want %v", got, want)
		}
	}
}

func TestEnqueue_TxGateAndAtomicity(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()

	// Outside any transaction: rejected.
	if err := c.Enqueue(ctx, "t", []byte("x")); !errors.Is(err, ErrOutsideTx) {
		t.Fatalf("outside tx: err = %v, want ErrOutsideTx", err)
	}
	// A transaction on ANOTHER handle: still rejected — that would not
	// be atomic with writes on the outbox's handle.
	other := dbtest.Open(t)
	err := other.RunInTx(ctx, func(txCtx context.Context) error {
		return c.Enqueue(txCtx, "t", []byte("x"))
	})
	if !errors.Is(err, ErrOutsideTx) {
		t.Fatalf("foreign tx: err = %v, want ErrOutsideTx", err)
	}
	// Topic validation.
	_ = c.h.RunInTx(ctx, func(txCtx context.Context) error {
		if err := c.Enqueue(txCtx, "", []byte("x")); !errors.Is(err, ErrTopicInvalid) {
			t.Fatalf("empty topic: err = %v, want ErrTopicInvalid", err)
		}
		long := make([]byte, MaxTopicLen+1)
		for i := range long {
			long[i] = 'a'
		}
		if err := c.Enqueue(txCtx, string(long), nil); !errors.Is(err, ErrTopicInvalid) {
			t.Fatalf("long topic: err = %v, want ErrTopicInvalid", err)
		}
		return nil
	})

	// Rollback drops the staged message with the business writes.
	sentinel := errors.New("boom")
	err = c.h.RunInTx(ctx, func(txCtx context.Context) error {
		if err := c.Enqueue(txCtx, "orders", []byte("rolled-back")); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback tx: err = %v", err)
	}
	var n int64
	if err := c.h.Unsafe(ctx).Model(&Record{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rows after rollback = %d, want 0", n)
	}

	// Commit makes it visible; EnqueueJSON round-trips.
	err = c.h.RunInTx(ctx, func(txCtx context.Context) error {
		return c.EnqueueJSON(txCtx, "orders", map[string]string{"id": "o_1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.records.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Topic != "orders" || string(page.Items[0].Payload) != `{"id":"o_1"}` {
		t.Fatalf("committed rows = %+v", page.Items)
	}
}

func TestRelay_DeliversInOrderAndAdvancesWatermark(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		enqueue(t, c, "t", fmt.Sprintf("m%d", i))
	}
	cp := &capture{}
	r := mkRelay(t, c, "orderly", cp, 0, 2) // settle 0: everything settles immediately

	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"m1", "m2", "m3", "m4", "m5"})

	w, err := c.states.load(ctx, "orderly")
	if err != nil {
		t.Fatal(err)
	}
	if !w.ok {
		t.Fatal("watermark not persisted after a fully settled sweep")
	}
	// A second sweep delivers nothing (keyset skip, no duplicates).
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"m1", "m2", "m3", "m4", "m5"})

	// New rows continue from the watermark.
	enqueue(t, c, "t", "m6")
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"m1", "m2", "m3", "m4", "m5", "m6"})
}

func TestRelay_AtLeastOnce_FailedHandlerRetriesInOrder(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "t", fmt.Sprintf("m%d", i))
	}
	boom := errors.New("downstream down")
	cp := &capture{fail: func(rec Record) error {
		if string(rec.Payload) == "m2" {
			return boom
		}
		return nil
	}}
	r := mkRelay(t, c, "retry", cp, 0, 10)

	// First sweep: m1 delivered, m2 fails, m3 not attempted
	// (head-of-line), watermark parks before m2.
	if err := r.run(ctx); !errors.Is(err, boom) {
		t.Fatalf("sweep err = %v, want the handler error", err)
	}
	wantPayloads(t, cp.payloads(), []string{"m1"})

	// Heal the handler: the retry resumes at m2 and keeps order.
	cp.mu.Lock()
	cp.fail = nil
	cp.mu.Unlock()
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"m1", "m2", "m3"})
}

func TestRelay_CrashRecovery_RedeliversUnsettledOnceEach(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "t", fmt.Sprintf("m%d", i))
	}
	// settle = 1h: rows stay unsettled, so the watermark is never
	// persisted — process memory is the only dedup.
	cp := &capture{}
	r := mkRelay(t, c, "crashy", cp, time.Hour, 10)
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.run(ctx); err != nil { // same process: mem dedups
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"m1", "m2", "m3"})
	if w, err := c.states.load(ctx, "crashy"); err != nil || w.ok {
		t.Fatalf("watermark = %+v, %v — must not persist inside the settle window", w, err)
	}

	// "Crash": a fresh relay instance (same name) loses mem and
	// replays the whole unsettled window — at-least-once, exactly the
	// contract. A consumer-side dedup key absorbs the duplicates.
	seen := map[string]int{}
	var mu sync.Mutex
	restarted, err := newRelay[Record]("crashy", func(_ context.Context, rec Record) error {
		mu.Lock()
		defer mu.Unlock()
		seen[string(rec.Payload)]++
		return nil
	}, c, relayCfg{}, time.Hour, func() int { return 10 })
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"m1", "m2", "m3"} {
		if seen[m] != 1 {
			t.Fatalf("replayed window: seen=%v, want each once", seen)
		}
	}
}

func TestRelay_SettleGatesWatermarkNotDelivery(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	enqueue(t, c, "t", "m1")
	enqueue(t, c, "t", "m2")

	cp := &capture{}
	r := mkRelay(t, c, "settling", cp, 30*time.Second, 10)
	base := time.Now()
	r.now = func() time.Time { return base } // rows are younger than settle

	// Delivery happens immediately; the watermark does not move.
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"m1", "m2"})
	if w, _ := c.states.load(ctx, "settling"); w.ok {
		t.Fatal("watermark persisted inside the settle window")
	}

	// Advance the clock past the settle window: the next sweep
	// re-scans (mem dedups, nothing is redelivered) and persists.
	r.now = func() time.Time { return base.Add(time.Minute) }
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"m1", "m2"})
	w, err := c.states.load(ctx, "settling")
	if err != nil {
		t.Fatal(err)
	}
	if !w.ok {
		t.Fatal("watermark still unpersisted after the window settled")
	}
	if len(r.mem) != 0 {
		t.Fatalf("mem not pruned after settle: %v", r.mem)
	}
}

func TestRelay_TieGroupLargerThanBatchProgresses(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	ts := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	const n = 25
	for i := 1; i <= n; i++ {
		insertAt(t, c, ts, 0, "t", fmt.Sprintf("m%02d", i))
	}
	cp := &capture{}
	r := mkRelay(t, c, "ties", cp, 0, 4) // batch far smaller than the tie group

	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	want := make([]string, n)
	for i := range want {
		want[i] = fmt.Sprintf("m%02d", i+1)
	}
	wantPayloads(t, cp.payloads(), want)

	// And the whole group settled: watermark sits at the last id.
	w, err := c.states.load(ctx, "ties")
	if err != nil {
		t.Fatal(err)
	}
	if !w.ok || !w.At.Equal(ts) {
		t.Fatalf("watermark = %+v, want at %v", w, ts)
	}
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), want) // no duplicates across the boundary
}

func TestRelay_LateCommitInsideSettleIsCaught(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	ts := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Second)
	// Two rows committed and swept first; ids leave a gap where the
	// late transaction's row will land.
	insertAt(t, c, ts, 10, "t", "early-a")
	insertAt(t, c, ts, 12, "t", "early-b")

	cp := &capture{}
	r := mkRelay(t, c, "late", cp, time.Hour, 10) // settle window still open
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"early-a", "early-b"})

	// The late commit becomes visible after the sweep, with an id and
	// created_at BEFORE rows already delivered (allocated earlier,
	// committed later). Inside the settle window the watermark has not
	// advanced, so the Gte overlap re-scan picks it up.
	insertAt(t, c, ts, 11, "t", "late-comer")
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"early-a", "early-b", "late-comer"})
}

func TestRelay_ConcurrentEnqueue_NoLoss(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()

	var mu sync.Mutex
	seen := map[string]int{}
	r, err := newRelay[Record]("concurrent", func(_ context.Context, rec Record) error {
		mu.Lock()
		defer mu.Unlock()
		seen[string(rec.Payload)]++
		return nil
	}, c, relayCfg{}, 0, func() int { return 7 })
	if err != nil {
		t.Fatal(err)
	}

	const writers, perWriter = 4, 25
	var wg sync.WaitGroup
	for wr := 0; wr < writers; wr++ {
		wg.Add(1)
		go func(wr int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				err := c.h.RunInTx(ctx, func(txCtx context.Context) error {
					return c.Enqueue(txCtx, "t", []byte(fmt.Sprintf("w%d-m%d", wr, i)))
				})
				if err != nil {
					t.Errorf("enqueue: %v", err)
					return
				}
			}
		}(wr)
	}
	// Sweep concurrently with the writers, then once more after they
	// finish: nothing committed may be missed, nothing delivered twice.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		if err := r.run(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			if err := r.run(ctx); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != writers*perWriter {
				t.Fatalf("delivered %d distinct messages, want %d", len(seen), writers*perWriter)
			}
			for k, n := range seen {
				if n != 1 {
					t.Fatalf("message %s delivered %d times in one process", k, n)
				}
			}
			return
		default:
		}
	}
}

func TestRelay_TopicFilterAndIndependentWatermarks(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	enqueue(t, c, "a", "a1")
	enqueue(t, c, "b", "b1")
	enqueue(t, c, "a", "a2")
	enqueue(t, c, "b", "b2")

	cpA, cpB := &capture{}, &capture{}
	ra := mkRelay(t, c, "only-a", cpA, 0, 10, "a")
	rb := mkRelay(t, c, "only-b", cpB, 0, 10, "b")

	if err := ra.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cpA.payloads(), []string{"a1", "a2"})
	if len(cpB.payloads()) != 0 {
		t.Fatal("relay B ran early")
	}
	if err := rb.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cpB.payloads(), []string{"b1", "b2"})

	wa, _ := c.states.load(ctx, "only-a")
	wb, _ := c.states.load(ctx, "only-b")
	if !wa.ok || !wb.ok {
		t.Fatalf("watermarks not persisted: a=%+v b=%+v", wa, wb)
	}
	// Progress is per relay: replaying B must not disturb A's state.
	if err := rb.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cpB.payloads(), []string{"b1", "b2"})
}

func TestRelay_RunNowOverlapReturnsBusy(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	enqueue(t, c, "t", "m1")

	release := make(chan struct{})
	started := make(chan struct{})
	r, err := newRelay[Record]("busy", func(_ context.Context, _ Record) error {
		close(started)
		<-release
		return nil
	}, c, relayCfg{}, 0, func() int { return 10 })
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = r.run(ctx) }()
	<-started
	if err := r.run(ctx); !errors.Is(err, scheduler.ErrBusy) {
		t.Fatalf("overlapping run err = %v, want scheduler.ErrBusy", err)
	}
	close(release)
}

// outboxEvent is the WithRelayFor escape-hatch fixture: a user-owned
// append-only table delivered by the same engine.
type outboxEvent struct {
	db.AppendOnlyModel
	Kind string `gorm:"type:varchar(40)"`
}

func (outboxEvent) TableName() string { return "outbox_test_events" }

func TestRelayFor_GenericAppendModel(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	if err := c.h.Migrate(ctx, db.Table(&outboxEvent{})); err != nil {
		t.Fatal(err)
	}
	events := store.NewAppend[outboxEvent](c.h, log.Empty(), store.WithQueryFields("created_at"))
	for _, k := range []string{"k1", "k2", "k3"} {
		if err := events.Create(ctx, &outboxEvent{Kind: k}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var kinds []string
	r, err := newRelay[outboxEvent]("generic", func(_ context.Context, ev outboxEvent) error {
		mu.Lock()
		defer mu.Unlock()
		kinds = append(kinds, ev.Kind)
		return nil
	}, c, relayCfg{}, 0, func() int { return 2 })
	if err != nil {
		t.Fatal(err)
	}
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(kinds) != 3 || kinds[0] != "k1" || kinds[2] != "k3" {
		t.Fatalf("generic relay delivered %v", kinds)
	}
	if w, err := c.states.load(ctx, "generic"); err != nil || !w.ok {
		t.Fatalf("generic relay watermark = %+v, %v", w, err)
	}
}

func TestCleanup_MinWatermarkFloorAndRetention(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	for i := 1; i <= 4; i++ {
		enqueue(t, c, "t", fmt.Sprintf("m%d", i))
	}
	cp := &capture{}
	r := mkRelay(t, c, "sweeper", cp, 0, 10)
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}

	// Zero registered record relays: never delete.
	if n, err := c.cleanupOnce(ctx, nil, nil, time.Nanosecond, 10); err != nil || n != 0 {
		t.Fatalf("cleanup with no record relays = %d, %v", n, err)
	}
	// A registered record relay without its own state row: never
	// delete.
	if n, err := c.cleanupOnce(ctx, []string{"sweeper", "lagging"}, nil, time.Nanosecond, 10); err != nil || n != 0 {
		t.Fatalf("cleanup with lagging relay = %d, %v", n, err)
	}

	// One relay, fully settled: retention floor applies. Everything
	// except the watermark row itself is deletable (strict <).
	n, err := c.cleanupOnce(ctx, []string{"sweeper"}, nil, time.Nanosecond, 2) // batch smaller than the sweep
	if err != nil {
		t.Fatal(err)
	}
	var left int64
	if err := c.h.Unsafe(ctx).Model(&Record{}).Count(&left).Error; err != nil {
		t.Fatal(err)
	}
	if n == 0 || left != 4-n {
		t.Fatalf("cleanup deleted %d, rows left %d", n, left)
	}
	// The rows at the watermark position survive (strict less-than).
	if left == 0 {
		t.Fatal("cleanup must not delete the watermark row itself")
	}

	// A stale relay_state row of an UNKNOWN name (decommissioned
	// relay, old watermark) lowers the floor and blocks further
	// cleanup — documented safe direction.
	old := watermark{At: time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Millisecond), ID: 1, ok: true}
	if err := c.states.save(ctx, "stale-relay", old); err != nil {
		t.Fatal(err)
	}
	enqueue(t, c, "t", "m5")
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := c.cleanupOnce(ctx, []string{"sweeper"}, nil, time.Nanosecond, 10); err != nil || n != 0 {
		t.Fatalf("cleanup past a stale relay = %d, %v — the stale watermark must floor it", n, err)
	}

	// Retention keeps young rows even below every watermark.
	if err := c.h.Unsafe(ctx).Where("relay_name = ?", "stale-relay").Delete(&relayState{}).Error; err != nil {
		t.Fatal(err)
	}
	if n, err := c.cleanupOnce(ctx, []string{"sweeper"}, nil, 24*time.Hour, 10); err != nil || n != 0 {
		t.Fatalf("cleanup with long retention = %d, %v — young rows must survive", n, err)
	}
}

// Round-1 review #1: a residual state row (decommissioned relay) must
// not stand in for a registered relay that has not advanced yet — the
// old row-count guard passed with rows == registered even though the
// lagging relay's row was missing, deleting messages it never saw.
func TestCleanup_Round1ResidualStateCannotStandInForLaggingRelay(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "t", fmt.Sprintf("m%d", i))
	}
	// Relay A delivered everything; relay B (registered) has failed
	// since boot — no state row. A residual row of decommissioned X
	// sits at a high watermark, making the row COUNT match the
	// registered count.
	cpA := &capture{}
	ra := mkRelay(t, c, "A", cpA, 0, 10)
	if err := ra.run(ctx); err != nil {
		t.Fatal(err)
	}
	wa, err := c.states.load(ctx, "A")
	if err != nil || !wa.ok {
		t.Fatalf("relay A watermark = %+v, %v", wa, err)
	}
	if err := c.states.save(ctx, "X", wa); err != nil {
		t.Fatal(err)
	}

	if n, err := c.cleanupOnce(ctx, []string{"A", "B"}, nil, time.Nanosecond, 10); err != nil || n != 0 {
		t.Fatalf("cleanup = %d, %v — X's residual row must not cover for lagging B", n, err)
	}
	var left int64
	if err := c.h.Unsafe(ctx).Model(&Record{}).Count(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left != 3 {
		t.Fatalf("rows left = %d, want all 3 kept for relay B", left)
	}
}

// Round-1 review #2: a WithRelayFor watermark tracks a user-owned
// table — it must neither authorise deleting outbox_messages (only
// generic relays registered ⇒ no cleanup) nor block the Record-relay
// floor (registered generic names are excluded from the min).
func TestCleanup_Round1GenericRelayCannotAuthorizeMessageDeletion(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	if err := c.h.Migrate(ctx, db.Table(&outboxEvent{})); err != nil {
		t.Fatal(err)
	}
	// Pending messages nobody consumes, and a generic relay over its
	// own table that is fully caught up (fresh, high watermark).
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "t", fmt.Sprintf("m%d", i))
	}
	events := store.NewAppend[outboxEvent](c.h, log.Empty(), store.WithQueryFields("created_at"))
	if err := events.Create(ctx, &outboxEvent{Kind: "k"}); err != nil {
		t.Fatal(err)
	}
	rg, err := newRelay[outboxEvent]("G", func(context.Context, outboxEvent) error { return nil },
		c, relayCfg{}, 0, func() int { return 10 })
	if err != nil {
		t.Fatal(err)
	}
	if err := rg.run(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := c.cleanupOnce(ctx, nil, []string{"G"}, time.Nanosecond, 10); err != nil || n != 0 {
		t.Fatalf("cleanup = %d, %v — a generic watermark must not delete outbox_messages", n, err)
	}
	var left int64
	if err := c.h.Unsafe(ctx).Model(&Record{}).Count(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left != 3 {
		t.Fatalf("rows left = %d, want all 3 messages kept", left)
	}

	// The flip side: a registered generic relay with an OLD watermark
	// (its table is quiet) must not hold the Record floor down.
	cp := &capture{}
	rr := mkRelay(t, c, "R", cp, 0, 10)
	if err := rr.run(ctx); err != nil {
		t.Fatal(err)
	}
	oldW := watermark{At: time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Millisecond), ID: 1, ok: true}
	if err := c.states.save(ctx, "G", oldW); err != nil {
		t.Fatal(err)
	}
	n, err := c.cleanupOnce(ctx, []string{"R"}, []string{"G"}, time.Nanosecond, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("cleanup deleted nothing — G's generic watermark must be excluded from the floor")
	}
}

// Round-1 review #3: mem is pruned on every watermark advance (batch
// boundaries and failure-path saves), not only at the end of a fully
// successful sweep — otherwise a long catch-up holds the entire
// processed backlog in memory and an early error return leaks every
// settled entry for the relay's lifetime.
func TestRelay_Round1MemPrunedOnEveryAdvance(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	for i := 1; i <= 6; i++ {
		enqueue(t, c, "t", fmt.Sprintf("m%d", i))
	}
	boom := errors.New("poison")
	cp := &capture{fail: func(rec Record) error {
		if string(rec.Payload) == "m5" {
			return boom
		}
		return nil
	}}
	r := mkRelay(t, c, "pruned", cp, 0, 2) // settle 0: every delivered row settles
	if err := r.run(ctx); !errors.Is(err, boom) {
		t.Fatalf("sweep err = %v, want the poison error", err)
	}
	// m1..m4 delivered and settled across two batches; the failure
	// path persisted the watermark at m4 — nothing settled may remain
	// in mem.
	if len(r.mem) != 0 {
		t.Fatalf("mem after failed sweep = %v, want empty (settled entries pruned)", r.mem)
	}
	w, err := c.states.load(ctx, "pruned")
	if err != nil || !w.ok {
		t.Fatalf("watermark = %+v, %v", w, err)
	}
}
