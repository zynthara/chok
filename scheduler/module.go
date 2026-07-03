package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
)

// Options is the "scheduler" yaml section. Every field is restart-only
// (untagged = restart, the conservative conf default).
type Options struct {
	Enabled bool `mapstructure:"enabled" default:"true"`

	// StopBudget bounds how long shutdown waits for in-flight jobs
	// after the cron loop stops accepting triggers. Serve returns when
	// the budget elapses even if a job is still stuck — the kernel's
	// draining phase must not hang on a runaway job.
	StopBudget time.Duration `mapstructure:"stop_budget" default:"15s"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.StopBudget <= 0 {
		return fmt.Errorf("scheduler: stop_budget must be positive, got %s", o.StopBudget)
	}
	return nil
}

// Module returns the scheduler component for chok.Use.
//
// Job registration is a peer/user responsibility: components that own
// cron jobs declare Needs: {Kind: "scheduler", Optional: true} and
// register from their own Init through the role interface
//
//	sc, ok := chok.Get[interface{ Register(scheduler.Job) error }](k, "scheduler")
//
// The cron loop starts in the serve phase (kernel.Server), strictly
// after every component's Init — registrations always land before the
// first trigger, without v1's after-start hook workaround.
func Module() kernel.Component { return &Component{} }

// Component owns the application-wide *Scheduler and adapts its
// lifecycle to the kernel: build at Init, run as a kernel.Server,
// bounded in-flight wind-down before Serve returns (SPEC §3.5).
type Component struct {
	opts  Options
	sched *Scheduler
}

// Describe implements kernel.Component.
func (c *Component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "scheduler",
		ConfigKey: "scheduler",
		Options:   Options{},
		Needs: []kernel.Dep{
			{Kind: "log", Optional: true},
		},
	}
}

// Init builds the scheduler but does not start the cron loop — that
// happens in Serve, after all Inits (and hence all registrations).
func (c *Component) Init(ctx context.Context, k kernel.Kernel) error {
	if err := k.Config().Section("scheduler", &c.opts); err != nil {
		return fmt.Errorf("scheduler: decode section: %w", err)
	}
	logger, ok := k.Logger().(log.Logger)
	if !ok {
		logger = log.Empty()
	}
	c.sched = New(context.Background(), logger)
	return nil
}

// Serve implements kernel.Server: start the cron, signal readiness,
// and on shutdown stop scheduling and wait — bounded by stop_budget —
// for in-flight jobs to finish before returning. The draining phase
// waits for every Serve to return before any component Close runs, so
// job dependencies (db, redis, ...) stay alive during wind-down
// (SPEC §3.3 design guarantee).
func (c *Component) Serve(ctx context.Context, ready func()) error {
	c.sched.Start()
	ready()
	<-ctx.Done()

	// ctx is already cancelled here; the stop budget needs its own
	// deadline while keeping trace correlation (WithoutCancel, never a
	// bare Background — shutdown discipline).
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.opts.StopBudget)
	defer cancel()
	return c.sched.Stop(stopCtx)
}

// Close is a no-op: wind-down happened at the end of Serve, which the
// kernel guarantees has returned before Close runs.
func (c *Component) Close(ctx context.Context) error { return nil }

// Health reports whether the scheduler is constructed. Failing jobs do
// NOT degrade component health — a flaky cron job must not pull the
// pod out of rotation; per-run failures are logged at Error level and
// visible via Entries().
func (c *Component) Health(ctx context.Context) error {
	if c.sched == nil {
		return fmt.Errorf("scheduler: not initialised")
	}
	return nil
}

// Register adds a Job (see Module docs for the role-interface use from
// peer components). Registration is valid from Init time on; adding
// jobs to a running scheduler is supported.
func (c *Component) Register(j Job) error {
	if c.sched == nil {
		return fmt.Errorf("scheduler: Register(%q) before Init", j.Name())
	}
	return c.sched.Register(j)
}

// Scheduler exposes the underlying scheduler for operational surfaces
// (Entries, RunNow). nil before Init.
func (c *Component) Scheduler() *Scheduler { return c.sched }
