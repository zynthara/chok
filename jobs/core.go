package jobs

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"time"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
)

// ErrOutsideTx is what Enqueue returns when its context does not carry a
// transaction of the database the jobs run on: the message must commit or
// roll back with the writes it describes, so an autocommit insert, or one
// riding another handle's transaction, is refused. It is a programming
// error: a handler that returns it answers 500.
var ErrOutsideTx error = outsideTxError{}

type outsideTxError struct{}

func (outsideTxError) Error() string {
	return "jobs: Enqueue needs the transaction context of db.RunInTx on the jobs' database"
}

// APIError is the 500: the request is fine, the code is not.
func (e outsideTxError) APIError() *chok.Error { return chok.ErrInternal.Wrap(e) }

// core is the storage side of the battery: the enqueue gate, the
// watermarks and the retention cleanup. Relays read through it; it owns
// no goroutines.
type core struct {
	d      *db.DB
	logger *slog.Logger
	states *stateStore
}

func newCore(d *db.DB, logger *slog.Logger) *core {
	return &core{d: d, logger: logger, states: &stateStore{d: d}}
}

// enqueue stages one message inside the caller's transaction. The gate
// checks for a transaction on this handle, not just any transaction: a
// txCtx from another handle would make the insert autocommit-adjacent —
// not atomic with the business writes — which is exactly what
// ErrOutsideTx exists to reject.
func (c *core) enqueue(ctx context.Context, topic string, payload []byte) error {
	if err := checkTopic(topic); err != nil {
		return err
	}
	if !c.d.InTx(ctx) {
		return ErrOutsideTx
	}
	if err := c.d.Unsafe(ctx).Create(&message{Topic: topic, Payload: payload}).Error; err != nil {
		return fmt.Errorf("jobs: enqueue to %q: %w", topic, err)
	}
	return nil
}

// enqueueJSON encodes v with encoding/json/v2 and enqueues it under
// topic: the same transactional contract as enqueue.
func (c *core) enqueueJSON(ctx context.Context, topic string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("jobs: encode the payload for %q: %w", topic, err)
	}
	return c.enqueue(ctx, topic, raw)
}

// cleanupOnce removes chok_jobs_messages rows below every watermark AND
// older than retention, in batches (two-step select-ids-then-delete keeps
// the statement portable and the row locks short).
//
// Which watermarks may authorise deleting a message is decided per topic
// (round-1 review — the rule exists because its absence lost messages):
// every worker registered in this process (topics) must have its OWN
// watermark row, or nothing is deleted — a bare row count let a residual
// row of a decommissioned worker stand in for a lagging worker that had
// not delivered anything yet. Residual rows of unknown topics still
// participate in the floor: they can only LOWER it (block cleanup) until
// removed by hand — the safe direction is keeping undelivered messages.
func (c *core) cleanupOnce(ctx context.Context, topics []string, retention time.Duration, batchSize int) (int64, error) {
	if len(topics) == 0 {
		return 0, nil
	}
	states, err := c.states.all(ctx)
	if err != nil {
		return 0, err
	}
	for _, topic := range topics {
		if _, ok := states[topic]; !ok {
			return 0, nil // a registered worker has no watermark yet
		}
	}
	floor := watermark{}
	for _, w := range states {
		if !floor.ok || w.At.Before(floor.At) {
			floor = w
		}
	}
	if !floor.ok {
		return 0, nil // defensive: the topics are verified above, so a floor must exist
	}
	cutoff := time.Now().Add(-retention)
	if floor.At.Before(cutoff) {
		cutoff = floor.At
	}
	cutoff = cutoff.UTC()
	gdb := c.d.Unsafe(ctx)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var ids []uint64
		if err := gdb.Model(&message{}).
			Where("created_at < ?", cutoff).
			Order("created_at").
			Limit(batchSize).
			Pluck("id", &ids).Error; err != nil {
			return total, fmt.Errorf("jobs: cleanup: select a batch: %w", err)
		}
		if len(ids) == 0 {
			return total, nil
		}
		res := gdb.Where("id IN ?", ids).Delete(&message{})
		if res.Error != nil {
			return total, fmt.Errorf("jobs: cleanup: delete a batch: %w", res.Error)
		}
		total += res.RowsAffected
		if len(ids) < batchSize {
			return total, nil
		}
	}
}
