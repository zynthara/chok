package db

import (
	"context"
	"sync"
	"time"

	"gorm.io/plugin/dbresolver"

	"github.com/zynthara/chok/v2/kernel"
)

// maintCloseTimeout caps how long close waits for the loop to drain
// and the parting optimize to run — maintenance is housekeeping and
// must leave the rest of the shutdown budget to the pool close.
const maintCloseTimeout = 5 * time.Second

// maintCloseGrace bounds the wait for an in-flight job after its SQL
// has been interrupted — a driver that ignores cancellation must not
// hang shutdown; the job goroutine is abandoned instead (the loop
// still exits on its own once the statement finally returns).
const maintCloseGrace = time.Second

const (
	maintenanceOK       = "ok"
	maintenanceError    = "error"
	maintenanceDeferred = "deferred"
)

// sqliteMaintenance is the background caretaker a long-lived process
// owes an embedded database — the jobs a database server would run
// for itself:
//
//   - PRAGMA wal_checkpoint(TRUNCATE): commits only ever append under
//     WAL; checkpointing folds the log back into the main file and
//     truncates it, so a busy writer next to long-lived readers
//     cannot grow the -wal file without bound.
//   - PRAGMA optimize: refreshes the query planner's statistics —
//     SQLite's recommendation for long-lived connections ("run
//     periodically, and once before close").
//
// Started by the db module for file-backed sqlite instances (memory
// databases vanish with the process; there is nothing to maintain),
// stopped synchronously before the pools close. Both statements run
// on the write pool: they take the single write connection for their
// duration, which is exactly the serialization they need.
type sqliteMaintenance struct {
	h        *DB
	logger   kernel.Logger
	instance string

	// jobCtx is what in-flight job SQL runs under: the Init context's
	// values (trace correlation) without its deadline — the loop
	// outlives Init. jobCancel belongs to close: when the shutdown
	// budget expires it interrupts the statement instead of letting it
	// outlive the registry teardown.
	jobCtx    context.Context
	jobCancel context.CancelFunc

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	// onTick observes every job run and its classified result. Production
	// metrics and tests share the same seam; carrying the explicit result is
	// necessary because a deferred checkpoint has err == nil.
	onTick func(job, result string)
}

// startSQLiteMaintenance launches the loop; nil when both intervals
// are disabled (nothing to run — no goroutine either).
func startSQLiteMaintenance(ctx context.Context, h *DB, o *SQLiteOptions, logger kernel.Logger, instance string, observer func(job, result string)) *sqliteMaintenance {
	m := newSQLiteMaintenance(ctx, h, o, logger, instance)
	if m != nil {
		m.onTick = observer
		m.start(o)
	}
	return m
}

func newSQLiteMaintenance(ctx context.Context, h *DB, o *SQLiteOptions, logger kernel.Logger, instance string) *sqliteMaintenance {
	if o.CheckpointInterval <= 0 && o.OptimizeInterval <= 0 {
		return nil
	}
	jobCtx, jobCancel := context.WithCancel(context.WithoutCancel(ctx))
	return &sqliteMaintenance{
		h:         h,
		logger:    logger,
		instance:  instance,
		jobCtx:    jobCtx,
		jobCancel: jobCancel,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

func (m *sqliteMaintenance) start(o *SQLiteOptions) {
	checkpointC, stopCheckpoint := maintTicker(o.CheckpointInterval)
	optimizeC, stopOptimize := maintTicker(o.OptimizeInterval)
	go func() {
		defer close(m.done)
		defer stopCheckpoint()
		defer stopOptimize()
		for {
			select {
			case <-m.stop:
				return
			case <-checkpointC:
				m.checkpoint(m.jobCtx)
			case <-optimizeC:
				m.optimize(m.jobCtx)
			}
		}
	}()
}

// maintTicker returns a nil channel for a disabled interval — a nil
// channel never fires in select, so the loop simply ignores that job.
func maintTicker(d time.Duration) (<-chan time.Time, func()) {
	if d <= 0 {
		return nil, func() {}
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// close stops the loop, waits for an in-flight job to finish, and
// runs the parting PRAGMA optimize SQLite recommends before a
// long-lived connection goes away — all within ctx's budget: when it
// expires the in-flight statement is interrupted and the parting
// optimize skipped, so a stuck PRAGMA cannot hang shutdown.
// Idempotent.
func (m *sqliteMaintenance) close(ctx context.Context) {
	m.stopOnce.Do(func() {
		defer m.jobCancel()
		// Cap the drain even under the kernel's (or an unbudgeted
		// caller's) larger allowance — maintenance is housekeeping.
		ctx, cancel := context.WithTimeout(ctx, maintCloseTimeout)
		defer cancel()
		close(m.stop)
		select {
		case <-m.done:
		case <-ctx.Done():
			m.jobCancel() // interrupt the in-flight statement
			select {
			case <-m.done:
			case <-time.After(maintCloseGrace):
				m.logger.Warn("db: sqlite maintenance abandoning in-flight job — close budget exhausted",
					"instance", m.instance)
				return
			}
		}
		if ctx.Err() != nil {
			m.logger.Warn("db: sqlite maintenance skipping parting optimize — close budget exhausted",
				"instance", m.instance)
			return
		}
		m.optimize(ctx)
	})
}

func (m *sqliteMaintenance) checkpoint(ctx context.Context) {
	// The write clause pins the statement to the write pool — a
	// checkpoint is a writer even though it looks like a query.
	var busy, logFrames, checkpointed int
	row := m.h.gdb.WithContext(ctx).Clauses(dbresolver.Write).Raw("PRAGMA wal_checkpoint(TRUNCATE)").Row()
	err := row.Scan(&busy, &logFrames, &checkpointed)
	result := maintenanceResult(err, busy == 1)
	switch {
	case err != nil:
		m.logger.Warn("db: sqlite wal checkpoint failed",
			"instance", m.instance, "error", err)
	case busy == 1:
		// Not an error: a reader held a snapshot past busy_timeout.
		// The next tick retries; persistent busy means a query is
		// holding its Rows() open far too long.
		m.logger.Debug("db: sqlite wal checkpoint deferred by active readers",
			"instance", m.instance, "wal_frames", logFrames)
	}
	if m.onTick != nil {
		m.onTick("checkpoint", result)
	}
}

func (m *sqliteMaintenance) optimize(ctx context.Context) {
	err := m.h.gdb.WithContext(ctx).Clauses(dbresolver.Write).Exec("PRAGMA optimize").Error
	result := maintenanceResult(err, false)
	if err != nil {
		m.logger.Warn("db: sqlite optimize failed",
			"instance", m.instance, "error", err)
	}
	if m.onTick != nil {
		m.onTick("optimize", result)
	}
}

func maintenanceResult(err error, deferred bool) string {
	if err != nil {
		return maintenanceError
	}
	if deferred {
		return maintenanceDeferred
	}
	return maintenanceOK
}
