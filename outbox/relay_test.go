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

// Round-2 review #1 (re-cut by round-3): the memory concern — mem
// growing while nothing settles — is bounded ACROSS sweeps, not by
// moving the cutoff inside one (round-3 found the per-batch refresh
// unsound: it can cover positions the cursor passed while a legal
// transaction was still invisible). Each sweep takes a fresh pre-scan
// cutoff, so rows delivered unsettled in one sweep settle, persist
// and prune on a later sweep once wall time passes.
func TestRelay_Round2MemBoundedAcrossSweeps(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	ts := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	const n = 25
	for i := 1; i <= n; i++ {
		insertAt(t, c, ts, 0, "t", fmt.Sprintf("m%02d", i))
	}
	cp := &capture{}
	r := mkRelay(t, c, "cross-sweep", cp, 10*time.Minute, 5)
	clock := ts.Add(time.Minute) // sweep 1: everything unsettled
	r.now = func() time.Time { return clock }

	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cp.payloads()) != n {
		t.Fatalf("delivered %d rows, want %d", len(cp.payloads()), n)
	}
	if len(r.mem) != n {
		t.Fatalf("mem after unsettled sweep = %d, want %d", len(r.mem), n)
	}
	if w, _ := c.states.load(ctx, "cross-sweep"); w.ok {
		t.Fatalf("watermark persisted inside the settle window: %+v", w)
	}

	// Sweep 2 under a later clock: the same rows are now settled — the
	// rescan (mem-dedup, no redelivery) advances and prunes.
	clock = ts.Add(15 * time.Minute)
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cp.payloads()) != n {
		t.Fatalf("redelivered on settle sweep: %d", len(cp.payloads()))
	}
	w, err := c.states.load(ctx, "cross-sweep")
	if err != nil {
		t.Fatal(err)
	}
	if !w.ok || !w.At.Equal(ts) || w.ID != n {
		t.Fatalf("watermark = %+v, want settled at (%v, %d)", w, ts, n)
	}
	if len(r.mem) != 0 {
		t.Fatalf("mem after settle sweep = %d entries, want 0", len(r.mem))
	}
}

// Round-2 review #2: run prunes mem against the loaded watermark
// before scanning. When another instance (last-write-wins degradation)
// advances the shared state past rows this instance delivered but
// never settled locally, those mem entries would otherwise outlive
// every local advance.
func TestRelay_Round2LoadedWatermarkPrunesForeignAdvance(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "t", fmt.Sprintf("m%d", i))
	}
	cp := &capture{}
	r := mkRelay(t, c, "shared", cp, time.Hour, 10) // unsettled: mem only
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.mem) != 3 {
		t.Fatalf("mem after first sweep = %d, want 3 unsettled entries", len(r.mem))
	}
	// "Instance B" advances the shared state past everything.
	page, err := c.records.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last := page.Items[len(page.Items)-1]
	if err := c.states.save(ctx, "shared", watermark{At: last.CreatedAt, ID: last.ID, ok: true}); err != nil {
		t.Fatal(err)
	}

	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"m1", "m2", "m3"}) // keyset skip, no redelivery
	if len(r.mem) != 0 {
		t.Fatalf("mem after foreign advance = %v, want empty (pruned on load)", r.mem)
	}
}

// Round-2 review #3: a topic-filtered relay whose topic stays quiet
// advances its watermark over foreign-topic rows to the settled
// frontier — otherwise it never grows a state row and the per-name
// retention guard (round-1 #1) blocks cleanup forever.
func TestCleanup_Round2QuietFilteredRelayDoesNotBlockRetention(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "hot", fmt.Sprintf("h%d", i))
	}
	cpHot := &capture{}
	hot := mkRelay(t, c, "hot-consumer", cpHot, 0, 10)
	if err := hot.run(ctx); err != nil {
		t.Fatal(err)
	}
	// The cold relay's topic has never appeared; its sweep delivers
	// nothing but must still claim the settled frontier.
	cpCold := &capture{}
	cold := mkRelay(t, c, "cold-consumer", cpCold, 0, 10, "cold")
	if err := cold.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cpCold.payloads()) != 0 {
		t.Fatalf("cold relay delivered %v, want nothing", cpCold.payloads())
	}
	wc, err := c.states.load(ctx, "cold-consumer")
	if err != nil {
		t.Fatal(err)
	}
	if !wc.ok {
		t.Fatal("quiet filtered relay has no watermark — it would block retention forever")
	}
	n, err := c.cleanupOnce(ctx, []string{"hot-consumer", "cold-consumer"}, nil, time.Nanosecond, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("cleanup deleted nothing — the quiet filtered relay stalled the floor")
	}

	// Correctness half: the jumped watermark must not skip topic rows
	// that arrive later.
	enqueue(t, c, "cold", "c1")
	if err := cold.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cpCold.payloads(), []string{"c1"})
}

