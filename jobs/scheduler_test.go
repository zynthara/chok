package jobs

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The scheduler tests are v2's scheduler/scheduler_test.go on the v3
// scheduler, in a synctest bubble: the clock is fake, so a run that
// "takes 2.5s" takes no time and every count is exact. v2 needed RunNow
// to run a job by hand and sleeps of real seconds; here the schedule
// itself fires.

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func periodicEntry(t *testing.T, name, spec string, overlap Overlap, fn func(context.Context) error) *entry {
	t.Helper()
	schedule, err := specParser.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	return newEntry(typePeriodic, name, spec, schedule, overlap, fn)
}

// startScheduler runs entries until the test ends.
func startScheduler(t *testing.T, entries ...*entry) *scheduler {
	t.Helper()
	s := &scheduler{logger: testLogger(t), entries: entries}
	ctx, cancel := context.WithCancel(t.Context())
	s.start(ctx)
	t.Cleanup(func() {
		cancel()
		if err := s.wait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s
}

// startTimes records when each run starts, as an offset from t0.
type startTimes struct {
	mu   sync.Mutex
	t0   time.Time
	list []time.Duration
}

func (s *startTimes) add() {
	s.mu.Lock()
	s.list = append(s.list, time.Since(s.t0))
	s.mu.Unlock()
}

func (s *startTimes) get() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.list...)
}

func wantStarts(t *testing.T, got []time.Duration, want ...time.Duration) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("runs started at %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("runs started at %v, want %v", got, want)
		}
	}
}

// TestPeriodic_FiresOnItsSchedule is v2's TestCronTrigger_EveryOneSecond,
// over the spec forms Periodic takes: @every, six fields with seconds,
// five fields, a descriptor and a zone prefix.
func TestPeriodic_FiresOnItsSchedule(t *testing.T) {
	for _, c := range []struct {
		spec  string
		after time.Duration
		want  int32
	}{
		{"@every 1s", 2500 * time.Millisecond, 2},
		{"* * * * * *", 2500 * time.Millisecond, 2},
		{"*/30 * * * * *", 95 * time.Second, 3},
		{"CRON_TZ=UTC */15 * * * *", 50 * time.Minute, 3},
		{"CRON_TZ=UTC @hourly", 150 * time.Minute, 2},
		{"CRON_TZ=UTC 30 2 * * *", 50 * time.Hour, 2},
	} {
		t.Run(c.spec, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var count atomic.Int32
				startScheduler(t, periodicEntry(t, "tick", c.spec, OverlapSkip, func(context.Context) error {
					count.Add(1)
					return nil
				}))
				time.Sleep(c.after) // the bubble starts at midnight UTC
				synctest.Wait()
				if got := count.Load(); got != c.want {
					t.Fatalf("%q fired %d times in %s, want %d", c.spec, got, c.after, c.want)
				}
			})
		})
	}
}

// TestPeriodic_RunRecordsStats is v2's TestRunNow_SynchronousAndStatsRecorded:
// a run counts, its error is the last error until a run succeeds.
func TestPeriodic_RunRecordsStats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t0 := time.Now()
		var calls atomic.Int32
		e := periodicEntry(t, "stats", "@every 1h", OverlapSkip, func(context.Context) error {
			n := calls.Add(1)
			time.Sleep(10 * time.Millisecond)
			if n == 2 {
				return errors.New("boom")
			}
			return nil
		})
		startScheduler(t, e)

		time.Sleep(time.Hour + time.Second)
		st := e.snapshot()
		if st.Runs != 1 || st.Failures != 0 || st.LastError != "" || st.AvgDuration != 10*time.Millisecond {
			t.Fatalf("after one run: %+v", st)
		}
		if !st.LastStart.Equal(t0.Add(time.Hour)) || !st.LastEnd.Equal(t0.Add(time.Hour+10*time.Millisecond)) {
			t.Fatalf("last run %v..%v, want it at 1h for 10ms", st.LastStart.Sub(t0), st.LastEnd.Sub(t0))
		}

		time.Sleep(time.Hour)
		if st := e.snapshot(); st.Runs != 2 || st.Failures != 1 || st.LastError != "boom" {
			t.Fatalf("after a failed run: %+v", st)
		}
		time.Sleep(time.Hour)
		if st := e.snapshot(); st.Runs != 3 || st.Failures != 1 || st.LastError != "" {
			t.Fatalf("a successful run clears the last error: %+v", st)
		}
	})
}

