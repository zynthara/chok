package jobs

import (
	"context"
	"errors"
	"log/slog"
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
	return newEntry(name, spec, schedule, overlap, fn, "job", name)
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
