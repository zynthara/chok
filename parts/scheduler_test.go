package parts

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/scheduler"
)

// testJob is a trivial Job implementation for scheduler tests.
type testJob struct {
	name, spec string
	runs       atomic.Int64
	failNext   atomic.Bool
}

func (j *testJob) Name() string             { return j.name }
func (j *testJob) Spec() string             { return j.spec }
func (j *testJob) Policy() scheduler.Policy { return scheduler.PolicySkipIfRunning }
func (j *testJob) Run(ctx context.Context) error {
	j.runs.Add(1)
	if j.failNext.CompareAndSwap(true, false) {
		return context.Canceled
	}
	return nil
}

func TestSchedulerComponent_Init_ConstructsScheduler(t *testing.T) {
	c := NewSchedulerComponent(context.Background(), time.Second)
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if c.Scheduler() == nil {
		t.Fatal("Scheduler() should not be nil after Init")
	}
}

func TestSchedulerComponent_Close_StopsScheduler(t *testing.T) {
	c := NewSchedulerComponent(context.Background(), time.Second)
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	// Register a job.
	j := &testJob{name: "t", spec: "@every 1h"}
	if err := c.Scheduler().Register(j); err != nil {
		t.Fatal(err)
	}
	// Start would normally fire via after_start hook; trigger manually for this unit test.
	c.Scheduler().Start()

	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close should succeed, got %v", err)
	}
}

func TestSchedulerComponent_Health_DegradesOnJobFailure(t *testing.T) {
	c := NewSchedulerComponent(context.Background(), time.Second)
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	j := &testJob{name: "t", spec: "@every 1h"}
	if err := c.Scheduler().Register(j); err != nil {
		t.Fatal(err)
	}

	// Fresh scheduler: no runs yet → OK.
	if s := c.Health(context.Background()); s.Status != component.HealthOK {
		t.Fatalf("fresh scheduler should be OK, got %q", s.Status)
	}

	// Simulate a failed run by invoking RunNow with failNext set.
	j.failNext.Store(true)
	_ = c.Scheduler().RunNow("t")

	if s := c.Health(context.Background()); s.Status != component.HealthDegraded {
		t.Fatalf("after failure, scheduler should be Degraded, got %q", s.Status)
	}
	_ = c.Close(context.Background())
}

func TestSchedulerComponent_DependsOnLog(t *testing.T) {
	c := NewSchedulerComponent(context.Background(), time.Second)
	deps := c.Dependencies()
	if len(deps) != 1 || deps[0] != "log" {
		t.Fatalf("scheduler deps should be [\"log\"], got %v", deps)
	}
}

func TestSchedulerComponent_AfterStartHook_StartsScheduler(t *testing.T) {
	// This test verifies that Init registers an after_start hook.
	// With a full Registry the hook would fire during Start; here we
	// assert the registration happens against our mockKernel.
	c := NewSchedulerComponent(context.Background(), time.Second)
	k := newMockKernel(nil)
	if err := c.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	if hooks := k.hooks[component.EventAfterStart]; len(hooks) != 1 {
		t.Fatalf("expected one after_start hook, got %d", len(hooks))
	}
}
