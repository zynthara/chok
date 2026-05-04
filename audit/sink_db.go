package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/rid"
)

// marshalJSON wraps json.Marshal + datatypes.JSON conversion. Kept
// as a helper so sink_db / future sinks share the same encoding —
// `nil` operator-supplied values become datatypes.JSON("null") which
// preserves "field was explicitly null" vs "field was omitted" in
// the rendered JSON column.
func marshalJSON(v any) (datatypes.JSON, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(raw), nil
}

// DBLogger is the async DB-backed Logger used by parts/audit. The
// Component owns one and exposes it as audit.Logger to callers.
//
// Why not parts/pool: audit is a single-producer-MPSC batched sink —
// many callers feed one channel, one worker drains, batches by size
// or interval, commits with one INSERT. Pool's any-func-any-time
// task model doesn't compose with batch ordering or with a Close
// that must drain the in-flight batch before db is closed by parts.
// SPEC §8 was updated (v0.3.5) to drop pool from Dependencies.
//
// Backpressure semantics:
//
//   - DropOnFull=true  : Log enqueues non-blocking. Channel full ⇒
//     entry dropped + Stats.Dropped++. Use when audit must never
//     delay business requests.
//   - DropOnFull=false : Log blocks until channel has space (or ctx
//     cancels). Use when audit is a compliance precondition for
//     the business operation.
//
// LogSync always commits synchronously regardless of DropOnFull,
// for the rare callsite that needs "audit before action".
type DBLogger struct {
	db     *gorm.DB
	logger log.Logger

	in chan Entry

	dropOnFull    bool
	batchSize     int
	flushInterval time.Duration

	// Worker lifecycle. closeOnce makes Close idempotent against
	// double-call (defensive in nested cleanup paths). done closes
	// when the worker has exited and any in-flight batch is committed.
	closeOnce sync.Once
	done      chan struct{}

	// Best-effort counters. Lifetime totals; resets on process
	// restart. Read via Stats() — paired with periodic logger.Warn
	// on each non-success path so operators never need to poll
	// Stats() to learn there's a problem.
	pending atomic.Int64
	dropped atomic.Uint64
	written atomic.Uint64
	failed  atomic.Uint64
	lastErr atomic.Pointer[errBox]
}

// errBox boxes an error for atomic.Pointer storage. Pointer-to-error
// can't be atomic without a wrapper because errors are interfaces.
type errBox struct {
	at  time.Time
	err error
}

const (
	// defaultBatchSize bounds INSERT row count per commit. 100 is a
	// conventional sweet spot — large enough to amortise transaction
	// overhead on busy audits, small enough that a single failure
	// loses bounded data.
	defaultBatchSize = 100

	// defaultFlushInterval ensures low-traffic audits don't sit in
	// the channel for minutes waiting for batchSize to fill. 1s is
	// short enough to feel "real-time" to admin UIs while still
	// amortising Postgres/MySQL fsync.
	defaultFlushInterval = 1 * time.Second
)

// DBLoggerOption tunes test-side knobs (smaller batches, faster
// flush) without exposing the surface to operators — production
// callers go through parts/audit which uses the constants.
type DBLoggerOption func(*DBLogger)

func withBatchSize(n int) DBLoggerOption {
	return func(l *DBLogger) {
		if n > 0 {
			l.batchSize = n
		}
	}
}

func withFlushInterval(d time.Duration) DBLoggerOption {
	return func(l *DBLogger) {
		if d > 0 {
			l.flushInterval = d
		}
	}
}

// NewDBLogger constructs an async DB-backed Logger. The worker
// goroutine starts immediately under parent (typically
// context.Background; do NOT pass an Init ctx — it's bounded by
// startup timeout and would kill the long-running worker).
//
// db must be ready (post-Init); bufferSize > 0; logger may be nil
// (defaults to log.Empty()).
func NewDBLogger(parent context.Context, db *gorm.DB, bufferSize int, dropOnFull bool, logger log.Logger, opts ...DBLoggerOption) *DBLogger {
	if logger == nil {
		logger = log.Empty()
	}
	if parent == nil {
		parent = context.Background()
	}
	l := &DBLogger{
		db:            db,
		logger:        logger,
		in:            make(chan Entry, bufferSize),
		dropOnFull:    dropOnFull,
		batchSize:     defaultBatchSize,
		flushInterval: defaultFlushInterval,
		done:          make(chan struct{}),
	}
	for _, opt := range opts {
		opt(l)
	}
	go l.run(parent)
	return l
}