// TestPeriodic_StopCancelsRunningJob is v2's TestStop_CancelsRunningJob:
// stopping ends the context of the run in flight, and the wait returns
// once it has.
func TestPeriodic_StopCancelsRunningJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		var sawErr error
		e := periodicEntry(t, "longrunner", "@every 1h", OverlapSkip, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			sawErr = ctx.Err()
			return ctx.Err()
		})
		s := &scheduler{logger: testLogger(t), entries: []*entry{e}}
		ctx, cancel := context.WithCancel(t.Context())
		s.start(ctx)
		<-started
		cancel()
		if err := s.wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(sawErr, context.Canceled) {
			t.Fatalf("the run saw %v, want its context canceled", sawErr)
		}
		// A run told to stop is not a failed run.
		if st := e.snapshot(); st.Runs != 1 || st.Failures != 0 || !st.Next.IsZero() {
			t.Fatalf("after the stop: %+v", st)
		}
	})
}

// TestPeriodic_PanicIsRecordedAsFailure is v2's
// TestPanic_RunNow_NotCrash_ReturnsError and TestPanic_CronTrigger_StillRecorded:
// a panic is recovered, counted as a failure with a "panic:" error, and
// the schedule goes on.
func TestPeriodic_PanicIsRecordedAsFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := periodicEntry(t, "boom", "@every 1s", OverlapSkip, func(context.Context) error { panic("deliberate") })
		startScheduler(t, e)
		time.Sleep(1500 * time.Millisecond)
		st := e.snapshot()
		if st.Runs != 1 || st.Failures != 1 || !strings.HasPrefix(st.LastError, "panic: deliberate") {
			t.Fatalf("after a panic: %+v", st)
		}
		time.Sleep(time.Second)
		if st := e.snapshot(); st.Runs != 2 || st.Failures != 2 {
			t.Fatalf("the schedule stopped after a panic: %+v", st)
		}
	})
}

// TestOverlapSkip_PanicReleasesTheSlot is v2's TestPanic_SkipPolicy_TokenReturned:
// a panicking run must free its slot, or Skip would drop every trigger
// after the first.
func TestOverlapSkip_PanicReleasesTheSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		startScheduler(t, periodicEntry(t, "skip-panic", "@every 1s", OverlapSkip, func(context.Context) error {
			calls.Add(1)
			panic("oops")
		}))
		time.Sleep(3500 * time.Millisecond)
		if got := calls.Load(); got != 3 {
			t.Fatalf("%d runs in 3.5s, want 3: the slot leaked after a panic", got)
		}
	})
}

// TestOverlapSkip_DropsOverlappingTriggers is v2's TestPolicySkip_DropsOverlappingTrigger:
// a run of 2.5s on a 1s schedule takes the triggers at 1s and 4s; those
// at 2s and 3s are dropped.
func TestOverlapSkip_DropsOverlappingTriggers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		starts := &startTimes{t0: time.Now()}
		e := periodicEntry(t, "slow-skip", "@every 1s", OverlapSkip, func(context.Context) error {
			starts.add()
			time.Sleep(2500 * time.Millisecond)
			return nil
		})
		startScheduler(t, e)
		time.Sleep(4500 * time.Millisecond)
		wantStarts(t, starts.get(), time.Second, 4*time.Second)
		if st := e.snapshot(); st.Skipped != 2 {
			t.Fatalf("skipped %d triggers, want 2", st.Skipped)
		}
	})
}

