package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zynthara/chok/log"
)

// --- test helpers ---

type testJob struct {
	name   string
	spec   string
	policy Policy
	run    func(ctx context.Context) error
}

func (j *testJob) Name() string                  { return j.name }
func (j *testJob) Spec() string                  { return j.spec }
func (j *testJob) Policy() Policy                { return j.policy }
func (j *testJob) Run(ctx context.Context) error { return j.run(ctx) }

func newSched(t *testing.T) *Scheduler {
	t.Helper()
	return New(context.Background(), log.Empty())
}

// --- tests ---

func TestRegister_DuplicateName(t *testing.T) {
	s := newSched(t)
	j := &testJob{name: "j1", spec: "@every 1h", run: func(context.Context) error { return nil }}
	if err := s.Register(j); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := s.Register(j); err == nil {
		t.Fatal("duplicate Register should error")
	}
}

func TestRegister_InvalidSpec_NoStateWritten(t *testing.T) {
	s := newSched(t)
	j := &testJob{name: "bad", spec: "not-a-cron-spec", run: func(context.Context) error { return nil }}
	if err := s.Register(j); err == nil {
		t.Fatal("invalid spec should error")
	}
	if len(s.entries) != 0 {
		t.Errorf("entries should be empty after failed Register, got %d", len(s.entries))
	}
}

func TestRunNow_NotFound(t *testing.T) {
	s := newSched(t)
	if err := s.RunNow("missing"); err == nil {
		t.Fatal("RunNow on missing name should error")
	}
}

func TestRunNow_SynchronousAndStatsRecorded(t *testing.T) {
	s := newSched(t)
	var executed int32
	j := &testJob{
		name: "sync", spec: "@every 1h",
		run: func(context.Context) error {
			atomic.AddInt32(&executed, 1)
			return nil
		},
	}
	if err := s.Register(j); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.RunNow("sync"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if atomic.LoadInt32(&executed) != 1 {
		t.Errorf("job did not execute synchronously")
	}
	ents := s.Entries()
	if len(ents) != 1 || ents[0].RunCount != 1 || ents[0].FailCount != 0 {
		t.Errorf("stats wrong: %+v", ents)
	}
}

func TestCronTrigger_EveryOneSecond(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive")
	}
	s := newSched(t)
	defer func() { _ = s.Stop(context.Background()) }()

	var count int32
	j := &testJob{
		name: "tick", spec: "@every 1s",
		run: func(context.Context) error {
			atomic.AddInt32(&count, 1)
			return nil
		},
	}
	if err := s.Register(j); err != nil {
		t.Fatal(err)
	}
	s.Start()

	time.Sleep(2500 * time.Millisecond)

	if got := atomic.LoadInt32(&count); got < 2 {
		t.Errorf("want >=2 triggers in 2.5s, got %d", got)
	}
}

func TestStop_CancelsRunningJob(t *testing.T) {
	s := newSched(t)

	started := make(chan struct{})
	finished := make(chan error, 1)
	j := &testJob{
		name: "longrunner", spec: "@every 1h",
		run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			finished <- ctx.Err()
			return ctx.Err()
		},
	}
	if err := s.Register(j); err != nil {
		t.Fatal(err)
	}

	go func() { _ = s.RunNow("longrunner") }()
	<-started

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Errorf("Stop: %v", err)
	}

	select {
	case err := <-finished:
		if err == nil {
			t.Error("job ctx should be canceled")
		}
	case <-time.After(1 * time.Second):
		t.Error("job did not return after Stop")
	}
}

func TestPanic_RunNow_NotCrash_ReturnsError(t *testing.T) {
	s := newSched(t)
	j := &testJob{
		name: "boom", spec: "@every 1h",
		run: func(context.Context) error { panic("deliberate") },
	}
	if err := s.Register(j); err != nil {
		t.Fatal(err)
	}

	err := s.RunNow("boom")
	if err == nil {
		t.Fatal("panic should be converted to error")
	}
	if !strings.HasPrefix(err.Error(), "panic:") {
		t.Errorf("error should start with 'panic:', got: %v", err)
	}

	ents := s.Entries()
	if ents[0].FailCount != 1 {
		t.Errorf("panic should count toward FailCount, got %d", ents[0].FailCount)
	}
	if ents[0].LastErr == "" {
		t.Error("LastErr should be set after panic")
	}
}

func TestPanic_CronTrigger_StillRecorded(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive")
	}
	s := newSched(t)
	defer func() { _ = s.Stop(context.Background()) }()

	var triggered int32
	j := &testJob{
		name: "boom2", spec: "@every 1s",
		run: func(context.Context) error {
			atomic.AddInt32(&triggered, 1)
			panic("boom")
		},
	}
	if err := s.Register(j); err != nil {
		t.Fatal(err)
	}
	s.Start()
	time.Sleep(2500 * time.Millisecond)

	if atomic.LoadInt32(&triggered) < 1 {
		t.Fatal("cron did not fire")
	}
	ents := s.Entries()
	if ents[0].FailCount < 1 {
		t.Errorf("panic in cron path should increment FailCount, got %d", ents[0].FailCount)
	}
}

