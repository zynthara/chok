package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zynthara/chok/internal/dbtest"
)

// The relay tests are v2's outbox/relay_test.go, the five review rounds
// included, under their v2 names. The adaptations: a worker here is keyed
// by its topic, so each relay's name in v2 is the topic its messages are
// enqueued under here; the WithRelayFor escape hatch over user-owned
// tables is gone (contract §5), with its two tests; every test runs on
// every lane.

func openCore(t *testing.T) *core {
	t.Helper()
	d := dbtest.Open(t)
	if err := d.RegisterSchema("jobs", tables(), migrations()); err != nil {
		t.Fatal(err)
	}
	return newCore(d, d.Logger())
}

// enqueue commits one message. On MySQL, whose datetime(3) keeps
// milliseconds, it then waits a millisecond, so consecutive messages get
// distinct created_at values as they did on v2's SQLite lane; the tie
// scenarios write their ties explicitly with insertAt.
func enqueue(t *testing.T, c *core, topic, payload string) {
	t.Helper()
	err := c.d.RunInTx(t.Context(), func(txCtx context.Context) error {
		return c.enqueue(txCtx, topic, []byte(payload))
	})
	if err != nil {
		t.Fatalf("enqueue %s/%s: %v", topic, payload, err)
	}
	if dbtest.Driver() == "mysql" {
		time.Sleep(time.Millisecond)
	}
}

// insertAt writes a message with a chosen created_at (and optional
// explicit id) directly — the tie/late-commit scenarios need positions
// Enqueue deliberately does not expose. Times are pre-truncated to the
// millisecond so every dialect round-trips them exactly.
func insertAt(t *testing.T, c *core, at time.Time, id uint64, topic, payload string) {
	t.Helper()
	row := message{ID: id, CreatedAt: at, Topic: topic, Payload: []byte(payload)}
	if err := c.d.Unsafe(context.Background()).Create(&row).Error; err != nil {
		t.Fatalf("insertAt: %v", err)
	}
}

// capture is a worker function that records deliveries and can be told
// to fail.
type capture struct {
	mu   sync.Mutex
	got  []rawMessage
	fail func(rawMessage) error
}

func (cp *capture) handle(_ context.Context, m rawMessage) error {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.fail != nil {
		if err := cp.fail(m); err != nil {
			return err
		}
	}
	cp.got = append(cp.got, m)
	return nil
}

func (cp *capture) payloads() []string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := make([]string, len(cp.got))
	for i, m := range cp.got {
		out[i] = string(m.Payload)
	}
	return out
}