// TestOverlapQueue_RunsTheOverlappingTriggerAfter is v2's
// TestPolicyDelay_QueuesSecondTrigger: the trigger at 2s waits for the run
// started at 1s and runs the moment it ends.
func TestOverlapQueue_RunsTheOverlappingTriggerAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		starts := &startTimes{t0: time.Now()}
		startScheduler(t, periodicEntry(t, "slow-queue", "@every 1s", OverlapQueue, func(context.Context) error {
			starts.add()
			time.Sleep(2 * time.Second)
			return nil
		}))
		time.Sleep(4500 * time.Millisecond)
		wantStarts(t, starts.get(), time.Second, 3*time.Second)
	})
}

// TestOverlapQueue_FoldsExtraTriggers: one run waits behind the current
// one, however many triggers fire meanwhile — v2 let blocked triggers
// pile up without bound when a job was slower than its schedule.
func TestOverlapQueue_FoldsExtraTriggers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		starts := &startTimes{t0: time.Now()}
		e := periodicEntry(t, "slower-queue", "@every 1s", OverlapQueue, func(context.Context) error {
			starts.add()
			time.Sleep(3500 * time.Millisecond)
			return nil
		})
		startScheduler(t, e)
		time.Sleep(4600 * time.Millisecond)
		// Triggers at 2s, 3s and 4s fire during the first run: the first
		// queues, the other two fold into it.
		wantStarts(t, starts.get(), time.Second, 4500*time.Millisecond)
		if st := e.snapshot(); st.Skipped != 2 {
			t.Fatalf("folded %d triggers, want 2", st.Skipped)
		}
	})
}

// TestOverlapAllow_RunsConcurrently: every trigger starts a run, beside
// the ones still going.
func TestOverlapAllow_RunsConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		starts := &startTimes{t0: time.Now()}
		var running, peak atomic.Int32
		e := periodicEntry(t, "allow", "@every 1s", OverlapAllow, func(context.Context) error {
			starts.add()
			n := running.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(2500 * time.Millisecond)
			running.Add(-1)
			return nil
		})
		startScheduler(t, e)
		time.Sleep(4500 * time.Millisecond)
		wantStarts(t, starts.get(), time.Second, 2*time.Second, 3*time.Second, 4*time.Second)
		if p := peak.Load(); p != 3 {
			t.Fatalf("at most %d runs at once, want 3", p)
		}
		if st := e.snapshot(); st.Skipped != 0 {
			t.Fatalf("skipped %d triggers, want none", st.Skipped)
		}
	})
}

// TestStats_NextAfterStart is v2's TestEntries_PopulatesNextAfterStart:
// Next is the coming activation while the scheduler runs, zero before and
// after; the average duration is 0 before any run.
func TestStats_NextAfterStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t0 := time.Now()
		e := periodicEntry(t, "x", "@every 1h", OverlapSkip, func(context.Context) error { return nil })
		if st := e.snapshot(); !st.Next.IsZero() || st.Name != "x" || st.Spec != "@every 1h" {
			t.Fatalf("before start: %+v", st)
		}
		s := &scheduler{logger: testLogger(t), entries: []*entry{e}}
		ctx, cancel := context.WithCancel(t.Context())
		s.start(ctx)
		synctest.Wait()
		st := e.snapshot()
		if !st.Next.Equal(t0.Add(time.Hour)) {
			t.Fatalf("next = %v after start, want in 1h", st.Next.Sub(t0))
		}
		if st.AvgDuration != 0 || st.Runs != 0 {
			t.Fatalf("before any run: %+v", st)
		}
		cancel()
		if err := s.wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if st := e.snapshot(); !st.Next.IsZero() {
			t.Fatalf("next = %v after the stop, want zero", st.Next)
		}
	})
}

