package parts

import (
	"context"
	"fmt"
	"time"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/scheduler"
)

// SchedulerComponent owns a *scheduler.Scheduler and wires its
// lifecycle into Registry.Start/Stop.
//
// Scheduler construction is internal — the component takes a parent
// context (defaulting to context.Background) and uses the Kernel's
// Logger. Declared Dependencies include "log" so the logger is
// guaranteed initialised when Init runs.
//
// Job registration is a user responsibility: call Scheduler().Register
// from an after_start hook or from a component that depends on
// "scheduler" and does the registration in its own Init. The component
// does NOT call Start during Init — it calls Start inside an
// after_start hook so job registrations have a chance to land first.
type SchedulerComponent struct {
	parent context.Context

	sched      *scheduler.Scheduler
	stopBudget time.Duration // max time Stop will wait for in-flight jobs
}

// NewSchedulerComponent creates a scheduler component. parent governs
// the scheduler's root context; pass context.Background (or a ctx
// derived from the app's run context). stopBudget caps Stop: if the
// context already has a deadline, that wins.
func NewSchedulerComponent(parent context.Context, stopBudget time.Duration) *SchedulerComponent {
	if parent == nil {
		parent = context.Background()
	}
	if stopBudget <= 0 {
		stopBudget = 15 * time.Second
	}
	return &SchedulerComponent{parent: parent, stopBudget: stopBudget}
}

// Name implements component.Component.
func (s *SchedulerComponent) Name() string { return "scheduler" }

// ConfigKey implements component.Component.
func (s *SchedulerComponent) ConfigKey() string { return "scheduler" }

// Dependencies implements component.Dependent.
func (s *SchedulerComponent) Dependencies() []string { return []string{"log"} }

// Init builds the scheduler but does NOT start it. Registering Jobs
// must happen before Start; the component defers Start to an
// after_start hook so other components / user code have time to
// register jobs in their own Init.
func (s *SchedulerComponent) Init(ctx context.Context, k component.Kernel) error {
	s.sched = scheduler.New(s.parent, k.Logger())
	// Start on after_start so every component's Init (including those
	// that Register jobs in their Init) has a chance to add entries first.
	k.On(component.EventAfterStart, func(_ context.Context) error {
		s.sched.Start()
		return nil
	})
	return nil
}

// Close stops the scheduler, bounded by min(ctx deadline, stopBudget).
func (s *SchedulerComponent) Close(ctx context.Context) error {
	if s.sched == nil {
		return nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, s.stopBudget)
	defer cancel()
	return s.sched.Stop(stopCtx)
}

// Health reports how many jobs have failed recently. A single failure
// degrades, not downs — the cron keeps running and operators can
// inspect Entries() for detail.
func (s *SchedulerComponent) Health(ctx context.Context) component.HealthStatus {
	if s.sched == nil {
		return component.HealthStatus{Status: component.HealthDown, Error: "scheduler not initialised"}
	}
	entries := s.sched.Entries()
	var failing int
	for _, e := range entries {
		if e.LastErr != "" {
			failing++
		}
	}
	status := component.HealthStatus{
		Status: component.HealthOK,
		Details: map[string]any{
			"jobs":    len(entries),
			"failing": failing,
		},
	}
	if failing > 0 {
		status.Status = component.HealthDegraded
	}
	return status
}

// Scheduler returns the underlying *scheduler.Scheduler for job
// registration. Safe to call after Init.
func (s *SchedulerComponent) Scheduler() *scheduler.Scheduler { return s.sched }

// MustRegister panics if the scheduler isn't initialised yet. Useful in
// after_start hooks where the component must be present.
func (s *SchedulerComponent) MustRegister(j scheduler.Job) {
	if s.sched == nil {
		panic(fmt.Sprintf("scheduler: MustRegister(%q) before Init", j.Name()))
	}
	if err := s.sched.Register(j); err != nil {
		panic(err)
	}
}
