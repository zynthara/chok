package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/zynthara/chok/v3/jobs/internal/cron"
)

// specParser reads a Periodic spec: five fields, or six with leading
// seconds, or a descriptor such as @daily or "@every 10m".
var specParser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// every activates a fixed interval after the previous activation, for any
// duration (cron's @every rounds to whole seconds): the worker sweeps and
// the cleanup.
type every time.Duration

func (e every) Next(t time.Time) time.Time { return t.Add(time.Duration(e)) }

// entry is one scheduled function — a Periodic job, a worker's sweep, or
// the retention cleanup — with its overlap state and its statistics.
type entry struct {
	name     string
	spec     string
	schedule cron.Schedule
	overlap  Overlap
	fn       func(ctx context.Context) error
	attrs    []any // what the log lines name it by
	quiet    bool  // successful runs log at Debug, not Info

	mu      sync.Mutex
	busy    bool // a run is in flight (OverlapSkip and OverlapQueue)
	queued  bool // OverlapQueue: run once more when the current run ends
	running int  // runs in flight
	total   time.Duration
	stats   JobStats
}

func newEntry(name, spec string, schedule cron.Schedule, overlap Overlap, fn func(ctx context.Context) error, attrs ...any) *entry {
	return &entry{
		name: name, spec: spec, schedule: schedule, overlap: overlap, fn: fn,
		attrs: slices.Clip(attrs), // appending to it allocates: runs may log concurrently
		stats: JobStats{Name: name, Spec: spec},
	}
}

func (e *entry) setNext(t time.Time) {
	e.mu.Lock()
	e.stats.Next = t
	e.mu.Unlock()
}

// snapshot returns the entry's statistics.
func (e *entry) snapshot() JobStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.stats
	if st.Runs > 0 {
		st.AvgDuration = e.total / time.Duration(st.Runs)
	}
	return st
}

// scheduler runs entries on their schedules: one loop per entry waits
// for the next activation and triggers a run, honoring the entry's
// Overlap; every run gets its own goroutine and a context that ends with
// the scheduler's. It is v2's scheduler with robfig/cron's runner replaced
// by those loops.
type scheduler struct {
	logger  *slog.Logger
	entries []*entry
	wg      sync.WaitGroup // the loops and the runs
}

// start launches the loops; everything stops when ctx is done.
func (s *scheduler) start(ctx context.Context) {
	for _, e := range s.entries {
		s.wg.Go(func() { s.loop(ctx, e) })
	}
}

// wait returns when every loop and run has returned, or with ctx's error.
func (s *scheduler) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// inFlight names the entries with a run in flight.
func (s *scheduler) inFlight() []string {
	var names []string
	for _, e := range s.entries {
		e.mu.Lock()
		if e.running > 0 {
			names = append(names, e.name)
		}
		e.mu.Unlock()
	}
	slices.Sort(names)
	return names
}

func (s *scheduler) loop(ctx context.Context, e *entry) {
	defer e.setNext(time.Time{})
	for {
		now := time.Now()
		next := e.schedule.Next(now)
		if next.IsZero() {
			s.logger.WarnContext(ctx, "jobs: the schedule has no next activation; the job will not run", append(e.attrs, "spec", e.spec)...)
			return
		}
		e.setNext(next)
		timer := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.trigger(ctx, e)
	}
}

// trigger starts a run unless the entry's Overlap says otherwise: Skip
// drops a trigger while a run is in flight; Queue keeps one to run when
// the current run ends, folding further ones into it; Allow always runs.
func (s *scheduler) trigger(ctx context.Context, e *entry) {
	e.mu.Lock()
	switch e.overlap {
	case OverlapAllow:
	case OverlapQueue:
		if e.busy {
			if e.queued {
				e.stats.Skipped++
			}
			e.queued = true
			e.mu.Unlock()
			return
		}
		e.busy = true
	default: // OverlapSkip
		if e.busy {
			e.stats.Skipped++
			e.mu.Unlock()
			s.logger.DebugContext(ctx, "jobs: a run is still in flight; trigger skipped", e.attrs...)
			return
		}
		e.busy = true
	}
	e.mu.Unlock()
	s.wg.Go(func() { s.runs(ctx, e) })
}

// runs runs e, and once more while a trigger is queued behind the run.
func (s *scheduler) runs(ctx context.Context, e *entry) {
	for {
		s.execute(ctx, e)
		e.mu.Lock()
		if e.overlap == OverlapAllow {
			e.mu.Unlock()
			return
		}
		if e.queued && ctx.Err() == nil {
			e.queued = false
			e.mu.Unlock()
			continue
		}
		e.busy, e.queued = false, false
		e.mu.Unlock()
		return
	}
}

// execute runs e once with panic recovery and statistics. A panic is a
// failed run: its stack goes to the log at Error (not into LastError, to
// keep Stats small). A run that ends with its context's error after the
// scheduler stopped is not a failure: it was told to stop.
func (s *scheduler) execute(ctx context.Context, e *entry) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.mu.Lock()
	e.running++
	e.mu.Unlock()
	start := time.Now()
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v", p)
				const maxStack = 8 << 10
				stack := debug.Stack()
				if len(stack) > maxStack {
					stack = stack[:maxStack]
				}
				s.logger.ErrorContext(runCtx, "jobs: a run panicked", append(e.attrs, "panic", fmt.Sprint(p), "stack", string(stack))...)
			}
		}()
		err = e.fn(runCtx)
	}()
	dur := time.Since(start)
	stopped := err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err())
	if stopped {
		err = nil
	}

	e.mu.Lock()
	e.running--
	e.stats.LastStart, e.stats.LastEnd = start, start.Add(dur)
	e.stats.Runs++
	e.total += dur
	if err != nil {
		e.stats.Failures++
		e.stats.LastError = err.Error()
	} else {
		e.stats.LastError = ""
	}
	e.mu.Unlock()

	attrs := append(e.attrs, "duration", dur)
	switch {
	case err != nil:
		s.logger.WarnContext(ctx, "jobs: run failed", append(attrs, "error", err.Error())...)
	case stopped:
		s.logger.DebugContext(ctx, "jobs: run stopped by shutdown", attrs...)
	case e.quiet:
		s.logger.DebugContext(ctx, "jobs: run ok", attrs...)
	default:
		s.logger.InfoContext(ctx, "jobs: run ok", attrs...)
	}
}
