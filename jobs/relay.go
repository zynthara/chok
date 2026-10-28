package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/zynthara/chok/v3/db"
)

// relay is the delivery engine of one worker: scan the committed messages
// of its topic past the persisted watermark in (created_at, id) order,
// hand each to the handler, advance the watermark over the settled prefix.
// It is v2's outbox relay with its algorithm unchanged; a v3 worker is
// always the topic-filtered shape.
//
// Scanning is the overlapping-watermark idiom: created_at >= W plus
// dedup. Dedup has two layers: positions at or before the persisted
// watermark are settled-delivered (exact keyset skip — sound because the
// watermark never advances into the unsettled window where late commits
// could still reorder ties), and mem remembers rows delivered in earlier
// sweeps that are not yet settled enough to advance the watermark over.
//
// v3 adds the failure handling around the handler call (attempt): a
// failure is counted on the message row and retried after a backoff,
// the last allowed failure parks the message in chok_jobs_dead and the
// sweep goes on; and mem outlives a graceful restart through
// chok_jobs_delivered (restore, saveDelivered).
type relay struct {
	topic   string
	handler func(ctx context.Context, m rawMessage) error
	states  *stateStore
	d       *db.DB
	logger  *slog.Logger
	retry   retryPolicy
	observe func(ctx context.Context, ev Event) // reports message.parked; nil without observers

	settle   time.Duration
	batch    int
	now      func() time.Time // injectable clock for settle tests
	maxPages int              // one sweep's page budget (maxSweepPages; test-tunable)

	delivered atomic.Int64 // messages the handler took, for Stats
	parked    atomic.Int64 // messages parked in the dead letters, for Stats

	// mu guards mem and restored. The scheduler never overlaps a
	// worker's sweeps; the TryLock turns an overlap into an error instead
	// of a race.
	mu       sync.Mutex
	mem      map[uint64]time.Time // delivered (or parked) but not yet settled: id → created_at
	restored bool                 // the first sweep read the delivered set back

	headMu sync.Mutex
	head   headState // where the last sweep stopped on a failing message, for Stats
}

// retryPolicy is what a worker does with a message its function fails
// on: try again after delay(n), park it at the maxAttempts-th failure.
type retryPolicy struct {
	maxAttempts         int
	backoff, maxBackoff time.Duration
}

// delay is the wait after a message's n-th failure: backoff doubled n-1
// times, at most maxBackoff. A zero backoff tries again on the next sweep.
func (p retryPolicy) delay(n int) time.Duration {
	d := p.backoff
	for i := 1; i < n && d < p.maxBackoff; i++ {
		if d > p.maxBackoff/2 {
			return p.maxBackoff
		}
		d *= 2
	}
	return min(d, p.maxBackoff)
}

// headState is the message a worker's last sweep stopped on because it
// failed: its failed deliveries so far and when it is tried next. Zero
// while the topic flows.
type headState struct {
	attempts int
	next     time.Time
}

// scanPos is the in-sweep cursor: the created_at boundary plus how many
// rows at or after it were already consumed from the result.
// OFFSET-on-boundary (not a bare LIMIT re-query) is what makes a tie
// group larger than one batch progress instead of spinning in place.
type scanPos struct {
	ts     time.Time
	offset int
}

// errSweepRunning is a sweep refused because another sweep of the same
// worker is in flight.
var errSweepRunning = errors.New("jobs: a sweep of this worker is already running")

// errRetryLater ends a sweep quietly at a message whose next try is not
// due yet: the topic waits without calling the handler. The scheduler
// counts the sweep as a run that succeeded and keeps LastError, the
// failure the topic still waits on.
var errRetryLater = errors.New("jobs: the next try of the head message is not due")

// loggedError is a delivery failure the relay has logged itself, with the
// message's attempt and next try: the scheduler counts the failed sweep
// without logging it again.
type loggedError struct{ error }

func (e loggedError) Unwrap() error { return e.error }

// maxErrorLen bounds the error stored with a failing or parked message:
// an error can carry a whole response body.
const maxErrorLen = 2000

