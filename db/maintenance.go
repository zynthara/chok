package db

import (
	"sync"
	"time"

	"gorm.io/plugin/dbresolver"

	"github.com/zynthara/chok/v2/kernel"
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

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	// onTick is a test seam: observes every job run and its result.
	// Set before start; never touched afterwards.
	onTick func(job string, err error)
}

// startSQLiteMaintenance launches the loop; nil when both intervals
// are disabled (nothing to run — no goroutine either).
func startSQLiteMaintenance(h *DB, o *SQLiteOptions, logger kernel.Logger, instance string) *sqliteMaintenance {
	m := newSQLiteMaintenance(h, o, logger, instance)
	if m != nil {
		m.start(o)
	}
	return m
}

func newSQLiteMaintenance(h *DB, o *SQLiteOptions, logger kernel.Logger, instance string) *sqliteMaintenance {
	if o.CheckpointInterval <= 0 && o.OptimizeInterval <= 0 {
		return nil
	}
	return &sqliteMaintenance{
		h:        h,
		logger:   logger,
		instance: instance,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
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
				m.checkpoint()
			case <-optimizeC:
				m.optimize()
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
// long-lived connection goes away. Idempotent.
func (m *sqliteMaintenance) close() {
	m.stopOnce.Do(func() {
		close(m.stop)
		<-m.done
		m.optimize()
	})
}

func (m *sqliteMaintenance) checkpoint() {
	// The write clause pins the statement to the write pool — a
	// checkpoint is a writer even though it looks like a query.
	var busy, logFrames, checkpointed int
	row := m.h.gdb.Clauses(dbresolver.Write).Raw("PRAGMA wal_checkpoint(TRUNCATE)").Row()
	err := row.Scan(&busy, &logFrames, &checkpointed)
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
		m.onTick("checkpoint", err)
	}
}

func (m *sqliteMaintenance) optimize() {
	err := m.h.gdb.Clauses(dbresolver.Write).Exec("PRAGMA optimize").Error
	if err != nil {
		m.logger.Warn("db: sqlite optimize failed",
			"instance", m.instance, "error", err)
	}
	if m.onTick != nil {
		m.onTick("optimize", err)
	}
}