func TestPanic_SkipPolicy_TokenReturned(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive")
	}
	s := newSched(t)
	defer func() { _ = s.Stop(context.Background()) }()

	var calls int32
	j := &testJob{
		name: "skip-panic", spec: "@every 1s", policy: PolicySkipIfRunning,
		run: func(context.Context) error {
			atomic.AddInt32(&calls, 1)
			panic("oops")
		},
	}
	if err := s.Register(j); err != nil {
		t.Fatal(err)
	}
	s.Start()
	time.Sleep(3500 * time.Millisecond)

	// If token leaked after the first panic, Skip would drop all subsequent
	// triggers and calls would stay at 1.
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Errorf("skip policy token leaked after panic: only %d triggers in 3.5s", got)
	}
}

func TestErrBusy_NotCountedAsFailure(t *testing.T) {
	s := newSched(t)
	j := &testJob{
		name: "busy", spec: "@every 1h",
		run: func(context.Context) error { return ErrBusy },
	}
	if err := s.Register(j); err != nil {
		t.Fatal(err)
	}
	if err := s.RunNow("busy"); !errors.Is(err, ErrBusy) {
		t.Errorf("RunNow should return ErrBusy, got: %v", err)
	}
	ents := s.Entries()
	if ents[0].RunCount != 0 {
		t.Errorf("ErrBusy should not increment RunCount, got %d", ents[0].RunCount)
	}
	if ents[0].FailCount != 0 {
		t.Errorf("ErrBusy should not increment FailCount, got %d", ents[0].FailCount)
	}
	if ents[0].LastErr != "" {
		t.Errorf("ErrBusy should not set LastErr, got %q", ents[0].LastErr)
	}
}

func TestErrBusy_Wrapped(t *testing.T) {
	s := newSched(t)
	j := &testJob{
		name: "busy-wrapped", spec: "@every 1h",
		run: func(context.Context) error {
			return fmt.Errorf("cleanup: %w", ErrBusy)
		},
	}
	if err := s.Register(j); err != nil {
		t.Fatal(err)
	}
	if err := s.RunNow("busy-wrapped"); !errors.Is(err, ErrBusy) {
		t.Errorf("errors.Is should recognize wrapped ErrBusy, got: %v", err)
	}
	ents := s.Entries()
	if ents[0].RunCount != 0 || ents[0].FailCount != 0 {
		t.Errorf("wrapped ErrBusy should not update stats: %+v", ents[0])
	}
}

func TestPolicyDelay_QueuesSecondTrigger(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive")
	}
	s := newSched(t)
	defer func() { _ = s.Stop(context.Background()) }()

	var (
		mu      sync.Mutex
		started []time.Time
	)
	j := &testJob{
		name: "slow-delay", spec: "@every 1s", policy: PolicyDelayIfRunning,
		run: func(context.Context) error {
			mu.Lock()
			started = append(started, time.Now())
			mu.Unlock()
			time.Sleep(2 * time.Second)
			return nil
		},
	}
	if err := s.Register(j); err != nil {
		t.Fatal(err)
	}
	s.Start()
	time.Sleep(4500 * time.Millisecond)

	mu.Lock()
	n := len(started)
	mu.Unlock()

	// Delay queues rather than drops; across 4.5s with 2s-per-run we expect ≥2.
	if n < 2 {
		t.Errorf("Delay policy should queue triggers; only %d runs started in 4.5s", n)
	}
}

func TestPolicySkip_DropsOverlappingTrigger(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive")
	}
	s := newSched(t)
	defer func() { _ = s.Stop(context.Background()) }()

	var calls int32
	j := &testJob{
		name: "slow-skip", spec: "@every 1s", policy: PolicySkipIfRunning,
		run: func(context.Context) error {
			atomic.AddInt32(&calls, 1)
			time.Sleep(2500 * time.Millisecond)
			return nil
		},
	}
	if err := s.Register(j); err != nil {
		t.Fatal(err)
	}
	s.Start()
	time.Sleep(4500 * time.Millisecond)

	// First run occupies ~2.5s; overlapping triggers at t=1s, t=2s should drop.
	// Across 4.5s expect at most 2 starts (first + one after the first finishes).
	got := atomic.LoadInt32(&calls)
	if got > 2 {
		t.Errorf("Skip policy should drop concurrent triggers; got %d starts", got)
	}
	if got < 1 {
		t.Errorf("at least one start expected, got %d", got)
	}
}

func TestEntries_PopulatesNextAfterStart(t *testing.T) {
	s := newSched(t)
	defer func() { _ = s.Stop(context.Background()) }()

	j := &testJob{
		name: "x", spec: "@every 1h",
		run: func(context.Context) error { return nil },
	}
	if err := s.Register(j); err != nil {
		t.Fatal(err)
	}
	s.Start()

	ents := s.Entries()
	if len(ents) != 1 {
		t.Fatalf("want 1 entry, got %d", len(ents))
	}
	if ents[0].Next.IsZero() {
		t.Error("Next should be populated after Start")
	}
	if ents[0].AvgDurMs != 0 {
		t.Errorf("AvgDurMs should be 0 before any run, got %d", ents[0].AvgDurMs)
	}
}