// errorText is err's message, cut to maxErrorLen bytes on a rune
// boundary.
func errorText(err error) string {
	s := err.Error()
	if len(s) <= maxErrorLen {
		return s
	}
	s = s[:maxErrorLen]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// newRelay builds the relay of a worker. Its retry policy tries a failed
// message again on the next sweep and parks it at the default
// MaxAttempts; New sets the Config's.
func newRelay(topic string, handler func(ctx context.Context, m rawMessage) error, c *core, settle time.Duration, batch int) (*relay, error) {
	if err := checkTopic(topic); err != nil {
		return nil, fmt.Errorf("jobs: worker %q: %w", topic, err)
	}
	if handler == nil {
		return nil, fmt.Errorf("jobs: worker %q has a nil function", topic)
	}
	return &relay{
		topic:    topic,
		handler:  handler,
		states:   c.states,
		d:        c.d,
		logger:   c.logger.With("topic", topic),
		retry:    retryPolicy{maxAttempts: defaultMaxAttempts},
		settle:   settle,
		batch:    batch,
		now:      time.Now,
		maxPages: maxSweepPages,
		mem:      make(map[uint64]time.Time),
	}, nil
}

// maxSweepPages bounds one sweep. The bound is what keeps the fixed
// pre-scan cutoff compatible with sustained production: a sweep that
// could loop forever would hold one cutoff forever (the round-2 #1
// memory pathology); capped sweeps end, and the next tick rescans the
// overlap from the persisted watermark under a fresh cutoff, so rows
// settle and mem prunes across sweeps. With the default batch size this
// is 10k rows per sweep per worker — delivery throughput is paced,
// correctness is not affected (at-least-once holds at any budget).
const maxSweepPages = 100

// run is one sweep. An error aborted it with the watermark parked before
// the failed row; errRetryLater ends it there quietly, at a message whose
// next try is not due.
func (r *relay) run(ctx context.Context) error {
	if !r.mu.TryLock() {
		return errSweepRunning
	}
	defer r.mu.Unlock()

	if !r.restored {
		if err := r.restore(ctx); err != nil {
			return err
		}
		r.restored = true
	}
	var head headState
	err := r.sweep(ctx, &head)
	r.headMu.Lock()
	r.head = head
	r.headMu.Unlock()
	return err
}

// headStats returns where the last sweep stopped on a failing message.
func (r *relay) headStats() headState {
	r.headMu.Lock()
	defer r.headMu.Unlock()
	return r.head
}

// sweep scans and delivers; it sets *head when it stops on a failing
// message.
func (r *relay) sweep(ctx context.Context, head *headState) error {
	w, err := r.states.load(ctx, r.topic)
	if err != nil {
		return err
	}
	// Prune against the loaded watermark before scanning: another process
	// (the accepted last-write-wins degradation) may have advanced the
	// shared state past entries this process still holds in mem — without
	// this they would outlive every local advance (round-2 review).
	r.prune(w)
	cand := w

	// Rows younger than the cutoff are unsettled: a still-open
	// transaction may yet commit a row with an earlier created_at, so the
	// persisted watermark must not advance past them. Delivery is NOT
	// delayed — unsettled rows are handed out immediately and remembered
	// in mem; settle only gates watermark advancement.
	//
	// The cutoff is taken ONCE, before the scan (round-3 review —
	// restoring this closed a Critical): covering a position is safe only
	// when its created_at predates the moment the CURSOR PASSED that
	// position by settle — an invisible row there can otherwise still
	// commit (within its own settle budget) after the pass, and the cursor
	// never returns within this sweep. Sweep start lower-bounds every pass
	// time, so the pre-scan cutoff is sound; the round-2 per-batch refresh
	// was not ("cutoff predates persist" ignores pass times) — it could
	// settle positions passed while a perfectly legal transaction was
	// still invisible, skipping its row forever. The endless-sweep memory
	// concern that refresh addressed is handled by maxSweepPages instead:
	// the next tick takes a fresh cutoff and rescans the overlap from the
	// persisted watermark, so entries settle and prune across sweeps.
	cutoff := r.now().Add(-r.settle).UTC()
	pages := 0
	drained := false

	// Stage 1 — the watermark-boundary tie remainder, resumed by COMPOSITE
	// position (round-4 review): rows AT w.At with id > w.ID, advanced by
	// id keyset. Restarting every sweep at created_at >= w.At OFFSET 0
	// refetched the covered prefix just to skip it in Go; each refetch
	// burned page budget, so a settled tie group wider than one sweep's
	// budget ate every sweep before reaching its own tail — the tail rows
	// starved forever (and the strict < cleanup never removes the
	// same-timestamp prefix, so it never recovered). Excluding the prefix
	// in SQL by exact keyset at the persisted watermark is safe for the
	// same reason the Go-side covers() skip was: the watermark is a
	// settled position, nothing can still commit at or before it.
	if w.ok {
		bAt, lastID := w.At, w.ID // fixed boundary; w advances as batches persist
		for pages < r.maxPages {
			if err := ctx.Err(); err != nil {
				r.persist(ctx, &w, cand)
				return err
			}
			pages++
			var page []message
			err := r.scope(ctx).
				Where("created_at = ? AND id > ?", bAt.UTC(), lastID).
				Order("created_at").Order("id").
				Limit(r.batch).
				Find(&page).Error
			if err != nil {
				r.persist(ctx, &w, cand)
				return fmt.Errorf("jobs: worker %q: scan the boundary: %w", r.topic, err)
			}
			for i := range page {
				at, id := page[i].CreatedAt, page[i].ID
				lastID = id
				if err := r.deliver(ctx, &page[i], at, id, w, &cand, cutoff, head); err != nil {
					r.persist(ctx, &w, cand)
					return err
				}
			}
			r.persist(ctx, &w, cand)
			if len(page) < r.batch {
				break
			}
		}
	}

	// Stage 2 — strictly beyond the boundary timestamp; the first fetch
	// uses > so the boundary group is never refetched, then the cursor
	// falls back to the >=-plus-offset progression for tie groups it
	// discovers itself.
	pos := scanPos{ts: w.At}
	strict := w.ok
	for ; pages < r.maxPages; pages++ {
		if err := ctx.Err(); err != nil {
			r.persist(ctx, &w, cand)
			return err
		}
		items, err := r.scanForward(ctx, pos, r.batch, strict)
		if err != nil {
			r.persist(ctx, &w, cand)
			return err
		}
		if len(items) == 0 {
			drained = true
			break
		}
		strict = false
		for i := range items {
			at, id := items[i].CreatedAt, items[i].ID
			if at.Equal(pos.ts) {
				pos.offset++
			} else {
				pos = scanPos{ts: at, offset: 1}
			}
			if err := r.deliver(ctx, &items[i], at, id, w, &cand, cutoff, head); err != nil {
				r.persist(ctx, &w, cand)
				return err
			}
		}
		// Per-batch persistence bounds the crash replay window during long
		// catch-up sweeps.
		r.persist(ctx, &w, cand)
		if len(items) < r.batch {
			drained = true
			break
		}
	}
	// A worker's scan only sees its topic, so a quiet topic would never
	// advance its watermark — leaving no state row (or a stale one) that
	// blocks the retention cleanup forever (round-2 review). Once the scan
	// is DRAINED, every message of the topic up to the settled frontier is
	// delivered, so the watermark may jump over the other topics' rows to
	// that frontier. Drained clean exits only: handler failures returned
	// above (head-of-line intact) and a budget-stopped sweep prove nothing
	// about rows they never reached. The probe reuses the pre-scan cutoff
	// for the same pass-time reason as the loop: a probe-time cutoff could
	// settle a match that committed after the cursor passed its position.
	if drained {
		f, err := r.frontier(ctx, cutoff)
		if err != nil {
			r.logger.WarnContext(ctx, "jobs: the frontier probe failed; the retention floor advances on a later sweep", "error", err)
		} else if f.ok && cand.after(f.At, f.ID) {
			cand = f
		}
	}
	r.persist(ctx, &w, cand)
	return nil
}

// scope is a query over the worker's topic.
func (r *relay) scope(ctx context.Context) *gorm.DB {
	return r.d.Unsafe(ctx).Model(&message{}).Where("topic = ?", r.topic)
}

// frontier returns the newest (created_at, id) position at or before
// cutoff across the WHOLE table — the highest watermark a drained worker
// may claim. cutoff MUST be the sweep's pre-scan cutoff (see run).
func (r *relay) frontier(ctx context.Context, cutoff time.Time) (watermark, error) {
	var row message
	err := r.d.Unsafe(ctx).Model(&message{}).
		Where("created_at <= ?", cutoff.UTC()).
		Order("created_at DESC").Order("id DESC").
		Limit(1).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return watermark{}, nil
	}
	if err != nil {
		return watermark{}, fmt.Errorf("jobs: worker %q: the settled frontier: %w", r.topic, err)
	}
	return watermark{At: row.CreatedAt, ID: row.ID, ok: true}, nil
}