// Log enqueues asynchronously per the Logger contract.
func (l *DBLogger) Log(ctx context.Context, entry Entry) {
	if l.dropOnFull {
		select {
		case l.in <- entry:
			l.pending.Add(1)
		default:
			l.dropped.Add(1)
			l.logger.Warn("audit: buffer full, dropping entry (DropOnFull=true)",
				"action", entry.Action,
				"resource", entry.Resource,
			)
		}
		return
	}
	// Block-until-space mode. Honour ctx cancellation so a hot-path
	// caller with a request deadline doesn't hang past it.
	select {
	case l.in <- entry:
		l.pending.Add(1)
	case <-ctx.Done():
		l.dropped.Add(1)
		l.logger.Warn("audit: buffer full and ctx cancelled, dropping entry",
			"action", entry.Action,
			"resource", entry.Resource,
			"ctx_err", ctx.Err().Error(),
		)
	}
}

// LogSync commits a single Entry synchronously. The pending counter
// is not touched (sync writes don't go through the buffer).
func (l *DBLogger) LogSync(ctx context.Context, entry Entry) error {
	row, err := buildLog(entry)
	if err != nil {
		return fmt.Errorf("audit: build log: %w", err)
	}
	if err := l.db.WithContext(ctx).Create(&row).Error; err != nil {
		l.failed.Add(1)
		l.lastErr.Store(&errBox{at: time.Now(), err: err})
		return fmt.Errorf("audit: insert log: %w", err)
	}
	l.written.Add(1)
	return nil
}

// Query reads persisted records with the given filter + pagination.
func (l *DBLogger) Query(ctx context.Context, q Query) ([]Log, int64, error) {
	tx := l.db.WithContext(ctx).Model(&Log{})
	if q.ActorID != "" {
		tx = tx.Where("actor_id = ?", q.ActorID)
	}
	if q.Resource != "" {
		tx = tx.Where("resource = ?", q.Resource)
	}
	if q.ResourceID != "" {
		tx = tx.Where("resource_id = ?", q.ResourceID)
	}
	if q.Action != "" {
		tx = tx.Where("action = ?", q.Action)
	}
	if q.Result != "" {
		tx = tx.Where("result = ?", q.Result)
	}
	if !q.From.IsZero() {
		tx = tx.Where("occurred_at >= ?", q.From)
	}
	if !q.To.IsZero() {
		tx = tx.Where("occurred_at <= ?", q.To)
	}

	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("audit: query count: %w", err)
	}

	page, size := q.Page, q.Size
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = 50
	}

	var rows []Log
	if err := tx.Order("occurred_at DESC").
		Limit(size).Offset((page - 1) * size).
		Find(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("audit: query find: %w", err)
	}
	return rows, total, nil
}

// Stats returns a snapshot of the lifetime counters.
func (l *DBLogger) Stats() Stats {
	pending := max(l.pending.Load(), 0) // defensive: counter underflow never expected
	return Stats{
		Pending: uint64(pending),
		Dropped: l.dropped.Load(),
		Written: l.written.Load(),
		Failed:  l.failed.Load(),
	}
}

// LastError returns the most recent sink-side DB error and when it
// happened. Returns (nil, zero-time) when no error has occurred.
// Useful for the parts/audit Health output.
func (l *DBLogger) LastError() (error, time.Time) {
	box := l.lastErr.Load()
	if box == nil {
		return nil, time.Time{}
	}
	return box.err, box.at
}

// Close stops the worker. Idempotent. Returns when the worker has
// drained any pending batch and exited. Caller (parts/audit
// Component.Close) is responsible for waiting before letting the
// underlying DB go away.
func (l *DBLogger) Close() {
	l.closeOnce.Do(func() {
		close(l.in)
	})
	<-l.done
}

