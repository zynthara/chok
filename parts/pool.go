package parts

import (
	"context"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/scheduler"
)

// PoolComponent wraps a scheduler.Pool as a Component so its lifecycle
// (start workers / drain on shutdown) is managed by the Registry.
//
// Typical usage:
//
//	pc := parts.NewPoolComponent(scheduler.PoolOptions{Workers: 5})
//	app.Register(pc)
//	// later, inside a handler:
//	pc.Pool().SubmitFunc(ctx, "send-email", sendWelcomeEmail)
//
// The pool's worker context is rooted in a long-lived parent (defaults
// to context.Background). The Init ctx must NOT be used as parent —
// Registry cancels Init ctx after Init returns, which would immediately
// kill all worker tasks.
type PoolComponent struct {
	parent context.Context
	opts   scheduler.PoolOptions
	pool   *scheduler.Pool
}

// NewPoolComponent constructs the component. The pool is not created
// until Init so the Kernel logger can be injected. The pool's worker
// context is rooted in context.Background; use NewPoolComponentWithParent
// to supply a specific parent.
func NewPoolComponent(opts scheduler.PoolOptions) *PoolComponent {
	return &PoolComponent{parent: context.Background(), opts: opts}
}

// NewPoolComponentWithParent constructs a pool component whose workers
// are rooted in the given parent context (e.g. the app's run context).
// nil parent is treated as context.Background.
func NewPoolComponentWithParent(parent context.Context, opts scheduler.PoolOptions) *PoolComponent {
	if parent == nil {
		parent = context.Background()
	}
	return &PoolComponent{parent: parent, opts: opts}
}

// Name implements component.Component.
func (p *PoolComponent) Name() string { return "pool" }

// ConfigKey implements component.ConfigKeyer.
func (p *PoolComponent) ConfigKey() string { return "pool" }

// Init creates the pool, injects the Kernel logger, and starts workers.
// The worker context is rooted in p.parent (long-lived), NOT the Init
// ctx — otherwise cancellation after Init would kill tasks.
func (p *PoolComponent) Init(_ context.Context, k component.Kernel) error {
	opts := p.opts
	if opts.Logger == nil {
		opts.Logger = k.Logger().With("component", "pool")
	}
	p.pool = scheduler.NewPool(opts)
	p.pool.Start(p.parent)
	k.Logger().Info("worker pool started",
		"workers", opts.Workers,
		"queue_size", opts.QueueSize,
	)
	return nil
}

// Close drains the queue and waits for in-flight tasks.
func (p *PoolComponent) Close(ctx context.Context) error {
	if p.pool == nil {
		return nil
	}
	return p.pool.Stop(ctx)
}

// Health reports pool statistics. A pool with panics is degraded; the
// pool itself is always "up" as long as it's running.
func (p *PoolComponent) Health(ctx context.Context) component.HealthStatus {
	if p.pool == nil {
		return component.HealthStatus{Status: component.HealthOK}
	}
	stats := p.pool.Stats()
	status := component.HealthOK
	if stats.Panicked > 0 {
		status = component.HealthDegraded
	}
	return component.HealthStatus{
		Status: status,
		Details: map[string]any{
			"submitted": stats.Submitted,
			"completed": stats.Completed,
			"failed":    stats.Failed,
			"panicked":  stats.Panicked,
			"pending":   stats.Pending,
			"in_flight": stats.InFlight,
		},
	}
}

// Pool returns the underlying *scheduler.Pool for task submission.
// nil before Init.
func (p *PoolComponent) Pool() *scheduler.Pool { return p.pool }