// TestPeriodic_UnsatisfiableSpecNeverFires: a spec without a next
// activation (the 30th of February) runs nothing and ends its loop.
func TestPeriodic_UnsatisfiableSpecNeverFires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		e := periodicEntry(t, "never", "0 0 30 2 *", OverlapSkip, func(context.Context) error {
			calls.Add(1)
			return nil
		})
		s := startScheduler(t, e)
		time.Sleep(24 * time.Hour)
		if calls.Load() != 0 || !e.snapshot().Next.IsZero() {
			t.Fatalf("the 30th of February fired: %d runs, %+v", calls.Load(), e.snapshot())
		}
		if err := s.wait(t.Context()); err != nil { // its loop has ended
			t.Fatal(err)
		}
	})
}

// observed collects the events an Observe function receives.
type observed struct {
	mu     sync.Mutex
	events []Event
}

func (o *observed) observe(ev Event) {
	o.mu.Lock()
	o.events = append(o.events, ev)
	o.mu.Unlock()
}

func (o *observed) get() []Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Event(nil), o.events...)
}

// TestObserve_ReportsStartsEndsAndFailures: every run of a periodic job is
// a run.started and then a run.finished or run.failed — an error and a
// panic both fail — with its start in UTC, its duration, its error and
// the job's next trigger after the event.
func TestObserve_ReportsStartsEndsAndFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t0 := time.Now().UTC()
		var calls atomic.Int32
		e := periodicEntry(t, "report", "@every 1s", OverlapSkip, func(context.Context) error {
			time.Sleep(100 * time.Millisecond)
			switch calls.Add(1) {
			case 2:
				return errors.New("boom")
			case 3:
				panic("deliberate")
			}
			return nil
		})
		o := &observed{}
		s := &scheduler{logger: testLogger(t), entries: []*entry{e}, observers: []func(Event){o.observe}}
		ctx, cancel := context.WithCancel(t.Context())
		s.start(ctx)
		time.Sleep(3500 * time.Millisecond)
		cancel()
		if err := s.wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		at := func(d time.Duration) time.Time { return t0.Add(d) }
		const run = 100 * time.Millisecond
		want := []Event{
			{Kind: "run.started", Job: "report", Type: "periodic", StartedAt: at(time.Second), Next: at(2 * time.Second)},
			{Kind: "run.finished", Job: "report", Type: "periodic", StartedAt: at(time.Second), Duration: run, Next: at(2 * time.Second)},
			{Kind: "run.started", Job: "report", Type: "periodic", StartedAt: at(2 * time.Second), Next: at(3 * time.Second)},
			{Kind: "run.failed", Job: "report", Type: "periodic", StartedAt: at(2 * time.Second), Duration: run, Error: "boom", Next: at(3 * time.Second)},
			{Kind: "run.started", Job: "report", Type: "periodic", StartedAt: at(3 * time.Second), Next: at(4 * time.Second)},
			{Kind: "run.failed", Job: "report", Type: "periodic", StartedAt: at(3 * time.Second), Duration: run, Error: "panic: deliberate", Next: at(4 * time.Second)},
		}
		got := o.get()
		if len(got) != len(want) {
			t.Fatalf("events:\n got %+v\nwant %+v", got, want)
		}
		for i := range want {
			g, w := got[i], want[i]
			if g.Kind != w.Kind || g.Job != w.Job || g.Type != w.Type || !g.StartedAt.Equal(w.StartedAt) ||
				g.Duration != w.Duration || g.Error != w.Error || !g.Next.Equal(w.Next) || g.Messages != 0 {
				t.Fatalf("event %d:\n got %+v\nwant %+v", i, g, w)
			}
			if g.StartedAt.Location() != time.UTC || g.Next.Location() != time.UTC {
				t.Fatalf("event %d is not in UTC: %+v", i, g)
			}
		}
		// The statistics are those of the same three runs.
		if st := e.snapshot(); st.Runs != 3 || st.Failures != 2 {
			t.Fatalf("stats %+v", st)
		}
	})
}