// run is the sink worker. Drains in until closed; flushes every
// flushInterval or as soon as the in-memory batch reaches batchSize.
// On in close, flushes any remaining batch and returns.
func (l *DBLogger) run(parent context.Context) {
	defer close(l.done)

	batch := make([]Log, 0, l.batchSize)
	timer := time.NewTimer(l.flushInterval)
	defer timer.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		l.commitBatch(parent, batch)
		// Reset slice header to length 0 but keep capacity so we
		// don't reallocate per flush.
		batch = batch[:0]
	}

	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(l.flushInterval)
	}

	for {
		select {
		case entry, ok := <-l.in:
			if !ok {
				flush()
				return
			}
			l.pending.Add(-1)
			row, err := buildLog(entry)
			if err != nil {
				// Build failure (e.g. JSON marshal) is not transient;
				// log + count + drop. Don't crash the worker on a
				// malformed payload.
				l.failed.Add(1)
				l.lastErr.Store(&errBox{at: time.Now(), err: err})
				l.logger.Error("audit: build log failed; dropping entry",
					"action", entry.Action,
					"error", err.Error(),
				)
				continue
			}
			batch = append(batch, row)
			if len(batch) >= l.batchSize {
				flush()
				resetTimer()
			}
		case <-timer.C:
			flush()
			timer.Reset(l.flushInterval)
		}
	}
}

// commitBatch is a single transactional INSERT. Failure increments
// the failed counter and records lastErr; rows in the failed batch
// are dropped (at-most-once — rolling batches back into the in
// channel would risk ordering churn and unbounded memory growth on
// a sustained DB outage).
func (l *DBLogger) commitBatch(ctx context.Context, batch []Log) {
	if err := l.db.WithContext(ctx).CreateInBatches(batch, len(batch)).Error; err != nil {
		l.failed.Add(uint64(len(batch)))
		l.lastErr.Store(&errBox{at: time.Now(), err: err})
		l.logger.Error("audit: batch insert failed; dropping batch",
			"size", len(batch),
			"error", err.Error(),
		)
		return
	}
	l.written.Add(uint64(len(batch)))
}

// buildLog converts an Entry into a Log row. Defaults are applied
// here so every sink path (sync + async) produces identical shape.
func buildLog(entry Entry) (Log, error) {
	if entry.Action == "" {
		return Log{}, errors.New("audit: Action is required")
	}
	row := Log{
		ID:         rid.New("audit"),
		ActorID:    entry.ActorID,
		ActorType:  entry.ActorType,
		ActorIP:    entry.ActorIP,
		Action:     entry.Action,
		Result:     entry.Result,
		Resource:   entry.Resource,
		ResourceID: entry.ResourceID,
		TraceID:    entry.TraceID,
		RequestID:  entry.RequestID,
		Reason:     entry.Reason,
	}
	if row.Result == "" {
		row.Result = ResultSuccess
	}
	if row.ActorType == "" && row.ActorID != "" {
		row.ActorType = ActorTypeUser
	}
	if !entry.OccurredAt.IsZero() {
		row.OccurredAt = entry.OccurredAt
	} else {
		row.OccurredAt = time.Now()
	}
	if entry.Before != nil {
		raw, err := marshalJSON(entry.Before)
		if err != nil {
			return Log{}, fmt.Errorf("marshal Before: %w", err)
		}
		row.Before = raw
	}
	if entry.After != nil {
		raw, err := marshalJSON(entry.After)
		if err != nil {
			return Log{}, fmt.Errorf("marshal After: %w", err)
		}
		row.After = raw
	}
	if len(entry.Metadata) > 0 {
		raw, err := marshalJSON(entry.Metadata)
		if err != nil {
			return Log{}, fmt.Errorf("marshal Metadata: %w", err)
		}
		row.Metadata = raw
	}
	return row, nil
}

// Compile-time interface assertion.
var (
	_ Logger  = (*DBLogger)(nil)
	_ Statser = (*DBLogger)(nil)
)
