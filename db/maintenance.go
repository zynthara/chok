package db

import (
	"context"
	"sync"
	"time"
)

// maintenance keeps a SQLite file healthy: a periodic WAL checkpoint, so
// the log does not grow without bound under constant readers, and a
// periodic PRAGMA optimize, plus one more on close.
type maintenance struct {
	d       *DB
	jobCtx  context.Context
	cancel  context.CancelFunc
	stop    chan struct{}
	done    chan struct{}
	stopped sync.Once
}

func startMaintenance(d *DB, c SQLiteConfig) *maintenance {
	if c.CheckpointInterval <= 0 && c.OptimizeInterval <= 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &maintenance{d: d, jobCtx: ctx, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
	checkpoint, stopCheckpoint := ticker(c.CheckpointInterval)
	optimize, stopOptimize := ticker(c.OptimizeInterval)
	go func() {
		defer close(m.done)
		defer stopCheckpoint()
		defer stopOptimize()
		for {
			select {
			case <-m.stop:
				return
			case <-checkpoint:
				m.checkpoint(m.jobCtx)
			case <-optimize:
				m.optimize(m.jobCtx)
			}
		}
	}()
	return m
}

func ticker(d time.Duration) (<-chan time.Time, func()) {
	if d <= 0 {
		return nil, func() {}
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// close stops the loop, interrupting a job that outlives ctx, then runs a
// parting optimize if ctx still has time.
func (m *maintenance) close(ctx context.Context) {
	m.stopped.Do(func() {
		defer m.cancel()
		close(m.stop)
		select {
		case <-m.done:
		case <-ctx.Done():
			m.cancel()
			<-m.done
			return
		}
		if ctx.Err() == nil {
			m.optimize(ctx)
		}
	})
}

func (m *maintenance) checkpoint(ctx context.Context) {
	var busy, frames, checkpointed int
	err := m.d.gdb.WithContext(ctx).Raw("PRAGMA wal_checkpoint(TRUNCATE)").Row().Scan(&busy, &frames, &checkpointed)
	switch {
	case err != nil:
		m.d.Logger().Warn("db: sqlite WAL checkpoint failed", "error", err)
	case busy == 1:
		m.d.Logger().Debug("db: sqlite WAL checkpoint deferred by active readers", "wal_frames", frames)
	}
}

func (m *maintenance) optimize(ctx context.Context) {
	if err := m.d.gdb.WithContext(ctx).Exec("PRAGMA optimize").Error; err != nil {
		m.d.Logger().Warn("db: sqlite optimize failed", "error", err)
	}
}
