package jobs

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/zynthara/chok/v3"
)

// The page size of Dead.
const (
	defaultDeadLimit = 100
	maxDeadLimit     = 1000
)

// deadChunk bounds the ids one statement of Requeue or Discard names.
const deadChunk = 500

// Dead returns the messages parked in the dead letters under topic, or
// under every topic for "", the longest parked first: at most limit of
// them, 1 to 1000, or 100 for 0. It reads only the database, so it works
// in a process whose Config.Enabled is false.
func (j *Jobs) Dead(ctx context.Context, topic string, limit int) ([]DeadMessage, error) {
	switch {
	case limit == 0:
		limit = defaultDeadLimit
	case limit < 0 || limit > maxDeadLimit:
		return nil, fmt.Errorf("jobs: Dead: limit must be between 1 and %d (0 takes %d), got %d", maxDeadLimit, defaultDeadLimit, limit)
	}
	q := j.core.d.Unsafe(ctx).Order("parked_at").Order("id").Limit(limit)
	if topic != "" {
		q = q.Where("topic = ?", topic)
	}
	var rows []deadRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("jobs: Dead: %w", err)
	}
	out := make([]DeadMessage, len(rows))
	for i := range rows {
		out[i] = rows[i].toDead()
	}
	return out, nil
}

// Requeue enqueues each parked message of ids again, under its topic and
// with its payload, as a new message — a new ID, created now — and drops
// it from the dead letters, all in one transaction; the new messages
// keep the order the parked ones were first enqueued in. An id that is
// not parked fails the call, naming it, and nothing changes: a handler
// that returns that error answers 404. Like Dead it works in a process
// whose Config.Enabled is false.
func (j *Jobs) Requeue(ctx context.Context, ids ...uint64) error {
	return j.takeDead(ctx, "Requeue", ids, func(txCtx context.Context, rows []deadRow) error {
		slices.SortFunc(rows, func(a, b deadRow) int {
			return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
		})
		tx := j.core.d.Unsafe(txCtx)
		for i := range rows {
			if err := tx.Create(&message{Topic: rows[i].Topic, Payload: rows[i].Payload}).Error; err != nil {
				return fmt.Errorf("jobs: Requeue: enqueue message %d again: %w", rows[i].ID, err)
			}
		}
		return nil
	})
}

// Discard deletes the parked messages of ids from the dead letters. An id
// that is not parked fails the call, naming it, and nothing changes: a
// handler that returns that error answers 404. Like Dead it works in a
// process whose Config.Enabled is false.
func (j *Jobs) Discard(ctx context.Context, ids ...uint64) error {
	return j.takeDead(ctx, "Discard", ids, nil)
}

// takeDead deletes the dead rows of ids in a transaction and hands them
// to then, inside it. Every id must be parked; a row another call took
// meanwhile fails the transaction too.
func (j *Jobs) takeDead(ctx context.Context, op string, ids []uint64, then func(txCtx context.Context, rows []deadRow) error) error {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 0 {
		return nil
	}
	return j.core.d.RunInTx(ctx, func(txCtx context.Context) error {
		tx := j.core.d.Unsafe(txCtx)
		var rows []deadRow
		for chunk := range slices.Chunk(ids, deadChunk) {
			var part []deadRow
			if err := tx.Where("id IN ?", chunk).Find(&part).Error; err != nil {
				return fmt.Errorf("jobs: %s: read the dead letters: %w", op, err)
			}
			rows = append(rows, part...)
		}
		if len(rows) < len(ids) {
			found := make(map[uint64]bool, len(rows))
			for _, r := range rows {
				found[r.ID] = true
			}
			var missing []uint64
			for _, id := range ids {
				if !found[id] {
					missing = append(missing, id)
				}
			}
			return notParkedError{op: op, ids: missing}
		}
		var deleted int64
		for chunk := range slices.Chunk(ids, deadChunk) {
			res := tx.Where("id IN ?", chunk).Delete(&deadRow{})
			if res.Error != nil {
				return fmt.Errorf("jobs: %s: delete from the dead letters: %w", op, res.Error)
			}
			deleted += res.RowsAffected
		}
		if deleted != int64(len(ids)) {
			return fmt.Errorf("jobs: %s: another call took some of these dead messages meanwhile; nothing changed", op)
		}
		if then == nil {
			return nil
		}
		return then(txCtx, rows)
	})
}

// notParkedError names the ids Requeue or Discard found no parked message
// for. It answers 404.
type notParkedError struct {
	op  string
	ids []uint64
}

func (e notParkedError) Error() string {
	return fmt.Sprintf("jobs: %s: no dead message with id %s; nothing changed", e.op, joinIDs(e.ids))
}

// APIError is the 404, naming the ids the caller gave.
func (e notParkedError) APIError() *chok.Error {
	return chok.ErrNotFound.Msgf("no dead message with id %s", joinIDs(e.ids)).Wrap(e)
}

func joinIDs(ids []uint64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(id, 10)
	}
	return strings.Join(parts, ", ")
}