// scanForward fetches one stage-2 batch at the cursor, ordered
// (created_at, id). The first fetch after a watermark is strict
// (created_at > pos.ts — stage 1 owns the boundary group); afterwards it
// is the >= overlap (a zero boundary means "from the beginning" and adds
// no filter), skipping the pos.offset rows already consumed at this
// boundary.
func (r *relay) scanForward(ctx context.Context, pos scanPos, batch int, strict bool) ([]message, error) {
	q := r.scope(ctx).Order("created_at").Order("id").Limit(batch)
	switch {
	case strict:
		// Stage 2's first fetch: strictly beyond the watermark boundary
		// (its tie group was drained by stage 1).
		q = q.Where("created_at > ?", pos.ts.UTC())
	default:
		q = q.Offset(pos.offset)
		if !pos.ts.IsZero() {
			q = q.Where("created_at >= ?", pos.ts.UTC())
		}
	}
	var items []message
	if err := q.Find(&items).Error; err != nil {
		return nil, fmt.Errorf("jobs: worker %q: scan: %w", r.topic, err)
	}
	return items, nil
}

// deliver handles one scanned row: dedup (persisted-watermark keyset,
// then process memory), the attempt, and the settled-candidate advance.
// Everything up to and including this row is delivered (or parked) when
// it returns nil (head-of-line: an error aborts the sweep above, so the
// prefix stays contiguous).
func (r *relay) deliver(ctx context.Context, row *message, at time.Time, id uint64, w watermark, cand *watermark, cutoff time.Time, head *headState) error {
	delivered := w.covers(at, id)
	if !delivered {
		if _, seen := r.mem[id]; seen {
			delivered = true
		}
	}
	if !delivered {
		if err := r.attempt(ctx, row, head); err != nil {
			return err
		}
		r.mem[id] = at
	}
	if !at.After(cutoff) {
		*cand = watermark{At: at, ID: id, ok: true}
	}
	return nil
}