// Round-3 review (Critical): the settle cutoff is taken once, before
// the scan. A per-batch refresh could cover a position the cursor
// passed while a perfectly legal transaction (commit within settle of
// its INSERT) was still invisible there — the row commits after the
// pass, later batches settle under the refreshed cutoff, and the
// watermark jumps the row forever. Timeline: the cursor passes R's
// position while R's transaction is open; mid-sweep R commits and the
// wall clock crosses settle; the remaining rows must NOT settle under
// this sweep's cutoff, so the next sweep's overlap rescan still finds
// R.
func TestRelay_Round3LateCommitDuringLongSweepNotSkipped(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	s := time.Now().UTC().Truncate(time.Millisecond)
	for i := 1; i <= 4; i++ { // ids 1-4: settled at sweep start
		insertAt(t, c, s.Add(-60*time.Second), 0, "t", fmt.Sprintf("a%d", i))
	}
	for i := 0; i < 4; i++ { // ids 6-9: unsettled, positions after the gap
		insertAt(t, c, s.Add(-10*time.Second), uint(6+i), "t", fmt.Sprintf("b%d", i+1))
	}
	for i := 0; i < 4; i++ { // ids 10-13: unsettled tail
		insertAt(t, c, s.Add(-5*time.Second), uint(10+i), "t", fmt.Sprintf("c%d", i+1))
	}

	clock := s
	var mu sync.Mutex
	var got []string
	r, err := newRelay[Record]("long-sweep", func(_ context.Context, rec Record) error {
		mu.Lock()
		defer mu.Unlock()
		if string(rec.Payload) == "b1" {
			// The cursor has passed the (s-20s, 5) position. Now the
			// held transaction commits R there, and the sweep drags on
			// past the settle window.
			insertAt(t, c, s.Add(-20*time.Second), 5, "t", "late-R")
			clock = s.Add(40 * time.Second)
		}
		got = append(got, string(rec.Payload))
		return nil
	}, c, relayCfg{}, 30*time.Second, func() int { return 4 })
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return clock }

	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	// The next sweep's overlap rescan must deliver R — a refreshed
	// cutoff would have settled the b/c rows and parked the watermark
	// past R's position.
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	count := 0
	for _, p := range got {
		if p == "late-R" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("late-committed row delivered %d times, want exactly 1 (got %v)", count, got)
	}
	if len(got) != 13 {
		t.Fatalf("delivered %d rows, want 13: %v", len(got), got)
	}
}

// Round-3 review: one sweep is bounded by its page budget; the next
// tick rescans the overlap from the persisted watermark under a fresh
// cutoff. This is what reconciles the fixed pre-scan cutoff with
// sustained production (the round-2 #1 memory concern).
func TestRelay_Round3SweepBudgetBoundsOnePass(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	const n = 20
	for i := 1; i <= n; i++ {
		enqueue(t, c, "t", fmt.Sprintf("m%02d", i))
	}
	cp := &capture{}
	r := mkRelay(t, c, "budgeted", cp, 0, 4)
	r.maxPages = 3 // 3 pages × batch 4 = 12 rows per sweep

	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cp.payloads()) != 12 {
		t.Fatalf("budgeted sweep delivered %d rows, want 12", len(cp.payloads()))
	}
	w, err := c.states.load(ctx, "budgeted")
	if err != nil || !w.ok {
		t.Fatalf("watermark after budgeted sweep = %+v, %v", w, err)
	}
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	want := make([]string, n)
	for i := range want {
		want[i] = fmt.Sprintf("m%02d", i+1)
	}
	wantPayloads(t, cp.payloads(), want) // remainder delivered, in order, no dupes
}

