package outbox

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/scheduler"
	"github.com/zynthara/chok/v2/store"
	"github.com/zynthara/chok/v2/store/where"
)

// runner is what the module schedules — one per registered relay.
type runner interface {
	relayName() string
	run(ctx context.Context) error
}

// relay is the delivery engine for one named consumer: scan committed
// rows past the persisted watermark in (created_at, id) order, hand
// each to the handler, advance the watermark over the settled prefix.
// Generic so the same engine serves the battery's Record table and the
// WithRelayFor escape hatch over user-owned append models.
//
// Scanning reuses the blessed overlapping-watermark idiom (docs/db.md
// §3.5) through store.AppendStore.List — created_at >= W plus dedup —
// rather than a private SQL loop; this battery is that documented
// consumer pattern's reference implementation. Dedup has two layers:
// positions at or before the persisted watermark are settled-delivered
// (exact keyset skip — sound because the watermark never advances into
// the unsettled window where late commits could still reorder ties),
// and mem remembers rows delivered in earlier sweeps that are not yet
// settled enough to advance the watermark over.
type relay[T db.AppendModeler] struct {
	name    string
	handler HandlerFor[T]
	scan    *store.AppendStore[T]
	states  *stateStore
	h       *db.DB // frontier probe (the one query the scan store cannot express)
	logger  log.Logger

	settle   time.Duration
	batch    func() int       // live snapshot (hot-reloadable option)
	now      func() time.Time // injectable clock for settle tests
	filtered bool             // OnTopics relay: watermark may jump the settled frontier
	maxPages int              // one sweep's page budget (maxSweepPages; test-tunable)

	base []int // FieldByIndex path to the embedded AppendOnlyModel

	// mu guards mem. Sweeps are serialised by PolicySkipIfRunning on
	// the cron path, but scheduler.RunNow bypasses the policy chain —
	// TryLock turns that overlap into ErrBusy instead of a race.
	mu  sync.Mutex
	mem map[uint]time.Time // delivered but not yet settled: id → created_at
}

// scanPos is the in-sweep cursor: the created_at boundary plus how
// many rows at or after it were already consumed from the result.
// OFFSET-on-boundary (not a bare LIMIT re-query) is what makes a tie
// group larger than one batch progress instead of spinning in place.
type scanPos struct {
	ts     time.Time
	offset int
}

func newRelay[T db.AppendModeler](name string, handler HandlerFor[T], c *core, cfg relayCfg, settle time.Duration, batch func() int) (*relay[T], error) {
	if name == "" || len(name) > 128 {
		return nil, fmt.Errorf("outbox: relay name must be non-empty and at most 128 bytes, got %q", name)
	}
	if handler == nil {
		return nil, fmt.Errorf("outbox: relay %q has a nil handler", name)
	}
	var zero T
	base := appendBasePath(reflect.TypeOf(zero))
	if base == nil {
		// Unreachable in practice: store.NewAppend below panics first
		// on models without the base. Kept as a defensive error.
		return nil, fmt.Errorf("outbox: relay %q model %T does not embed db.AppendOnlyModel", name, zero)
	}
	scanOpts := []store.StoreOption{
		store.WithQueryFields("created_at"),
		store.WithMaxPageSize(0),
	}
	if len(cfg.topics) > 0 {
		scanOpts = append(scanOpts, store.WithScope(topicScope(cfg.topics)))
	}
	return &relay[T]{
		name:     name,
		handler:  handler,
		scan:     store.NewAppend[T](c.h, c.logger, scanOpts...),
		states:   c.states,
		h:        c.h,
		logger:   c.logger.With("relay", name),
		settle:   settle,
		batch:    batch,
		now:      time.Now,
		filtered: len(cfg.topics) > 0,
		maxPages: maxSweepPages,
		base:     base,
		mem:      make(map[uint]time.Time),
	}, nil
}

// maxSweepPages bounds one sweep. The bound is what keeps the fixed
// pre-scan cutoff compatible with sustained production: a sweep that
// could loop forever would hold one cutoff forever (the round-2 #1
// memory pathology); capped sweeps end, and the next tick rescans the
// overlap from the persisted watermark under a fresh cutoff, so rows
// settle and mem prunes across sweeps. With the default batch size
// this is 10k rows per sweep per relay — delivery throughput is paced,
// correctness is not affected (at-least-once holds at any budget).
const maxSweepPages = 100