// attempt delivers row, unless it was parked already or its next try is
// not due (errRetryLater). nil means the topic moves past row: the
// handler took it, or it is parked. A failure below MaxAttempts is
// counted on the row and stops the sweep there; the last one parks it.
func (r *relay) attempt(ctx context.Context, row *message, head *headState) error {
	if row.Attempts > 0 {
		// It failed before. A crash between parking it and the watermark
		// passing it leaves it parked and still ahead of the watermark.
		parked, err := r.isParked(ctx, row.ID)
		if err != nil || parked {
			return err
		}
	}
	if row.RetryAt != nil && row.RetryAt.After(r.now()) {
		*head = headState{attempts: row.Attempts, next: row.RetryAt.UTC()}
		return errRetryLater
	}
	err := r.call(ctx, row.toMessage())
	if err == nil {
		r.delivered.Add(1)
		return nil
	}
	if ctx.Err() != nil {
		return err // stopped by shutdown: not a failure of the message
	}
	n := row.Attempts + 1
	if _, undecodable := errors.AsType[decodeError](err); undecodable || n >= r.retry.maxAttempts {
		return r.park(ctx, row, n, err)
	}
	return r.fail(ctx, row, n, err, head)
}

// call runs the handler on m. A panic is a failed delivery: its stack
// goes to the log at Error, as the scheduler logs a run's.
func (r *relay) call(ctx context.Context, m rawMessage) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
			r.logger.ErrorContext(ctx, "jobs: a worker function panicked", "id", m.ID, "panic", fmt.Sprint(p), "stack", panicStack())
		}
	}()
	return r.handler(ctx, m)
}

// fail records the n-th failure of row on its message row: the count,
// when to try next and the error. The sweep stops at row; the sweeps
// until the next try leave it alone.
func (r *relay) fail(ctx context.Context, row *message, n int, cause error, head *headState) error {
	text := errorText(cause)
	var retryAt *time.Time
	var next time.Time
	if d := r.retry.delay(n); d > 0 {
		next = r.now().Add(d).UTC()
		retryAt = &next
	}
	err := r.d.Unsafe(ctx).Model(&message{}).Where("id = ?", row.ID).
		Updates(map[string]any{"attempts": n, "retry_at": retryAt, "last_error": text}).Error
	if err != nil {
		return fmt.Errorf("jobs: worker %q: record the failure of message %d: %w (the failure: %v)", r.topic, row.ID, err, cause)
	}
	*head = headState{attempts: n, next: next}
	r.logger.WarnContext(ctx, "jobs: a message failed; the topic waits for its next try",
		"id", row.ID, "attempt", n, "max_attempts", r.retry.maxAttempts, "next_try", next, "error", text)
	return loggedError{fmt.Errorf("jobs: worker %q: message %d failed (attempt %d of %d): %w", r.topic, row.ID, n, r.retry.maxAttempts, cause)}
}