// Round-3 review: the frontier probe reuses the sweep's pre-scan
// cutoff. A probe-time cutoff could settle a matching row that
// committed after the filtered cursor passed its position — the jump
// would cover a message the scan never saw.
func TestRelay_Round3FrontierUsesPreScanCutoff(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	s := time.Now().UTC().Truncate(time.Millisecond)
	for i := 1; i <= 3; i++ { // foreign rows, unsettled at sweep start
		insertAt(t, c, s.Add(-10*time.Second), 0, "hot", fmt.Sprintf("h%d", i))
	}
	cpCold := &capture{}
	cold := mkRelay(t, c, "cold-frontier", cpCold, 30*time.Second, 10, "cold")
	calls := 0
	cold.now = func() time.Time {
		calls++
		if calls == 1 {
			return s
		}
		return s.Add(40 * time.Second)
	}

	// Sweep 1: nothing matches, nothing is settled under the pre-scan
	// cutoff — the watermark must NOT appear (a probe recomputing "now"
	// would see the foreign rows as settled and claim their position).
	if err := cold.run(ctx); err != nil {
		t.Fatal(err)
	}
	if w, _ := c.states.load(ctx, "cold-frontier"); w.ok {
		t.Fatalf("frontier claimed inside the settle window: %+v", w)
	}

	// A cold message "ages in" at a position before the foreign rows —
	// with the premature frontier above it would have been skipped.
	insertAt(t, c, s.Add(-20*time.Second), 0, "cold", "c1")
	if err := cold.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cpCold.payloads(), []string{"c1"})
	w, err := c.states.load(ctx, "cold-frontier")
	if err != nil || !w.ok {
		t.Fatalf("frontier after settled sweep = %+v, %v", w, err)
	}
}

// lateCommitRealTx is the real-transaction shape of the round-3
// Critical: a concurrently held transaction enqueues, the relay scans
// past the invisible position, the transaction commits within its
// settle budget — the overlap rescan must still deliver it. Real
// database lanes (MySQL/Postgres) exercise true concurrent visibility;
// the sqlite shape cannot race (single write connection).
func lateCommitRealTx(t *testing.T, c *core) {
	t.Helper()
	ctx := context.Background()
	const settle = 500 * time.Millisecond
	cp := &capture{}
	r := mkRelay(t, c, "late-real", cp, settle, 10)

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.h.RunInTx(ctx, func(txCtx context.Context) error {
			if err := c.Enqueue(txCtx, "t", []byte("late-tx")); err != nil {
				return err
			}
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	enqueue(t, c, "t", "visible") // later position, committed first

	if err := r.run(ctx); err != nil { // passes the invisible position
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"visible"})

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	time.Sleep(settle + 200*time.Millisecond) // everything ages past settle

	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, p := range cp.payloads() {
		seen[p]++
	}
	if seen["late-tx"] != 1 || seen["visible"] != 1 || len(seen) != 2 {
		t.Fatalf("delivered = %v, want visible and late-tx exactly once each", cp.payloads())
	}
	w, err := c.states.load(ctx, "late-real")
	if err != nil || !w.ok {
		t.Fatalf("watermark = %+v, %v", w, err)
	}
}

func TestOutboxRelay_LateCommitRealTx(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("real concurrent-commit visibility needs the postgres lane (sqlite serialises writes)")
	}
	c := openCore(t)
	lateCommitRealTx(t, c)
}

// Round-4 review (Critical): a settled tie group wider than one
// sweep's page budget must not starve its own tail. The old scan
// restarted every sweep at created_at >= W.At OFFSET 0, refetching the
// covered prefix just to skip it in Go — each refetch burned budget,
// so rows past the budget boundary inside the tie group were never
// reached (and the strict < cleanup never removes the same-timestamp
// prefix, so it never recovered). The composite-watermark resume
// (created_at = W.At AND id > W.ID, then created_at > W.At) excludes
// the prefix in SQL.
func TestRelay_Round4WideSettledTieDoesNotStarveTail(t *testing.T) {
	wideSettledTie(t, openCore(t))
}