// TestObserve_ReportsSkippedTriggers: a trigger dropped under OverlapSkip,
// or folded into the queued run under OverlapQueue, is a run.skipped
// stamped with the moment it fired; the trigger that queues is not.
func TestObserve_ReportsSkippedTriggers(t *testing.T) {
	for _, c := range []struct {
		overlap Overlap
		run     time.Duration
		skipped []time.Duration
	}{
		{OverlapSkip, 2500 * time.Millisecond, []time.Duration{2 * time.Second, 3 * time.Second}},
		// Triggers at 2s, 3s and 4s fire during the first run: 2s queues,
		// 3s and 4s fold into it.
		{OverlapQueue, 3500 * time.Millisecond, []time.Duration{3 * time.Second, 4 * time.Second}},
	} {
		t.Run(c.overlap.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				t0 := time.Now().UTC()
				e := periodicEntry(t, "slow", "@every 1s", c.overlap, func(context.Context) error {
					time.Sleep(c.run)
					return nil
				})
				o := &observed{}
				s := &scheduler{logger: testLogger(t), entries: []*entry{e}, observers: []func(Event){o.observe}}
				ctx, cancel := context.WithCancel(t.Context())
				s.start(ctx)
				time.Sleep(4600 * time.Millisecond)
				cancel()
				if err := s.wait(t.Context()); err != nil {
					t.Fatal(err)
				}
				var skipped []time.Duration
				for _, ev := range o.get() {
					if ev.Kind == "run.skipped" {
						if ev.Job != "slow" || ev.Type != "periodic" || ev.Duration != 0 || !ev.Next.Equal(ev.StartedAt.Add(time.Second)) {
							t.Fatalf("skipped event %+v", ev)
						}
						skipped = append(skipped, ev.StartedAt.Sub(t0))
					}
				}
				if !slices.Equal(skipped, c.skipped) {
					t.Fatalf("skipped triggers at %v, want %v", skipped, c.skipped)
				}
				if st := e.snapshot(); st.Skipped != int64(len(c.skipped)) {
					t.Fatalf("stats count %d skipped, the events %d", st.Skipped, len(c.skipped))
				}
			})
		})
	}
}

// TestObserve_PanicIsIsolated: an Observe function that panics is logged
// at Warn and changes nothing else — the run goes on and counts as it
// would, and the functions given after it still get every event, in the
// order the functions were given.
func TestObserve_PanicIsIsolated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs syncBuffer
		var order []string
		var mu sync.Mutex
		record := func(name string) func(Event) {
			return func(ev Event) {
				mu.Lock()
				order = append(order, name+" "+ev.Kind)
				mu.Unlock()
			}
		}
		var ran atomic.Int32
		e := periodicEntry(t, "steady", "@every 1s", OverlapSkip, func(context.Context) error {
			ran.Add(1)
			return nil
		})
		s := &scheduler{
			logger:  slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
			entries: []*entry{e},
			observers: []func(Event){
				record("first"),
				func(Event) { panic("observer bug") },
				record("third"),
			},
		}
		ctx, cancel := context.WithCancel(t.Context())
		s.start(ctx)
		time.Sleep(2500 * time.Millisecond)
		cancel()
		if err := s.wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if ran.Load() != 2 {
			t.Fatalf("the job ran %d times, want 2", ran.Load())
		}
		if st := e.snapshot(); st.Runs != 2 || st.Failures != 0 || st.LastError != "" {
			t.Fatalf("the panics leaked into the stats: %+v", st)
		}
		want := []string{
			"first run.started", "third run.started", "first run.finished", "third run.finished",
			"first run.started", "third run.started", "first run.finished", "third run.finished",
		}
		mu.Lock()
		got := slices.Clone(order)
		mu.Unlock()
		if !slices.Equal(got, want) {
			t.Fatalf("observer calls %v, want %v", got, want)
		}
		if out := logs.String(); strings.Count(out, "jobs: an Observe function panicked") != 4 || !strings.Contains(out, "observer bug") {
			t.Fatalf("log:\n%s", out)
		}
	})
}

// syncBuffer is a bytes.Buffer safe for concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
