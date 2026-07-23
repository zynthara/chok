// Package outbox is the transactional outbox battery: reliable,
// at-least-once delivery of messages written atomically with business
// data (arch-backlog #9).
//
// The EntityChanged bus (store.WithBus) is in-process and at-most-once
// — a crash between COMMIT and flush loses the event, and saturated
// subscriber queues drop. That is fine for cache invalidation and
// wrong for audit streams, projections or webhooks that must observe
// every committed write. The outbox closes the gap with two moving
// parts:
//
//   - Enqueue writes an outbox row inside the caller's RunInTx
//     transaction — the message commits or rolls back atomically with
//     the business writes it describes.
//   - A Relay (a scheduler job) scans committed rows with the
//     overlapping-watermark idiom (docs/db.md §3.5) and hands each one
//     to a user Handler, persisting its progress in outbox_relay_state
//     so a crash resumes without loss.
//
// # Delivery guarantee
//
// At-least-once, in (created_at, id) order per relay. Progress is
// persisted only after delivery succeeds, so any crash window replays
// — a Handler WILL see duplicates and must be idempotent (dedupe on a
// business key carried in the payload). Exactly-once is deliberately
// not offered: it would require the delivery target to join the
// outbox's transaction (2PC or a target-side dedup table), which is
// the consumer's trade to make, not the framework's. This is the same
// contract docs/db.md §7.2 assigns to reliable consumption.
//
// # Correctness bound: settle_window
//
// created_at is assigned when the INSERT runs; the row becomes visible
// at COMMIT. The relay therefore treats the last settle_window of time
// as unsettled and only advances its persisted watermark past rows
// older than that. The guarantee holds while every transaction that
// calls Enqueue commits within settle_window of the insert — keep
// enqueueing transactions shorter than settle_window (default 30s) or
// raise the option. The sqlite production shape is exempt (the single
// write connection serialises commits). Large clock steps backwards
// can also breach the bound (same caveat as the §3.5 watermark).
//
// # Usage
//
//	chok.Use(db.Module(...), scheduler.Module(), outbox.Module(
//	    outbox.WithRelay("projector", func(ctx context.Context, rec outbox.Record) error {
//	        return project(ctx, rec.Topic, rec.Payload)
//	    }),
//	))
//
//	// inside business code — same transaction as the writes:
//	ob := outbox.From(k)
//	err := h.RunInTx(ctx, func(txCtx context.Context) error {
//	    if err := orders.Create(txCtx, o); err != nil {
//	        return err
//	    }
//	    return ob.EnqueueJSON(txCtx, "order.created", o)
//	})
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/internal/txctx"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store"
)

// MaxTopicLen bounds Record.Topic (it is a varchar(200) column).
const MaxTopicLen = 200

var (
	// ErrOutsideTx is returned by Enqueue when ctx does not carry a
	// RunInTx transaction on the outbox's own database handle. The
	// whole point of the battery is atomicity with the business writes
	// — an autocommit insert (or one riding another handle's
	// transaction) silently loses that, so it is rejected instead.
	ErrOutsideTx = errors.New("outbox: Enqueue requires the RunInTx txCtx of the outbox's database handle")

	// ErrTopicInvalid is returned by Enqueue for an empty topic or one
	// longer than MaxTopicLen.
	ErrTopicInvalid = errors.New("outbox: topic must be non-empty and at most 200 bytes")
)

// Record is the battery's message row (table outbox_messages). It is
// an append-only model: rows are written once by Enqueue inside the
// caller's transaction and only ever read by relays; the retention
// sweep (Options.Retention) is the sole deleter.
//
// Payload is opaque bytes — chok does not pick an encoding. Handlers
// that need structure agree on one with their producers (EnqueueJSON
// is the JSON sugar).
type Record struct {
	db.AppendOnlyModel
	Topic   string `json:"topic"   gorm:"type:varchar(200);not null"`
	Payload []byte `json:"payload"`
}

// TableName pins the battery table name.
func (Record) TableName() string { return "outbox_messages" }

// Enqueuer is the produce side of the battery — what business code
// gets from From (or kernel.Get on the "outbox" component).
type Enqueuer interface {
	// Enqueue stages one message inside the caller's transaction.
	// ctx MUST be a RunInTx txCtx on the outbox's database handle;
	// anything else returns ErrOutsideTx. The message becomes visible
	// to relays only when that transaction commits.
	Enqueue(ctx context.Context, topic string, payload []byte) error

	// EnqueueJSON marshals v and enqueues it under topic. Same
	// transactional contract as Enqueue.
	EnqueueJSON(ctx context.Context, topic string, v any) error
}

// Handler consumes one delivered Record. Returning an error stops the
// current sweep before the watermark passes this row; the relay
// retries it next tick (head-of-line: later rows wait — a permanently
// failing row blocks its relay, by design, so nothing is skipped).
// Handlers run on the scheduler's clock and must respect ctx.
//
// Handlers MUST be idempotent: at-least-once delivery replays the
// unsettled window after a crash or restart.
type Handler func(ctx context.Context, rec Record) error