// wideSettledTie is the round-4 shape shared with the MySQL lane twin
// (datetime(3) makes wide same-timestamp groups the realistic path).
func wideSettledTie(t *testing.T, c *core) {
	t.Helper()
	ctx := context.Background()
	ts := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	const n = 13 // maxPages(3) × batch(4) + 1
	for i := 1; i <= n; i++ {
		insertAt(t, c, ts, 0, "t", fmt.Sprintf("m%02d", i))
	}
	// Rows after the tie group prove the tail is reachable in order.
	insertAt(t, c, ts.Add(time.Second), 0, "t", "n1")
	insertAt(t, c, ts.Add(2*time.Second), 0, "t", "n2")

	cp := &capture{}
	r := mkRelay(t, c, "wide-tie", cp, 0, 4)
	r.maxPages = 3

	if err := r.run(ctx); err != nil { // budget-bounded: first 12 of the tie
		t.Fatal(err)
	}
	if len(cp.payloads()) != 12 {
		t.Fatalf("first sweep delivered %d, want 12 (budget)", len(cp.payloads()))
	}
	if err := r.run(ctx); err != nil { // boundary resume: m13, then n1 n2
		t.Fatal(err)
	}
	want := make([]string, 0, n+2)
	for i := 1; i <= n; i++ {
		want = append(want, fmt.Sprintf("m%02d", i))
	}
	want = append(want, "n1", "n2")
	wantPayloads(t, cp.payloads(), want)

	// Steady state: nothing redelivered, watermark at the true tail.
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), want)
	w, err := c.states.load(ctx, "wide-tie")
	if err != nil || !w.ok || !w.At.Equal(ts.Add(2*time.Second)) {
		t.Fatalf("watermark = %+v, %v — want the n2 position", w, err)
	}
}

// Round-5 review: pins the ACCEPTED latency trade the docs promise. A
// same-timestamp group wider than one sweep's budget that is still
// unsettled cannot reach its tail — mem-skips burn the budget and the
// watermark cannot enter the group — until the timestamp settles;
// then the composite-keyset resume (round-4) drains the tail within a
// couple of sweeps. Total tail lag ≈ settle_window + a few
// poll_intervals, and no message is lost.
func TestRelay_Round5WideUnsettledTieTailLagsUntilSettle(t *testing.T) {
	c := openCore(t)
	ctx := context.Background()
	s := time.Now().UTC().Truncate(time.Millisecond)
	ts := s.Add(-time.Second) // recent: unsettled under a 30s window
	const n = 7               // budget is maxPages(3) × batch(2) = 6
	for i := 1; i <= n; i++ {
		insertAt(t, c, ts, 0, "t", fmt.Sprintf("m%d", i))
	}
	cp := &capture{}
	r := mkRelay(t, c, "wide-unsettled", cp, 30*time.Second, 2)
	r.maxPages = 3
	clock := s
	r.now = func() time.Time { return clock }

	// While the group is unsettled, extra sweeps re-skip the delivered
	// prefix and never reach the tail — the documented, bounded lag.
	for i := 0; i < 2; i++ {
		if err := r.run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(cp.payloads()) != 6 {
		t.Fatalf("unsettled sweeps delivered %d, want 6 (budget-bounded prefix)", len(cp.payloads()))
	}
	if w, _ := c.states.load(ctx, "wide-unsettled"); w.ok {
		t.Fatalf("watermark inside the settle window: %+v", w)
	}

	// Settle elapses: the watermark enters the group, and the boundary
	// keyset reaches the tail on the following sweep.
	clock = s.Add(time.Minute)
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7"}
	wantPayloads(t, cp.payloads(), want)
	if w, err := c.states.load(ctx, "wide-unsettled"); err != nil || !w.ok || w.ID != n {
		t.Fatalf("watermark = %+v, %v — want the tail position", w, err)
	}
}
