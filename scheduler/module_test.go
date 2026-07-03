package scheduler_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/scheduler"
)

// testJob is a minimal scheduler.Job.
type testJob struct {
	name   string
	spec   string
	policy scheduler.Policy
	run    func(ctx context.Context) error
}

func (j *testJob) Name() string                  { return j.name }
func (j *testJob) Spec() string                  { return j.spec }
func (j *testJob) Policy() scheduler.Policy      { return j.policy }
func (j *testJob) Run(ctx context.Context) error { return j.run(ctx) }

// jobOwner registers a job from its own Init through the role
// interface — the documented peer pattern (audit purge uses it).
type jobOwner struct {
	job scheduler.Job
	ok  bool
}

func (o *jobOwner) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:  "jobowner",
		Needs: []kernel.Dep{{Kind: "scheduler", Optional: true}},
	}
}

func (o *jobOwner) Init(ctx context.Context, k kernel.Kernel) error {
	sc, ok := kernel.Get[interface{ Register(scheduler.Job) error }](k, "scheduler")
	if !ok {
		return nil // scheduler absent: degrade, mirroring audit's soft dependency
	}
	if err := sc.Register(o.job); err != nil {
		return err
	}
	o.ok = true
	return nil
}

func (o *jobOwner) Close(context.Context) error { return nil }

func TestModule_PeerRegistersAtInit_CronFires(t *testing.T) {
	var runs atomic.Int32
	owner := &jobOwner{job: &testJob{
		name: "tick", spec: "@every 1s",
		run: func(context.Context) error { runs.Add(1); return nil },
	}}
	choktest.NewTestKernel(t, "", scheduler.Module(), owner)

	if !owner.ok {
		t.Fatal("peer Init could not register through the role interface")
	}
	deadline := time.Now().Add(3 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if runs.Load() == 0 {
		t.Fatal("cron never fired after serve phase started")
	}
}

func TestModule_RunNowThroughAccessor(t *testing.T) {
	var runs atomic.Int32
	owner := &jobOwner{job: &testJob{
		name: "manual", spec: "@every 1h",
		run: func(context.Context) error { runs.Add(1); return nil },
	}}
	tk := choktest.NewTestKernel(t, "", scheduler.Module(), owner)

	sc, ok := kernel.Get[*scheduler.Component](tk, "scheduler")
	if !ok {
		t.Fatal("scheduler component not visible")
	}
	if err := sc.Scheduler().RunNow("manual"); err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 1 {
		t.Fatalf("RunNow executions = %d, want 1", runs.Load())
	}
}

func TestModule_StopCancelsInFlightJobWithinBudget(t *testing.T) {
	started := make(chan struct{})
	sawCancel := make(chan struct{})
	owner := &jobOwner{job: &testJob{
		name: "long", spec: "@every 1h",
		run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(sawCancel)
			return ctx.Err()
		},
	}}

	// Hand-rolled kernel (not NewTestKernel) so the test owns Stop
	// timing instead of racing the t.Cleanup.
	tk, err := choktest.StartKernel(t, "scheduler:\n  stop_budget: 5s\n", scheduler.Module(), owner)
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := kernel.Get[*scheduler.Component](tk, "scheduler")
	go func() { _ = sc.Scheduler().RunNow("long") }()
	<-started

	stopStart := time.Now()
	if err := tk.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-sawCancel:
	default:
		t.Fatal("in-flight job never observed cancellation during wind-down")
	}
	if elapsed := time.Since(stopStart); elapsed > 4*time.Second {
		t.Fatalf("Stop took %s — wind-down should be prompt once jobs observe cancel", elapsed)
	}
}

func TestModule_Disabled_NotVisible(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
scheduler:
  enabled: false
`, scheduler.Module())
	if _, ok := kernel.Get[*scheduler.Component](tk, "scheduler"); ok {
		t.Fatal("disabled scheduler must not be reachable via kernel.Get")
	}
}

func TestModule_InvalidStopBudget_FailsStartup(t *testing.T) {
	_, err := choktest.StartKernel(t, `
scheduler:
  stop_budget: -1s
`, scheduler.Module())
	if err == nil {
		t.Fatal("expected validation failure for negative stop_budget")
	}
	if !strings.Contains(err.Error(), "stop_budget") {
		t.Fatalf("error should name stop_budget, got: %v", err)
	}
}
