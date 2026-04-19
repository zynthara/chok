package scheduler

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/zynthara/chok/log"
)

// Scheduler runs registered Jobs on their cron schedules.
type Scheduler struct {
	cron    *cron.Cron
	rootCtx context.Context
	cancel  context.CancelFunc
	logger  log.Logger

	mu      sync.RWMutex
	entries map[string]*entryState
}

type entryState struct {
	name    string
	spec    string
	entryID cron.EntryID
	job     Job
	// runGuard enforces the Job's Policy across every invocation path —
	// both cron-triggered runs and operator-triggered RunNow calls.
	// Buffered channel of capacity 1 acts as a policy-aware mutex:
	//
	//   PolicySkipIfRunning  — non-blocking send; if full, return ErrBusy.
	//   PolicyDelayIfRunning — blocking send; serialise executions.
	//
	// Using a shared channel avoids the prior bug where cron used
	// robfig/cron's internal mutex and RunNow bypassed it entirely.
	runGuard   chan struct{}
	lastStart  time.Time
	lastEnd    time.Time
	lastErr    string
	runCount   int64
	failCount  int64
	totalDurMs int64
}

// Entry is an immutable snapshot of a Job's runtime state, intended for
// admin APIs and diagnostics.
type Entry struct {
	Name      string    `json:"name"`
	Spec      string    `json:"spec"`
	Next      time.Time `json:"next"`
	LastStart time.Time `json:"last_start"`
	LastEnd   time.Time `json:"last_end"`
	LastErr   string    `json:"last_err"`
	RunCount  int64     `json:"run_count"`
	FailCount int64     `json:"fail_count"`
	AvgDurMs  int64     `json:"avg_dur_ms"`
}

// New constructs a Scheduler. The parent context governs job execution;
// Stop() cancels the derived root context, signalling all in-flight
// Run(ctx) invocations to wind down.
//
// Uses the cron runner's default time zone (time.Local). Containerised
// deployments with TZ=UTC will see specs like "0 0 * * *" fire at UTC
// midnight; call NewWithLocation to pin an explicit zone.
func New(parent context.Context, logger log.Logger) *Scheduler {
	return NewWithLocation(parent, logger, time.Local)
}

// NewWithLocation constructs a Scheduler with an explicit time.Location.
// All cron specs are interpreted in this zone. Typical use: a web app
// with users spread across zones picks a business zone (e.g. "America/
// New_York") so operator-facing specs line up with invoices/reports.
func NewWithLocation(parent context.Context, logger log.Logger, loc *time.Location) *Scheduler {
	if loc == nil {
		loc = time.Local
	}
	rootCtx, cancel := context.WithCancel(parent)
	c := cron.New(
		cron.WithLogger(cronLoggerAdapter{logger: logger}),
		cron.WithLocation(loc),
	)
	return &Scheduler{
		cron:    c,
		rootCtx: rootCtx,
		cancel:  cancel,
		logger:  logger,
		entries: map[string]*entryState{},
	}
}

// Register adds a Job. An invalid spec returns an error and leaves
// internal state untouched. Duplicate names are rejected.
func (s *Scheduler) Register(j Job) error {
	name := j.Name()
	spec := j.Spec()

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, dup := s.entries[name]; dup {
		return fmt.Errorf("scheduler: job %q already registered", name)
	}

	st := &entryState{
		name:     name,
		spec:     spec,
		job:      j,
		runGuard: make(chan struct{}, 1),
	}

	id, err := s.cron.AddJob(spec, s.wrapJob(st))
	if err != nil {
		return fmt.Errorf("scheduler: invalid spec %q for job %q: %w", spec, name, err)
	}
	st.entryID = id
	s.entries[name] = st
	return nil
}

// RunNow synchronously executes the named Job once, outside the cron
// schedule. It respects the Job's Policy: with PolicySkipIfRunning,
// RunNow returns ErrBusy when a concurrent run is in progress; with
// PolicyDelayIfRunning, RunNow blocks until the in-flight run completes.
// Panic recovery and statistics bookkeeping match scheduled runs.
//
// Returns the Job's error (or wrapped panic) if the run executed. When
// PolicySkipIfRunning rejects execution, returns ErrBusy (which is
// classified as "busy, not failure" and not counted in FailCount).
func (s *Scheduler) RunNow(name string) error {
	s.mu.RLock()
	st, ok := s.entries[name]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("scheduler: job %q not found", name)
	}
	return s.executeGuarded(st)
}

// Start begins the cron loop. Call after all Register calls.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop cancels the root context (signalling in-flight jobs) and waits
// for the cron to drain. Returns ctx.Err() wrapped if the shutdown
// context expires before all jobs finish.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.cancel()
	done := s.cron.Stop()
	select {
	case <-done.Done():
		return nil
	case <-ctx.Done():
		return fmt.Errorf("scheduler stop: %w", ctx.Err())
	}
}

