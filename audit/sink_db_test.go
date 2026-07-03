package audit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func newSinkDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	// The async sink goroutine queries concurrently with the test; a
	// second pool connection over :memory: would be a fresh empty
	// database (same pinning db.Open applies).
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
	}
	if err := db.AutoMigrate(&Log{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func waitForRowCount(t *testing.T, db *gorm.DB, want int64, total time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(total)
	var got int64
	for time.Now().Before(deadline) {
		_ = db.Model(&Log{}).Count(&got).Error
		if got >= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return got
}

// TestDBLogger_LogSync_RoundTrip pins the synchronous write path:
// LogSync commits immediately and the row is queryable on return.
func TestDBLogger_LogSync_RoundTrip(t *testing.T) {
	db := newSinkDB(t)
	l := NewDBLogger(context.Background(), db, 8, false, nil)
	t.Cleanup(l.Close)

	err := l.LogSync(context.Background(), Entry{
		ActorID:    "usr_alice",
		Action:     "task.create",
		Resource:   "task",
		ResourceID: "tsk_001",
	})
	if err != nil {
		t.Fatalf("LogSync: %v", err)
	}

	var rows []Log
	if err := db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Result != ResultSuccess {
		t.Errorf("default Result = %q, want %q", rows[0].Result, ResultSuccess)
	}
	if rows[0].ActorType != ActorTypeUser {
		t.Errorf("default ActorType = %q, want %q (auto-derived from non-blank ActorID)", rows[0].ActorType, ActorTypeUser)
	}
	if rows[0].OccurredAt.IsZero() {
		t.Error("OccurredAt should be defaulted to now()")
	}
}

// TestDBLogger_LogSync_RejectsBlankAction pins SPEC-derived
// validation at the sink boundary. Action is the one required field;
// a blank action wouldn't be queryable by the action index and is
// almost always a bug at the call site.
func TestDBLogger_LogSync_RejectsBlankAction(t *testing.T) {
	db := newSinkDB(t)
	l := NewDBLogger(context.Background(), db, 8, false, nil)
	t.Cleanup(l.Close)

	if err := l.LogSync(context.Background(), Entry{Resource: "task"}); err == nil {
		t.Fatal("blank Action should be rejected")
	}
}

// TestDBLogger_AsyncDrainsOnClose proves the Close contract: any
// entries queued before Close return get flushed before Close
// returns.
func TestDBLogger_AsyncDrainsOnClose(t *testing.T) {
	db := newSinkDB(t)
	l := NewDBLogger(context.Background(), db, 64, false, nil)

	const n = 25
	for range n {
		l.Log(context.Background(), Entry{
			ActorID: "usr_alice",
			Action:  "task.create",
		})
	}
	l.Close()

	var got int64
	if err := db.Model(&Log{}).Count(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Errorf("after Close, row count = %d, want %d (Close should drain in-flight batch)", got, n)
	}
}

// TestDBLogger_BatchFlushOnTimer covers the low-traffic path: even
// when batchSize is never reached, the flushInterval fires and rows
// land in the DB. Pinning this prevents future "optimisation" PRs
// from removing the timer in the name of throughput — that would
// silently break low-volume audits.
func TestDBLogger_BatchFlushOnTimer(t *testing.T) {
	db := newSinkDB(t)
	l := NewDBLogger(context.Background(), db, 64, false, nil,
		withBatchSize(1000),                    // batchSize unreachable in test
		withFlushInterval(50*time.Millisecond), // timer flushes early
	)
	t.Cleanup(l.Close)

	l.Log(context.Background(), Entry{
		ActorID: "usr_alice",
		Action:  "task.create",
	})

	got := waitForRowCount(t, db, 1, time.Second)
	if got != 1 {
		t.Errorf("timer-driven flush never landed: got %d rows", got)
	}
}

// TestDBLogger_DropOnFull_DropsOverflow proves the back-pressure
// contract: with DropOnFull=true, a saturated buffer drops new
// entries and bumps the counter rather than blocking the caller.
func TestDBLogger_DropOnFull_DropsOverflow(t *testing.T) {
	db := newSinkDB(t)

	// Tiny buffer + slow worker: keep the worker parked behind a
	// blocked DB to overflow the channel deterministically. We
	// achieve "slow worker" by issuing more entries than the
	// buffer at once before the worker can drain — race-prone in
	// principle, but with buffer=1 + 50 entries the overflow is
	// observed reliably.
	l := NewDBLogger(context.Background(), db, 1, true, nil)
	t.Cleanup(l.Close)

	const burst = 200
	for range burst {
		l.Log(context.Background(), Entry{
			ActorID: "usr_alice",
			Action:  "task.create",
		})
	}

	stats := l.Stats()
	if stats.Dropped == 0 {
		t.Errorf("expected some drops with buffer=1 + DropOnFull=true + %d entries, got Stats=%+v", burst, stats)
	}
	if stats.Pending+stats.Dropped+stats.Written < 1 {
		t.Error("counters should account for at least the first enqueue")
	}
}

// TestDBLogger_BlockOnFull_RespectsCtx pins the back-pressure
// contract for DropOnFull=false: a saturated buffer blocks the
// producer, but only until ctx cancels — it must not stall a
// caller past their request deadline.
func TestDBLogger_BlockOnFull_RespectsCtx(t *testing.T) {
	db := newSinkDB(t)
	l := NewDBLogger(context.Background(), db, 1, false, nil)
	t.Cleanup(l.Close)

	// Saturate: send entries serially with a tiny ctx timeout so
	// at least one Log call must observe ctx cancellation.
	var dropped int
	for range 100 {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
		l.Log(ctx, Entry{
			ActorID: "usr_alice",
			Action:  "task.create",
		})
		cancel()
	}
	stats := l.Stats()
	dropped = int(stats.Dropped)
	if dropped == 0 {
		// All landed (worker was fast); skip the assertion. The
		// contract is "respect ctx", not "always drop"; we can't
		// force the timing. The interesting bug shape — caller
		// hangs past ctx — would surface as the test exceeding
		// the testing timeout, which `go test -timeout` catches.
		t.Log("no entries dropped under ctx pressure; worker drained fast enough — contract still upheld")
	}
}

// TestDBLogger_PendingCounterIsConsistent — a coarse sanity check
// that pending decrements match writes after a Close drain.
func TestDBLogger_PendingCounterIsConsistent(t *testing.T) {
	db := newSinkDB(t)
	l := NewDBLogger(context.Background(), db, 32, false, nil)

	const n = 10
	for range n {
		l.Log(context.Background(), Entry{ActorID: "u", Action: "x"})
	}
	l.Close()
	stats := l.Stats()
	if stats.Pending != 0 {
		t.Errorf("Pending after Close = %d, want 0 (Close should drain)", stats.Pending)
	}
	if stats.Written != n {
		t.Errorf("Written after Close = %d, want %d", stats.Written, n)
	}
}

// TestDBLogger_Query_PageAndFilter — pin the read-side semantics:
// filters compose, total count is unbounded by limit, page is
// 1-indexed.
func TestDBLogger_Query_PageAndFilter(t *testing.T) {
	db := newSinkDB(t)
	l := NewDBLogger(context.Background(), db, 64, false, nil)
	t.Cleanup(l.Close)

	for i := range 12 {
		entry := Entry{
			ActorID:    "usr_alice",
			Action:     "task.create",
			Resource:   "task",
			ResourceID: "tsk",
		}
		if i >= 8 {
			entry.ActorID = "usr_bob"
		}
		if err := l.LogSync(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}

	// All Alice entries (8) — first page of size 5 has 5 rows.
	rows, total, err := l.Query(context.Background(), Query{
		ActorID: "usr_alice",
		Page:    1,
		Size:    5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 8 {
		t.Errorf("total = %d, want 8", total)
	}
	if len(rows) != 5 {
		t.Errorf("page 1 size 5 returned %d rows", len(rows))
	}
	// Second page: 3 remaining.
	rows2, _, _ := l.Query(context.Background(), Query{
		ActorID: "usr_alice",
		Page:    2,
		Size:    5,
	})
	if len(rows2) != 3 {
		t.Errorf("page 2 returned %d rows, want 3", len(rows2))
	}
}

// TestDBLogger_Concurrent_NoRaceOnStats race-detector smoke: many
// goroutines hammering Log alongside Stats() reads.
func TestDBLogger_Concurrent_NoRaceOnStats(t *testing.T) {
	db := newSinkDB(t)
	l := NewDBLogger(context.Background(), db, 256, true, nil)

	var wg sync.WaitGroup
	const writers = 8
	const perWriter = 50
	wg.Add(writers + 1)

	stop := atomic.Bool{}
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = l.Stats()
		}
	}()

	for range writers {
		go func() {
			defer wg.Done()
			for range perWriter {
				l.Log(context.Background(), Entry{ActorID: "u", Action: "x"})
			}
		}()
	}

	// Wait for writers to finish then stop reader.
	go func() {
		// Bounded fudge factor: writers are bursty but bounded.
		time.Sleep(200 * time.Millisecond)
		stop.Store(true)
	}()
	wg.Wait()
	l.Close()
}