func mkRelay(t *testing.T, c *core, topic string, cp *capture, settle time.Duration, batch int) *relay {
	t.Helper()
	r, err := newRelay(topic, cp.handle, c, settle, batch)
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

func countMessages(t *testing.T, c *core) int64 {
	t.Helper()
	var n int64
	if err := c.d.Unsafe(context.Background()).Model(&message{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEnqueue_TxGateAndAtomicity(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()

	// Outside any transaction: rejected.
	if err := c.enqueue(ctx, "t", []byte("x")); !errors.Is(err, ErrOutsideTx) {
		t.Fatalf("outside tx: err = %v, want ErrOutsideTx", err)
	}
	// A transaction on ANOTHER handle: still rejected — that would not
	// be atomic with writes on the jobs' handle.
	other := dbtest.Open(t)
	err := other.RunInTx(ctx, func(txCtx context.Context) error {
		return c.enqueue(txCtx, "t", []byte("x"))
	})
	if !errors.Is(err, ErrOutsideTx) {
		t.Fatalf("foreign tx: err = %v, want ErrOutsideTx", err)
	}
	// Topic validation.
	_ = c.d.RunInTx(ctx, func(txCtx context.Context) error {
		if err := c.enqueue(txCtx, "", []byte("x")); !errors.Is(err, errBadTopic) {
			t.Fatalf("empty topic: err = %v, want errBadTopic", err)
		}
		long := strings.Repeat("a", maxTopicLen+1)
		if err := c.enqueue(txCtx, long, nil); !errors.Is(err, errBadTopic) {
			t.Fatalf("long topic: err = %v, want errBadTopic", err)
		}
		return nil
	})

	// Rollback drops the staged message with the business writes.
	sentinel := errors.New("boom")
	err = c.d.RunInTx(ctx, func(txCtx context.Context) error {
		if err := c.enqueue(txCtx, "orders", []byte("rolled-back")); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback tx: err = %v", err)
	}
	if n := countMessages(t, c); n != 0 {
		t.Fatalf("rows after rollback = %d, want 0", n)
	}

	// Commit makes it visible; enqueueJSON round-trips.
	err = c.d.RunInTx(ctx, func(txCtx context.Context) error {
		return c.enqueueJSON(txCtx, "orders", map[string]string{"id": "o_1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var rows []message
	if err := c.d.Unsafe(ctx).Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Topic != "orders" || string(rows[0].Payload) != `{"id":"o_1"}` {
		t.Fatalf("committed rows = %+v", rows)
	}
}

func TestRelay_DeliversInOrderAndAdvancesWatermark(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	for i := 1; i <= 5; i++ {
		enqueue(t, c, "orderly", fmt.Sprintf("m%d", i))
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
	enqueue(t, c, "orderly", "m6")
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"m1", "m2", "m3", "m4", "m5", "m6"})
}

func TestRelay_AtLeastOnce_FailedHandlerRetriesInOrder(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "retry", fmt.Sprintf("m%d", i))
	}
	boom := errors.New("downstream down")
	cp := &capture{fail: func(m rawMessage) error {
		if string(m.Payload) == "m2" {
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
	ctx := t.Context()
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "crashy", fmt.Sprintf("m%d", i))
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

	// "Crash": a fresh relay instance (same topic) loses mem and replays
	// the whole unsettled window — at-least-once, exactly the contract. A
	// consumer-side dedup key absorbs the duplicates.
	seen := map[string]int{}
	var mu sync.Mutex
	restarted, err := newRelay("crashy", func(_ context.Context, m rawMessage) error {
		mu.Lock()
		defer mu.Unlock()
		seen[string(m.Payload)]++
		return nil
	}, c, time.Hour, 10)
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
	ctx := t.Context()
	enqueue(t, c, "settling", "m1")
	enqueue(t, c, "settling", "m2")

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

	// Advance the clock past the settle window: the next sweep re-scans
	// (mem dedups, nothing is redelivered) and persists.
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
	ctx := t.Context()
	ts := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	const n = 25
	for i := 1; i <= n; i++ {
		insertAt(t, c, ts, 0, "ties", fmt.Sprintf("m%02d", i))
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
	ctx := t.Context()
	ts := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Second)
	// Two rows committed and swept first; ids leave a gap where the late
	// transaction's row will land.
	insertAt(t, c, ts, 10, "late", "early-a")
	insertAt(t, c, ts, 12, "late", "early-b")

	cp := &capture{}
	r := mkRelay(t, c, "late", cp, time.Hour, 10) // settle window still open
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"early-a", "early-b"})

	// The late commit becomes visible after the sweep, with an id and
	// created_at BEFORE rows already delivered (allocated earlier,
	// committed later). Inside the settle window the watermark has not
	// advanced, so the >= overlap re-scan picks it up.
	insertAt(t, c, ts, 11, "late", "late-comer")
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"early-a", "early-b", "late-comer"})
}

func TestRelay_ConcurrentEnqueue_NoLoss(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()

	// v2 ran this with settle 0, on SQLite only, over an unfiltered relay.
	// A worker here is the topic-filtered shape: a drained sweep probes the
	// settled frontier, so with settle 0 a message whose transaction
	// commits between the scan and the probe is jumped — and on Postgres
	// and MySQL a writer can also commit behind the cursor. Covering
	// commit latency is the settle window's job (Config.SettleWindow), so
	// the test gives it one on every lane.
	const settle = 2 * time.Second
	var mu sync.Mutex
	seen := map[string]int{}
	r, err := newRelay("concurrent", func(_ context.Context, m rawMessage) error {
		mu.Lock()
		defer mu.Unlock()
		seen[string(m.Payload)]++
		return nil
	}, c, settle, 7)
	if err != nil {
		t.Fatal(err)
	}

	const writers, perWriter = 4, 25
	var wg sync.WaitGroup
	for wr := range writers {
		wg.Go(func() {
			for i := range perWriter {
				err := c.d.RunInTx(ctx, func(txCtx context.Context) error {
					return c.enqueue(txCtx, "concurrent", fmt.Appendf(nil, "w%d-m%d", wr, i))
				})
				if err != nil {
					t.Errorf("enqueue: %v", err)
					return
				}
			}
		})
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
			// Every writer has committed: once the window has passed, a
			// rescan settles the lot without redelivering any of it.
			r.now = func() time.Time { return time.Now().Add(settle + time.Second) }
			if err := r.run(ctx); err != nil {
				t.Fatal(err)
			}
			if w, err := c.states.load(ctx, "concurrent"); err != nil || !w.ok || len(r.mem) != 0 {
				t.Fatalf("after the window: watermark %+v (%v), %d entries in mem, want it persisted and mem empty", w, err, len(r.mem))
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
	ctx := t.Context()
	enqueue(t, c, "a", "a1")
	enqueue(t, c, "b", "b1")
	enqueue(t, c, "a", "a2")
	enqueue(t, c, "b", "b2")

	cpA, cpB := &capture{}, &capture{}
	ra := mkRelay(t, c, "a", cpA, 0, 10)
	rb := mkRelay(t, c, "b", cpB, 0, 10)

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

	wa, _ := c.states.load(ctx, "a")
	wb, _ := c.states.load(ctx, "b")
	if !wa.ok || !wb.ok {
		t.Fatalf("watermarks not persisted: a=%+v b=%+v", wa, wb)
	}
	// Progress is per worker: replaying B must not disturb A's state.
	if err := rb.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cpB.payloads(), []string{"b1", "b2"})
}

// TestRelay_OverlappingSweepIsRefused is v2's
// TestRelay_RunNowOverlapReturnsBusy: there is no RunNow, and a worker's
// sweeps never overlap under the scheduler, but the guard on mem stays.
func TestRelay_OverlappingSweepIsRefused(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	enqueue(t, c, "busy", "m1")

	release := make(chan struct{})
	started := make(chan struct{})
	r, err := newRelay("busy", func(_ context.Context, _ rawMessage) error {
		close(started)
		<-release
		return nil
	}, c, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- r.run(ctx) }()
	<-started
	if err := r.run(ctx); !errors.Is(err, errSweepRunning) {
		t.Fatalf("overlapping run err = %v, want errSweepRunning", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestCleanup_MinWatermarkFloorAndRetention(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	for i := 1; i <= 4; i++ {
		enqueue(t, c, "sweeper", fmt.Sprintf("m%d", i))
	}
	cp := &capture{}
	r := mkRelay(t, c, "sweeper", cp, 0, 10)
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}

	// Zero registered workers: never delete.
	if n, err := c.cleanupOnce(ctx, nil, time.Nanosecond, 10); err != nil || n != 0 {
		t.Fatalf("cleanup with no workers = %d, %v", n, err)
	}
	// A registered worker without its own state row: never delete.
	if n, err := c.cleanupOnce(ctx, []string{"sweeper", "lagging"}, time.Nanosecond, 10); err != nil || n != 0 {
		t.Fatalf("cleanup with a lagging worker = %d, %v", n, err)
	}

	// One worker, fully settled: retention floor applies. Everything
	// except the watermark row itself is deletable (strict <).
	n, err := c.cleanupOnce(ctx, []string{"sweeper"}, time.Nanosecond, 2) // batch smaller than the sweep
	if err != nil {
		t.Fatal(err)
	}
	left := countMessages(t, c)
	if n == 0 || left != 4-n {
		t.Fatalf("cleanup deleted %d, rows left %d", n, left)
	}
	// The rows at the watermark position survive (strict less-than).
	if left == 0 {
		t.Fatal("cleanup must not delete the watermark row itself")
	}

	// A stale watermark row of an UNKNOWN topic (decommissioned worker,
	// old watermark) lowers the floor and blocks further cleanup —
	// documented safe direction.
	old := watermark{At: time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Millisecond), ID: 1, ok: true}
	if err := c.states.save(ctx, "stale-worker", old); err != nil {
		t.Fatal(err)
	}
	enqueue(t, c, "sweeper", "m5")
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := c.cleanupOnce(ctx, []string{"sweeper"}, time.Nanosecond, 10); err != nil || n != 0 {
		t.Fatalf("cleanup past a stale worker = %d, %v — the stale watermark must floor it", n, err)
	}

	// Retention keeps young rows even below every watermark.
	if err := c.d.Unsafe(ctx).Where("topic = ?", "stale-worker").Delete(&watermarkRow{}).Error; err != nil {
		t.Fatal(err)
	}
	if n, err := c.cleanupOnce(ctx, []string{"sweeper"}, 24*time.Hour, 10); err != nil || n != 0 {
		t.Fatalf("cleanup with long retention = %d, %v — young rows must survive", n, err)
	}
}

// Round-1 review #1: a residual state row (decommissioned worker) must
// not stand in for a registered worker that has not advanced yet — the
// old row-count guard passed with rows == registered even though the
// lagging worker's row was missing, deleting messages it never saw.
func TestCleanup_Round1ResidualStateCannotStandInForLaggingRelay(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "A", fmt.Sprintf("m%d", i))
	}
	// Worker A delivered everything; worker B (registered) has failed
	// since boot — no state row. A residual row of decommissioned X sits
	// at a high watermark, making the row COUNT match the registered
	// count.
	cpA := &capture{}
	ra := mkRelay(t, c, "A", cpA, 0, 10)
	if err := ra.run(ctx); err != nil {
		t.Fatal(err)
	}
	wa, err := c.states.load(ctx, "A")
	if err != nil || !wa.ok {
		t.Fatalf("worker A watermark = %+v, %v", wa, err)
	}
	if err := c.states.save(ctx, "X", wa); err != nil {
		t.Fatal(err)
	}

	if n, err := c.cleanupOnce(ctx, []string{"A", "B"}, time.Nanosecond, 10); err != nil || n != 0 {
		t.Fatalf("cleanup = %d, %v — X's residual row must not cover for lagging B", n, err)
	}
	if left := countMessages(t, c); left != 3 {
		t.Fatalf("rows left = %d, want all 3 kept for worker B", left)
	}
}

// Round-1 review #3: mem is pruned on every watermark advance (batch
// boundaries and failure-path saves), not only at the end of a fully
// successful sweep — otherwise a long catch-up holds the entire
// processed backlog in memory and an early error return leaks every
// settled entry for the relay's lifetime.
func TestRelay_Round1MemPrunedOnEveryAdvance(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	for i := 1; i <= 6; i++ {
		enqueue(t, c, "pruned", fmt.Sprintf("m%d", i))
	}
	boom := errors.New("poison")
	cp := &capture{fail: func(m rawMessage) error {
		if string(m.Payload) == "m5" {
			return boom
		}
		return nil
	}}
	r := mkRelay(t, c, "pruned", cp, 0, 2) // settle 0: every delivered row settles
	if err := r.run(ctx); !errors.Is(err, boom) {
		t.Fatalf("sweep err = %v, want the poison error", err)
	}
	// m1..m4 delivered and settled across two batches; the failure path
	// persisted the watermark at m4 — nothing settled may remain in mem.
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
// cutoff, so rows delivered unsettled in one sweep settle, persist and
// prune on a later sweep once wall time passes.
func TestRelay_Round2MemBoundedAcrossSweeps(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	ts := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	const n = 25
	for i := 1; i <= n; i++ {
		insertAt(t, c, ts, 0, "cross-sweep", fmt.Sprintf("m%02d", i))
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

// Round-2 review #2: run prunes mem against the loaded watermark before
// scanning. When another process (last-write-wins degradation) advances
// the shared state past rows this process delivered but never settled
// locally, those mem entries would otherwise outlive every local
// advance.
func TestRelay_Round2LoadedWatermarkPrunesForeignAdvance(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "shared", fmt.Sprintf("m%d", i))
	}
	cp := &capture{}
	r := mkRelay(t, c, "shared", cp, time.Hour, 10) // unsettled: mem only
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.mem) != 3 {
		t.Fatalf("mem after first sweep = %d, want 3 unsettled entries", len(r.mem))
	}
	// "Process B" advances the shared state past everything.
	var last message
	if err := c.d.Unsafe(ctx).Order("created_at DESC").Order("id DESC").Take(&last).Error; err != nil {
		t.Fatal(err)
	}
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

// Round-2 review #3: a worker whose topic stays quiet advances its
// watermark over the other topics' rows to the settled frontier —
// otherwise it never grows a state row and the per-topic retention guard
// (round-1 #1) blocks cleanup forever.
func TestCleanup_Round2QuietFilteredRelayDoesNotBlockRetention(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	for i := 1; i <= 3; i++ {
		enqueue(t, c, "hot", fmt.Sprintf("h%d", i))
	}
	cpHot := &capture{}
	hot := mkRelay(t, c, "hot", cpHot, 0, 10)
	if err := hot.run(ctx); err != nil {
		t.Fatal(err)
	}
	// The cold worker's topic has never appeared; its sweep delivers
	// nothing but must still claim the settled frontier.
	cpCold := &capture{}
	cold := mkRelay(t, c, "cold", cpCold, 0, 10)
	if err := cold.run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cpCold.payloads()) != 0 {
		t.Fatalf("cold worker delivered %v, want nothing", cpCold.payloads())
	}
	wc, err := c.states.load(ctx, "cold")
	if err != nil {
		t.Fatal(err)
	}
	if !wc.ok {
		t.Fatal("quiet worker has no watermark — it would block retention forever")
	}
	n, err := c.cleanupOnce(ctx, []string{"hot", "cold"}, time.Nanosecond, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("cleanup deleted nothing — the quiet worker stalled the floor")
	}

	// Correctness half: the jumped watermark must not skip topic rows
	// that arrive later.
	enqueue(t, c, "cold", "c1")
	if err := cold.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cpCold.payloads(), []string{"c1"})
}

// Round-3 review (Critical): the settle cutoff is taken once, before the
// scan. A per-batch refresh could cover a position the cursor passed
// while a perfectly legal transaction (commit within settle of its
// INSERT) was still invisible there — the row commits after the pass,
// later batches settle under the refreshed cutoff, and the watermark
// jumps the row forever. Timeline: the cursor passes R's position while
// R's transaction is open; mid-sweep R commits and the wall clock
// crosses settle; the remaining rows must NOT settle under this sweep's
// cutoff, so the next sweep's overlap rescan still finds R.
func TestRelay_Round3LateCommitDuringLongSweepNotSkipped(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	s := time.Now().UTC().Truncate(time.Millisecond)
	for i := 1; i <= 4; i++ { // ids 1-4: settled at sweep start
		insertAt(t, c, s.Add(-60*time.Second), 0, "long-sweep", fmt.Sprintf("a%d", i))
	}
	for i := range 4 { // ids 6-9: unsettled, positions after the gap
		insertAt(t, c, s.Add(-10*time.Second), uint64(6+i), "long-sweep", fmt.Sprintf("b%d", i+1))
	}
	for i := range 4 { // ids 10-13: unsettled tail
		insertAt(t, c, s.Add(-5*time.Second), uint64(10+i), "long-sweep", fmt.Sprintf("c%d", i+1))
	}

	clock := s
	var mu sync.Mutex
	var got []string
	r, err := newRelay("long-sweep", func(_ context.Context, m rawMessage) error {
		mu.Lock()
		defer mu.Unlock()
		if string(m.Payload) == "b1" {
			// The cursor has passed the (s-20s, 5) position. Now the held
			// transaction commits R there, and the sweep drags on past
			// the settle window.
			insertAt(t, c, s.Add(-20*time.Second), 5, "long-sweep", "late-R")
			clock = s.Add(40 * time.Second)
		}
		got = append(got, string(m.Payload))
		return nil
	}, c, 30*time.Second, 4)
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return clock }

	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	// The next sweep's overlap rescan must deliver R — a refreshed cutoff
	// would have settled the b/c rows and parked the watermark past R's
	// position.
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

// Round-3 review: one sweep is bounded by its page budget; the next tick
// rescans the overlap from the persisted watermark under a fresh cutoff.
// This is what reconciles the fixed pre-scan cutoff with sustained
// production (the round-2 #1 memory concern).
func TestRelay_Round3SweepBudgetBoundsOnePass(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	const n = 20
	for i := 1; i <= n; i++ {
		enqueue(t, c, "budgeted", fmt.Sprintf("m%02d", i))
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

// Round-3 review: the frontier probe reuses the sweep's pre-scan cutoff.
// A probe-time cutoff could settle a matching row that committed after
// the cursor passed its position — the jump would cover a message the
// scan never saw.
func TestRelay_Round3FrontierUsesPreScanCutoff(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	s := time.Now().UTC().Truncate(time.Millisecond)
	for i := 1; i <= 3; i++ { // other topics' rows, unsettled at sweep start
		insertAt(t, c, s.Add(-10*time.Second), 0, "hot", fmt.Sprintf("h%d", i))
	}
	cpCold := &capture{}
	cold := mkRelay(t, c, "cold", cpCold, 30*time.Second, 10)
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
	// would see the other rows as settled and claim their position).
	if err := cold.run(ctx); err != nil {
		t.Fatal(err)
	}
	if w, _ := c.states.load(ctx, "cold"); w.ok {
		t.Fatalf("frontier claimed inside the settle window: %+v", w)
	}

	// A cold message "ages in" at a position before the other rows — with
	// the premature frontier above it would have been skipped.
	insertAt(t, c, s.Add(-20*time.Second), 0, "cold", "c1")
	if err := cold.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cpCold.payloads(), []string{"c1"})
	w, err := c.states.load(ctx, "cold")
	if err != nil || !w.ok {
		t.Fatalf("frontier after settled sweep = %+v, %v", w, err)
	}
}

// lateCommitRealTx is the real-transaction shape of the round-3
// Critical: a concurrently held transaction enqueues, the relay scans
// past the invisible position, the transaction commits within its
// settle budget — the overlap rescan must still deliver it. Real
// database lanes (MySQL/Postgres) exercise true concurrent visibility;
// SQLite cannot race (one write connection: a second writer waits for
// the open transaction).
func lateCommitRealTx(t *testing.T, c *core) {
	t.Helper()
	ctx := t.Context()
	const settle = 500 * time.Millisecond
	cp := &capture{}
	r := mkRelay(t, c, "late-real", cp, settle, 10)

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.d.RunInTx(ctx, func(txCtx context.Context) error {
			if err := c.enqueue(txCtx, "late-real", []byte("late-tx")); err != nil {
				return err
			}
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	enqueue(t, c, "late-real", "visible") // later position, committed first

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
	if dbtest.Driver() == "sqlite" {
		t.Skip("SQLite has one writer: a second transaction waits for the open one, so no commit can land behind the relay's cursor")
	}
	lateCommitRealTx(t, openCore(t))
}

// Round-4 review (Critical): a settled tie group wider than one sweep's
// page budget must not starve its own tail. The old scan restarted every
// sweep at created_at >= W.At OFFSET 0, refetching the covered prefix
// just to skip it in Go — each refetch burned budget, so rows past the
// budget boundary inside the tie group were never reached (and the
// strict < cleanup never removes the same-timestamp prefix, so it never
// recovered). The composite-watermark resume (created_at = W.At AND id >
// W.ID, then created_at > W.At) excludes the prefix in SQL.
func TestRelay_Round4WideSettledTieDoesNotStarveTail(t *testing.T) {
	wideSettledTie(t, openCore(t))
}

// wideSettledTie is the round-4 shape shared with the MySQL twin
// (datetime(3) makes wide same-timestamp groups the realistic path).
func wideSettledTie(t *testing.T, c *core) {
	t.Helper()
	ctx := t.Context()
	ts := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	const n = 13 // maxPages(3) × batch(4) + 1
	for i := 1; i <= n; i++ {
		insertAt(t, c, ts, 0, "wide-tie", fmt.Sprintf("m%02d", i))
	}
	// Rows after the tie group prove the tail is reachable in order.
	insertAt(t, c, ts.Add(time.Second), 0, "wide-tie", "n1")
	insertAt(t, c, ts.Add(2*time.Second), 0, "wide-tie", "n2")

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
// watermark cannot enter the group — until the timestamp settles; then
// the composite-keyset resume (round-4) drains the tail within a couple
// of sweeps. Total tail lag ≈ settle_window + a few poll_intervals, and
// no message is lost.
func TestRelay_Round5WideUnsettledTieTailLagsUntilSettle(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	s := time.Now().UTC().Truncate(time.Millisecond)
	ts := s.Add(-time.Second) // recent: unsettled under a 30s window
	const n = 7               // budget is maxPages(3) × batch(2) = 6
	for i := 1; i <= n; i++ {
		insertAt(t, c, ts, 0, "wide-unsettled", fmt.Sprintf("m%d", i))
	}
	cp := &capture{}
	r := mkRelay(t, c, "wide-unsettled", cp, 30*time.Second, 2)
	r.maxPages = 3
	clock := s
	r.now = func() time.Time { return clock }

	// While the group is unsettled, extra sweeps re-skip the delivered
	// prefix and never reach the tail — the documented, bounded lag.
	for range 2 {
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

// TestRelay_ClockZoneDoesNotMatter: the relay compares times by instant
// whatever zone its clock reports. SQLite keeps times as text, so a
// cutoff sent in another zone than the stored UTC would compare as text
// and settle — or skip — the wrong rows; the relay sends UTC.
func TestRelay_ClockZoneDoesNotMatter(t *testing.T) {
	c := openCore(t)
	ctx := t.Context()
	enqueue(t, c, "zoned", "m1")
	enqueue(t, c, "zoned", "m2")

	cp := &capture{}
	r := mkRelay(t, c, "zoned", cp, 30*time.Second, 10)
	east := time.FixedZone("UTC+8", 8*60*60)
	base := time.Now().In(east)
	r.now = func() time.Time { return base } // rows are younger than settle

	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	wantPayloads(t, cp.payloads(), []string{"m1", "m2"})
	if w, _ := c.states.load(ctx, "zoned"); w.ok {
		t.Fatalf("watermark %+v inside the settle window: the cutoff compared by zone, not by instant", w)
	}
	r.now = func() time.Time { return base.Add(time.Minute) }
	if err := r.run(ctx); err != nil {
		t.Fatal(err)
	}
	if w, err := c.states.load(ctx, "zoned"); err != nil || !w.ok {
		t.Fatalf("watermark = %+v, %v after the window", w, err)
	}
	wantPayloads(t, cp.payloads(), []string{"m1", "m2"})
}
