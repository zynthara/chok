// Package scheduler wraps robfig/cron/v3 for running periodic background
// jobs with panic-safe execution, per-job overlap policies, and observable
// statistics.
//
// Scope: single-instance deployments. Distributed locking and multi-node
// coordination are intentionally out of scope.
//
// Execution pipeline for cron-triggered jobs:
//
//	cron schedule -> Policy (Delay / Skip) -> Recover -> execute -> Job.Run
//
// Policy is outer so it always observes a clean return from its wrapped
// chain and its internal bookkeeping (mutex unlock / channel refill) is
// never skipped by a panic. execute() additionally has its own defer
// recover so that RunNow (which bypasses the chain) is equally safe and
// so that panic is accounted for in statistics.
package scheduler

import (
	"context"
	"errors"
)

// ErrBusy signals that a Job refused to run because its own concurrency
// guard rejected the call (an instance is already in progress).
//
// execute treats ErrBusy as neither success nor failure: RunCount and
// FailCount are unchanged, LastErr is not overwritten, and only a Debug
// log line is emitted. Return ErrBusy directly, or wrap it with
// fmt.Errorf("...: %w", ErrBusy); both forms are recognised via errors.Is.
var ErrBusy = errors.New("scheduler: job busy")

// Policy controls how the scheduler handles a cron trigger that fires
// while a previous run of the same Job is still in progress.
type Policy int

const (
	// PolicyDelayIfRunning queues the new trigger until the previous run
	// finishes. Suitable for infrequent, must-not-skip tasks.
	PolicyDelayIfRunning Policy = iota

	// PolicySkipIfRunning drops the new trigger. Suitable for frequent
	// tasks where missing an occasional tick is acceptable.
	PolicySkipIfRunning
)

// Job is the contract for anything registered with Scheduler.
//
// Name must be unique within a Scheduler and is also the key for RunNow.
// Spec is any expression accepted by robfig/cron/v3 (e.g. "@every 1h",
// "0 */5 * * *"). Run must respect ctx cancellation; returning ErrBusy
// indicates "refused by internal guard" and is not counted as a failure.
type Job interface {
	Name() string
	Spec() string
	Policy() Policy
	Run(ctx context.Context) error
}
