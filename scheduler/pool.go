package scheduler

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zynthara/chok/log"
)

// Task is a named unit of work submitted to a Pool.
type Task struct {
	// Name identifies the task in logs and metrics. Not required to be
	// unique — multiple tasks with the same name are allowed.
	Name string
	// Run executes the task. The context is cancelled when the Pool is
	// stopped; long-running tasks should check ctx.Done().
	Run func(ctx context.Context) error
}

// PoolOptions configures a Pool.
type PoolOptions struct {
	// Workers is the number of concurrent worker goroutines. Default 10.
	Workers int
	// QueueSize is the buffered channel capacity. When the queue is full,
	// Submit blocks until a slot opens or ctx is cancelled. Default 1000.
	QueueSize int
	// Logger receives task start/completion/error/panic events.
	Logger log.Logger
}

// PoolStats is a snapshot of pool runtime statistics.
type PoolStats struct {
	Submitted int64 `json:"submitted"`
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
	Panicked  int64 `json:"panicked"`
	Pending   int   `json:"pending"` // current queue depth
	InFlight  int64 `json:"in_flight"`
}

// ErrPoolStopped is returned by Submit when the Pool has been stopped.
var ErrPoolStopped = errors.New("pool: stopped")

// Pool is a bounded worker pool for one-off async tasks. It fills the
// gap between cron-scheduled jobs (Scheduler) and unmanaged goroutines.
//
// Submitted tasks run on a fixed set of worker goroutines. Panics are
// recovered and logged. Stop drains the queue and waits for in-flight
// tasks, integrating cleanly with the Component shutdown sequence.
type Pool struct {
	opts      PoolOptions
	queue     chan Task
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	startOnce sync.Once // ensures Start is called exactly once
	closeOnce sync.Once // ensures close(queue) is called exactly once

	submitted atomic.Int64
	completed atomic.Int64
	failed    atomic.Int64
	panicked  atomic.Int64
	inFlight  atomic.Int64
}

// NewPool creates a Pool but does not start workers. Call Start to begin
// processing. Intended to be wrapped by a PoolComponent that wires the
// lifecycle into the Registry.
func NewPool(opts PoolOptions) *Pool {
	if opts.Workers <= 0 {
		opts.Workers = 10
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 1000
	}
	if opts.Logger == nil {
		opts.Logger = log.Empty()
	}
	return &Pool{
		opts:  opts,
		queue: make(chan Task, opts.QueueSize),
	}
}

// Start launches worker goroutines. The parent context is used to derive
// a cancellable context — Stop() cancels it, signalling workers to wind
// down after draining. Start is idempotent — the second call is a no-op.
func (p *Pool) Start(parent context.Context) {
	p.startOnce.Do(func() {
		p.ctx, p.cancel = context.WithCancel(parent)
		for range p.opts.Workers {
			p.wg.Add(1)
			go p.worker()
		}
	})
}

// Submit enqueues a task for async execution. It blocks when the queue
// is full or returns an error when ctx is cancelled. Submit is safe to
// call from any goroutine. Returns ErrPoolStopped after Stop is called.
//
// Uses recover to handle the inherent TOCTOU race between Submit and
// Stop: if Stop closes the channel between our check and the send, the
// panic is caught and converted to ErrPoolStopped.
func (p *Pool) Submit(ctx context.Context, t Task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pool: submit %q: %w", t.Name, ErrPoolStopped)
		}
	}()
	select {
	case p.queue <- t:
		p.submitted.Add(1)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("pool: submit %q: %w", t.Name, ctx.Err())
	}
}

// SubmitFunc is a convenience wrapper around Submit.
func (p *Pool) SubmitFunc(ctx context.Context, name string, fn func(context.Context) error) error {
	return p.Submit(ctx, Task{Name: name, Run: fn})
}

// Stop signals workers to drain and waits for all in-flight tasks to
// complete. Stop is idempotent — the second call waits for the same
// drain without panicking. Returns ctx.Err() if the deadline is exceeded.
func (p *Pool) Stop(ctx context.Context) error {
	// Cancel worker context first so in-flight tasks can observe
	// cancellation and wind down promptly during the drain phase.
	if p.cancel != nil {
		p.cancel()
	}

	// closeOnce ensures close(p.queue) is called exactly once, even
	// if Stop is called concurrently or multiple times.
	p.closeOnce.Do(func() {
		close(p.queue)
	})

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("pool stop: %w", ctx.Err())
	}
}

// Stats returns a snapshot of pool runtime statistics.
func (p *Pool) Stats() PoolStats {
	return PoolStats{
		Submitted: p.submitted.Load(),
		Completed: p.completed.Load(),
		Failed:    p.failed.Load(),
		Panicked:  p.panicked.Load(),
		Pending:   len(p.queue),
		InFlight:  p.inFlight.Load(),
	}
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for task := range p.queue {
		p.execute(task)
	}
}

func (p *Pool) execute(t Task) {
	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)

	start := time.Now()
	var err error

	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
				p.panicked.Add(1)
				const maxStack = 8 << 10
				stackBytes := debug.Stack()
				if len(stackBytes) > maxStack {
					stackBytes = stackBytes[:maxStack]
				}
				p.opts.Logger.Error("pool task panic",
					"task", t.Name,
					"panic", fmt.Sprintf("%v", r),
					"stack", string(stackBytes),
				)
			}
		}()
		err = t.Run(p.ctx)
	}()

	dur := time.Since(start)
	if err != nil {
		p.failed.Add(1)
		p.opts.Logger.Warn("pool task failed",
			"task", t.Name,
			"error", err.Error(),
			"duration_ms", dur.Milliseconds(),
		)
	} else {
		p.completed.Add(1)
		p.opts.Logger.Debug("pool task ok",
			"task", t.Name,
			"duration_ms", dur.Milliseconds(),
		)
	}
}