// HandlerFor is the Handler shape of the generic escape hatch
// (WithRelayFor): the same reliable scan-and-deliver loop over a
// user-owned append-only model.
type HandlerFor[T db.AppendModeler] func(ctx context.Context, rec T) error

// core is the assembled battery: the enqueue face plus what relays
// and the cleanup sweep need. Constructed at Component Init (or by
// tests); it owns no goroutines — relays run on the scheduler.
type core struct {
	h       *db.DB
	logger  log.Logger
	records *store.AppendStore[Record]
	states  *stateStore
}

func newCore(h *db.DB, logger log.Logger) *core {
	if logger == nil {
		logger = log.Empty()
	}
	return &core{
		h:      h,
		logger: logger,
		// Explicit query fields: the relay filters on created_at and
		// the topic scope; nothing else is queryable and construction
		// stays independent of the handle's strict-mode policy.
		records: store.NewAppend[Record](h, logger,
			store.WithQueryFields("created_at", "topic"),
			store.WithMaxPageSize(0),
		),
		states: &stateStore{h: h},
	}
}

// Enqueue implements Enqueuer.
func (c *core) Enqueue(ctx context.Context, topic string, payload []byte) error {
	if topic == "" || len(topic) > MaxTopicLen {
		return fmt.Errorf("%w (got %d bytes)", ErrTopicInvalid, len(topic))
	}
	// The gate checks for a transaction on THIS handle, not just any
	// transaction (db.InTx is deliberately handle-agnostic): a txCtx
	// from another handle would make the insert autocommit-adjacent —
	// not atomic with the business writes — which is exactly the
	// half-battery this error exists to reject.
	if txctx.DB(ctx, c.h) == nil {
		return ErrOutsideTx
	}
	return c.records.Create(ctx, &Record{Topic: topic, Payload: payload})
}

// EnqueueJSON implements Enqueuer.
func (c *core) EnqueueJSON(ctx context.Context, topic string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("outbox: marshal payload for topic %q: %w", topic, err)
	}
	return c.Enqueue(ctx, topic, raw)
}

// cleanupOnce removes outbox_messages rows below every Record relay's
// watermark AND older than retention, in batches (two-step
// select-ids-then-delete keeps the statement portable and the row
// locks short — audit's purge shape).
//
// Which watermark may authorise deleting a message is decided per NAME
// (round-1 review — both rules exist because their absence lost
// messages):
//
//   - Every registered relay that scans outbox_messages (record) must
//     have its OWN state row, or nothing is deleted — a bare row count
//     let a residual row of a decommissioned relay stand in for a
//     lagging relay that had not delivered anything yet.
//   - Watermarks of registered WithRelayFor relays (generic) are
//     excluded from the floor: they track a user-owned table, so their
//     progress says nothing about outbox_messages — with only generic
//     relays registered (record empty) nothing is deleted at all.
//     Retention for generic tables belongs to the table's owner.
//   - Residual rows of unknown names still participate in the floor:
//     they can only LOWER it (block cleanup) until removed by hand —
//     the safe direction is keeping undelivered messages.
func (c *core) cleanupOnce(ctx context.Context, record, generic []string, retention time.Duration, batchSize int) (int64, error) {
	if len(record) == 0 {
		return 0, nil
	}
	states, err := c.states.all(ctx)
	if err != nil {
		return 0, err
	}
	for _, name := range record {
		if _, ok := states[name]; !ok {
			return 0, nil // a message-scanning relay has no watermark yet
		}
	}
	genericSet := make(map[string]struct{}, len(generic))
	for _, name := range generic {
		genericSet[name] = struct{}{}
	}
	floor := watermark{}
	for name, w := range states {
		if _, isGeneric := genericSet[name]; isGeneric {
			continue
		}
		if !floor.ok || w.At.Before(floor.At) {
			floor = w
		}
	}
	if !floor.ok {
		return 0, nil // defensive: record names verified above, so a floor must exist
	}
	cutoff := time.Now().Add(-retention)
	if floor.At.Before(cutoff) {
		cutoff = floor.At
	}
	gdb := c.h.Unsafe(ctx)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var ids []uint
		if err := gdb.Model(&Record{}).
			Where("created_at < ?", cutoff).
			Order("created_at").
			Limit(batchSize).
			Pluck("id", &ids).Error; err != nil {
			return total, fmt.Errorf("outbox: cleanup select batch: %w", err)
		}
		if len(ids) == 0 {
			return total, nil
		}
		res := gdb.Where("id IN ?", ids).Delete(&Record{})
		if res.Error != nil {
			return total, fmt.Errorf("outbox: cleanup delete batch: %w", res.Error)
		}
		total += res.RowsAffected
		if len(ids) < batchSize {
			return total, nil
		}
	}
}