// park copies row into the dead letters and records its last failure on
// the message row in one transaction, so a crash leaves it either
// failing or parked, then logs and reports it. Parking twice (a crash
// before the watermark passed it) keeps the first copy.
func (r *relay) park(ctx context.Context, row *message, n int, cause error) error {
	text := errorText(cause)
	parkedAt := r.now().UTC()
	err := r.d.RunInTx(ctx, func(txCtx context.Context) error {
		tx := r.d.Unsafe(txCtx)
		dead := deadRow{
			ID: row.ID, Topic: row.Topic, Payload: row.Payload, CreatedAt: row.CreatedAt,
			ParkedAt: parkedAt, Attempts: n, LastError: text,
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&dead).Error; err != nil {
			return err
		}
		return tx.Model(&message{}).Where("id = ?", row.ID).
			Updates(map[string]any{"attempts": n, "retry_at": nil, "last_error": text}).Error
	})
	if err != nil {
		return fmt.Errorf("jobs: worker %q: park message %d: %w (the failure: %v)", r.topic, row.ID, err, cause)
	}
	r.parked.Add(1)
	r.logger.ErrorContext(ctx, "jobs: a message is parked in the dead letters; the topic moves on",
		"id", row.ID, "attempts", n, "error", text)
	if r.observe != nil {
		r.observe(ctx, Event{Kind: kindParked, Job: r.topic, Type: typeWorker, StartedAt: parkedAt, Error: text})
	}
	return nil
}

// isParked reports whether the message id is in the dead letters.
func (r *relay) isParked(ctx context.Context, id uint64) (bool, error) {
	var n int64
	if err := r.d.Unsafe(ctx).Model(&deadRow{}).Where("id = ?", id).Count(&n).Error; err != nil {
		return false, fmt.Errorf("jobs: worker %q: look for message %d in the dead letters: %w", r.topic, id, err)
	}
	return n > 0, nil
}

// restore reads back the delivered set the worker saved when the jobs
// last stopped — what it had delivered or parked that its watermark had
// not passed — into mem, so this process does not deliver it again, and
// deletes it: the next stop saves mem anew.
func (r *relay) restore(ctx context.Context) error {
	var rows []deliveredRow
	if err := r.d.Unsafe(ctx).Where("topic = ?", r.topic).Find(&rows).Error; err != nil {
		return fmt.Errorf("jobs: worker %q: read the delivered set: %w", r.topic, err)
	}
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		r.mem[row.MessageID] = row.CreatedAt
	}
	if err := r.d.Unsafe(ctx).Where("topic = ?", r.topic).Delete(&deliveredRow{}).Error; err != nil {
		r.logger.WarnContext(ctx, "jobs: deleting the delivered set failed; the next start reads it again", "error", err)
	}
	return nil
}

// saveDelivered writes mem to chok_jobs_delivered for restore. It runs
// when the jobs stop, after the last sweep: a sweep still in flight
// keeps its set (errSweepRunning), and the next start may deliver it
// again, as after a crash.
func (r *relay) saveDelivered(ctx context.Context) (int, error) {
	if !r.mu.TryLock() {
		return 0, errSweepRunning
	}
	defer r.mu.Unlock()
	if len(r.mem) == 0 {
		return 0, nil
	}
	rows := make([]deliveredRow, 0, len(r.mem))
	for id, at := range r.mem {
		rows = append(rows, deliveredRow{Topic: r.topic, MessageID: id, CreatedAt: at.UTC()})
	}
	// A row may be there already when restore could not delete it.
	err := r.d.Unsafe(ctx).Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, deliveredBatch).Error
	if err != nil {
		return 0, fmt.Errorf("jobs: worker %q: save the delivered set: %w", r.topic, err)
	}
	return len(rows), nil
}

// deliveredBatch is how many delivered-set rows one insert writes.
const deliveredBatch = 500

// persist advances the stored watermark to cand when it moved, then
// prunes the mem entries the new watermark covers. Pruning rides every
// advance — batch boundaries and the pre-return saves on failure paths
// alike — so mem stays bounded by the unsettled window instead of the
// whole backlog processed by a long catch-up sweep (round-1 review: an
// end-of-sweep-only prune leaked every settled entry when a later row's
// handler error returned early, and grew without bound while a sweep
// never caught up).
//
// Save errors are logged, not returned: a failed save costs redelivery
// (the next sweep rescans from the older watermark), never loss.
func (r *relay) persist(ctx context.Context, w *watermark, cand watermark) {
	if !cand.ok || !w.after(cand.At, cand.ID) {
		return
	}
	if err := r.states.save(ctx, r.topic, cand); err != nil {
		r.logger.WarnContext(ctx, "jobs: saving the watermark failed; the next sweep redelivers the settled window", "error", err)
		return
	}
	*w = cand
	r.prune(*w)
}

// prune drops the mem entries the persisted watermark now covers.
func (r *relay) prune(w watermark) {
	for id, at := range r.mem {
		if w.covers(at, id) {
			delete(r.mem, id)
		}
	}
}