func (r *relay[T]) relayName() string { return r.name }

// run is one sweep. It returns scheduler.ErrBusy when another sweep is
// still in flight (RunNow racing the cron trigger); any other error
// aborted the sweep with the watermark parked before the failed row.
func (r *relay[T]) run(ctx context.Context) error {
	if !r.mu.TryLock() {
		return scheduler.ErrBusy
	}
	defer r.mu.Unlock()

	w, err := r.states.load(ctx, r.name)
	if err != nil {
		return err
	}
	// Prune against the loaded watermark before scanning: another
	// instance (the accepted last-write-wins degradation) may have
	// advanced the shared state past entries this instance still holds
	// in mem — without this they would outlive every local advance
	// (round-2 review).
	r.prune(w)
	cand := w
	pos := scanPos{ts: w.At}

	// Rows younger than the cutoff are unsettled: a still-open
	// transaction may yet commit a row with an earlier created_at, so
	// the persisted watermark must not advance past them. Delivery is
	// NOT delayed — unsettled rows are handed out immediately and
	// remembered in mem; settle only gates watermark advancement.
	//
	// The cutoff is taken ONCE, before the scan (round-3 review —
	// restoring this closed a Critical): covering a position is safe
	// only when its created_at predates the moment the CURSOR PASSED
	// that position by settle — an invisible row there can otherwise
	// still commit (within its own settle budget) after the pass, and
	// the cursor never returns within this sweep. Sweep start lower-
	// bounds every pass time, so the pre-scan cutoff is sound; the
	// round-2 per-batch refresh was not ("cutoff predates persist"
	// ignores pass times) — it could settle positions passed while a
	// perfectly legal transaction was still invisible, skipping its
	// row forever. The endless-sweep memory concern that refresh
	// addressed is handled by maxSweepPages instead: the next tick
	// takes a fresh cutoff and rescans the overlap from the persisted
	// watermark, so entries settle and prune across sweeps.
	cutoff := r.now().Add(-r.settle)
	drained := false
	for pages := 0; pages < r.maxPages; pages++ {
		if err := ctx.Err(); err != nil {
			r.persist(ctx, &w, cand)
			return err
		}
		batch := r.batch()
		items, err := r.scanFrom(ctx, pos, batch)
		if err != nil {
			r.persist(ctx, &w, cand)
			return err
		}
		if len(items) == 0 {
			drained = true
			break
		}
		for i := range items {
			at, id := r.rowPos(&items[i])
			if at.Equal(pos.ts) {
				pos.offset++
			} else {
				pos = scanPos{ts: at, offset: 1}
			}
			delivered := w.covers(at, id)
			if !delivered {
				if _, seen := r.mem[id]; seen {
					delivered = true
				}
			}
			if !delivered {
				if err := r.handler(ctx, items[i]); err != nil {
					r.persist(ctx, &w, cand)
					return fmt.Errorf("outbox: relay %q: deliver row (created_at=%s, id=%d): %w",
						r.name, at.Format(time.RFC3339Nano), id, err)
				}
				r.mem[id] = at
			}
			// Everything up to and including this row is delivered
			// (head-of-line: a failure returns above, so the prefix is
			// contiguous). Settled rows become the next watermark.
			if !at.After(cutoff) {
				cand = watermark{At: at, ID: id, ok: true}
			}
		}
		// Per-batch persistence bounds the crash replay window during
		// long catch-up sweeps.
		r.persist(ctx, &w, cand)
		if len(items) < batch {
			drained = true
			break
		}
	}
	// A topic-filtered relay only sees matching rows, so a quiet topic
	// would never advance its watermark — leaving no state row (or a
	// stale one) that blocks the retention sweep forever (round-2
	// review). Once the filtered scan is DRAINED, every matching row up
	// to the settled frontier is delivered, so the watermark may jump
	// over the foreign-topic rows to that frontier. Drained clean exits
	// only: handler failures returned above (head-of-line intact) and a
	// budget-stopped sweep proves nothing about rows it never reached.
	// The probe reuses the pre-scan cutoff for the same pass-time
	// reason as the loop: a probe-time cutoff could settle a match
	// that committed after the filtered cursor passed its position.
	if drained && r.filtered {
		f, err := r.frontier(ctx, cutoff)
		if err != nil {
			r.logger.Warn("outbox: frontier probe failed — retention floor advances next sweep", "error", err)
		} else if f.ok && cand.after(f.At, f.ID) {
			cand = f
		}
	}
	r.persist(ctx, &w, cand)
	return nil
}