// Entries returns a snapshot of all registered Jobs' stats.
func (s *Scheduler) Entries() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Entry, 0, len(s.entries))
	for _, st := range s.entries {
		ce := s.cron.Entry(st.entryID)
		var avg int64
		if st.runCount > 0 {
			avg = st.totalDurMs / st.runCount
		}
		out = append(out, Entry{
			Name:      st.name,
			Spec:      st.spec,
			Next:      ce.Next,
			LastStart: st.lastStart,
			LastEnd:   st.lastEnd,
			LastErr:   st.lastErr,
			RunCount:  st.runCount,
			FailCount: st.failCount,
			AvgDurMs:  avg,
		})
	}
	return out
}

// wrapJob returns a cron.Job that honours the registered Policy via the
// shared runGuard on entryState. Using our own guard (instead of
// robfig/cron's DelayIfStillRunning / SkipIfStillRunning wrappers) lets
// the RunNow path share the same mutual-exclusion mechanism — cron and
// operator-triggered runs can no longer race.
//
// cron.Recover is layered on top as defence in depth; execute() already
// has its own recover, but the outer Recover catches any goroutine-
// scheduled panic that escapes before execute's defer fires.
func (s *Scheduler) wrapJob(st *entryState) cron.Job {
	adapter := cronLoggerAdapter{logger: s.logger}
	return cron.NewChain(cron.Recover(adapter)).Then(cron.FuncJob(func() {
		_ = s.executeGuarded(st)
	}))
}

// executeGuarded acquires the per-entry runGuard according to the Job's
// Policy, then runs execute. Returns ErrBusy when PolicySkipIfRunning
// rejects admission.
func (s *Scheduler) executeGuarded(st *entryState) error {
	switch st.job.Policy() {
	case PolicySkipIfRunning:
		select {
		case st.runGuard <- struct{}{}:
			defer func() { <-st.runGuard }()
		default:
			return ErrBusy
		}
	default: // PolicyDelayIfRunning is the default
		st.runGuard <- struct{}{}
		defer func() { <-st.runGuard }()
	}
	return s.execute(st.job)
}

// execute runs a Job with panic recovery and statistics bookkeeping.
// Invoked from two paths: cron trigger (via wrapJob's chain) and RunNow.
// Panics are converted to errors, counted in FailCount, and additionally
// logged at Error level with a full stack trace (which is deliberately
// NOT copied into LastErr to keep admin-API payloads small).
// ErrBusy is treated as "refused by Job's own guard": no stats update,
// only a Debug log.
func (s *Scheduler) execute(j Job) (err error) {
	start := time.Now()
	runCtx, cancel := context.WithCancel(s.rootCtx)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
			const maxStack = 8 << 10
			stackBytes := debug.Stack()
			if len(stackBytes) > maxStack {
				stackBytes = stackBytes[:maxStack]
			}
			s.logger.ErrorContext(runCtx, "job panic recovered",
				"job", j.Name(),
				"panic", fmt.Sprintf("%v", r),
				"stack", string(stackBytes),
			)
		}

		dur := time.Since(start)

		if errors.Is(err, ErrBusy) {
			s.logger.DebugContext(runCtx, "job busy, skipped",
				"job", j.Name(),
				"duration_ms", dur.Milliseconds(),
			)
			return
		}

		s.record(j.Name(), start, dur, err)

		if err != nil {
			s.logger.WarnContext(runCtx, "job failed",
				"job", j.Name(),
				"error", err.Error(),
				"duration_ms", dur.Milliseconds(),
			)
		} else {
			s.logger.InfoContext(runCtx, "job ok",
				"job", j.Name(),
				"duration_ms", dur.Milliseconds(),
			)
		}
	}()

	return j.Run(runCtx)
}

func (s *Scheduler) record(name string, start time.Time, dur time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.entries[name]
	if st == nil {
		return
	}
	st.lastStart = start
	st.lastEnd = time.Now()
	st.runCount++
	st.totalDurMs += dur.Milliseconds()
	if err != nil {
		st.failCount++
		st.lastErr = err.Error()
	} else {
		st.lastErr = ""
	}
}

// cronLoggerAdapter bridges chok's log.Logger to cron.Logger.
type cronLoggerAdapter struct{ logger log.Logger }

func (a cronLoggerAdapter) Info(msg string, keysAndValues ...any) {
	a.logger.Info("cron: "+msg, keysAndValues...)
}

func (a cronLoggerAdapter) Error(err error, msg string, keysAndValues ...any) {
	kv := append([]any{"error", err.Error()}, keysAndValues...)
	a.logger.Error("cron: "+msg, kv...)
}
