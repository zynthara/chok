package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

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
type relay struct {
	topic   string
	handler func(ctx context.Context, m Message) error
	states  *stateStore
	d       *db.DB
	logger  *slog.Logger

	settle   time.Duration
	batch    int
	now      func() time.Time // injectable clock for settle tests
	maxPages int              // one sweep's page budget (maxSweepPages; test-tunable)

	delivered atomic.Int64 // messages handed to the handler, for Stats

	// mu guards mem. The scheduler never overlaps a worker's sweeps; the
	// TryLock turns an overlap into an error instead of a race.
	mu  sync.Mutex
	mem map[uint64]time.Time // delivered but not yet settled: id → created_at
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

func newRelay(topic string, handler func(ctx context.Context, m Message) error, c *core, settle time.Duration, batch int) (*relay, error) {
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

// run is one sweep. Any error aborted the sweep with the watermark parked
// before the failed row.
func (r *relay) run(ctx context.Context) error {
	if !r.mu.TryLock() {
		return errSweepRunning
	}
	defer r.mu.Unlock()

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
				if err := r.deliver(ctx, &page[i], at, id, w, &cand, cutoff); err != nil {
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
			if err := r.deliver(ctx, &items[i], at, id, w, &cand, cutoff); err != nil {
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
// then process memory), the handler, and the settled-candidate advance.
// Everything up to and including this row is delivered when it returns
// nil (head-of-line: an error aborts the sweep above, so the prefix stays
// contiguous).
func (r *relay) deliver(ctx context.Context, row *message, at time.Time, id uint64, w watermark, cand *watermark, cutoff time.Time) error {
	delivered := w.covers(at, id)
	if !delivered {
		if _, seen := r.mem[id]; seen {
			delivered = true
		}
	}
	if !delivered {
		if err := r.handler(ctx, row.toMessage()); err != nil {
			return fmt.Errorf("jobs: worker %q: deliver message %d (created_at %s): %w",
				r.topic, id, at.UTC().Format(time.RFC3339Nano), err)
		}
		r.delivered.Add(1)
		r.mem[id] = at
	}
	if !at.After(cutoff) {
		*cand = watermark{At: at, ID: id, ok: true}
	}
	return nil
}

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