// frontier returns the newest (created_at, id) position at or before
// cutoff across the WHOLE table — the highest watermark a drained
// relay may claim. cutoff MUST be the sweep's pre-scan cutoff (see
// run). It rides the sanctioned Unsafe hatch for the one shape the
// where DSL cannot express (descending id tie-pick with LIMIT 1); the
// probe only runs for topic-filtered relays, i.e. always against the
// battery's own Record table with its default column names.
func (r *relay[T]) frontier(ctx context.Context, cutoff time.Time) (watermark, error) {
	var row T
	err := r.h.Unsafe(ctx).Model(new(T)).
		Where("created_at <= ?", cutoff).
		Order("created_at DESC, id DESC").
		Limit(1).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return watermark{}, nil
	}
	if err != nil {
		return watermark{}, fmt.Errorf("outbox: relay %q: settled frontier: %w", r.name, err)
	}
	at, id := r.rowPos(&row)
	return watermark{At: at, ID: id, ok: true}, nil
}

// scanFrom fetches one batch at the cursor: created_at >= pos.ts (the
// Gte overlap of §3.5 — the zero boundary means "from the beginning"
// and adds no filter), ordered (created_at, id) by AppendStore's
// deterministic-order guarantee, skipping the pos.offset rows already
// consumed at this boundary.
func (r *relay[T]) scanFrom(ctx context.Context, pos scanPos, batch int) ([]T, error) {
	// Option order matters: WithPage(1, batch) contributes the LIMIT,
	// then WithOffset overrides its zero offset (the DSL has no
	// standalone limit option).
	opts := []where.Option{where.WithPage(1, batch), where.WithOffset(pos.offset)}
	if !pos.ts.IsZero() {
		opts = append(opts, where.WithFilterOp("created_at", where.Gte, pos.ts))
	}
	page, err := r.scan.List(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("outbox: relay %q: scan: %w", r.name, err)
	}
	return page.Items, nil
}

// persist advances the stored watermark to cand when it moved, then
// prunes mem entries the new watermark covers. Pruning rides every
// advance — batch boundaries and the pre-return saves on failure paths
// alike — so mem stays bounded by the unsettled window instead of the
// whole backlog processed by a long catch-up sweep (round-1 review:
// an end-of-sweep-only prune leaked every settled entry when a later
// row's handler error returned early, and grew without bound while a
// sweep never caught up).
//
// Save errors are logged, not returned: a failed save costs
// redelivery (the next sweep rescans from the older watermark), never
// loss.
func (r *relay[T]) persist(ctx context.Context, w *watermark, cand watermark) {
	if !cand.ok || !w.after(cand.At, cand.ID) {
		return
	}
	if err := r.states.save(ctx, r.name, cand); err != nil {
		r.logger.Warn("outbox: watermark save failed — next sweep will redeliver the settled window", "error", err)
		return
	}
	*w = cand
	r.prune(*w)
}

// prune drops mem entries the persisted watermark now covers.
func (r *relay[T]) prune(w watermark) {
	for id, at := range r.mem {
		if w.covers(at, id) {
			delete(r.mem, id)
		}
	}
}

// rowPos reads the (created_at, id) position off the embedded base.
func (r *relay[T]) rowPos(row *T) (time.Time, uint) {
	base := reflect.ValueOf(row).Elem().FieldByIndex(r.base).Interface().(db.AppendOnlyModel)
	return base.CreatedAt, base.ID
}

// appendBaseType identifies db.AppendOnlyModel by type, so alias
// embeds (type Base = db.AppendOnlyModel) resolve like the plain form.
var appendBaseType = reflect.TypeOf(db.AppendOnlyModel{})

// appendBasePath returns the FieldByIndex path to the embedded
// db.AppendOnlyModel, walking value-typed anonymous fields (the only
// legal embed shape — ValidateAppendModel rejects the rest).
func appendBasePath(t reflect.Type) []int {
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.Anonymous || f.Type.Kind() != reflect.Struct {
			continue
		}
		if f.Type == appendBaseType {
			return []int{i}
		}
		if sub := appendBasePath(f.Type); sub != nil {
			return append([]int{i}, sub...)
		}
	}
	return nil
}
