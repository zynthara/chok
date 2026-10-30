package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/zynthara/chok/db"
)

// watermark is the in-process shape of a worker's persisted position.
// ok=false means "no state row yet": scan from the beginning of the
// table. At and ID always originate from database read-backs (a scanned
// row's created_at/id or a loaded state row), never from Go-generated
// times, so comparisons stay inside one rounding domain (MySQL stores
// millisecond precision; a Go-side nanosecond value would compare unequal
// to its own round-trip).
type watermark struct {
	At time.Time
	ID uint64
	ok bool
}

// covers reports whether the position (at, id) is at or before the
// watermark — i.e. already delivered in a settled prefix. Zero-value
// watermarks cover nothing.
func (w watermark) covers(at time.Time, id uint64) bool {
	if !w.ok {
		return false
	}
	if at.Before(w.At) {
		return true
	}
	return at.Equal(w.At) && id <= w.ID
}

// after reports whether (at, id) advances past the watermark.
func (w watermark) after(at time.Time, id uint64) bool {
	return !w.covers(at, id)
}

// stateStore reads and upserts the workers' watermarks.
type stateStore struct {
	d *db.DB
}

// load returns the persisted watermark of the worker of topic
// (ok=false when no row exists yet).
func (s *stateStore) load(ctx context.Context, topic string) (watermark, error) {
	var row watermarkRow
	err := s.d.Unsafe(ctx).Where("topic = ?", topic).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return watermark{}, nil
	}
	if err != nil {
		return watermark{}, fmt.Errorf("jobs: load the watermark of %q: %w", topic, err)
	}
	return watermark{At: row.WatermarkAt, ID: row.WatermarkID, ok: true}, nil
}

// save upserts the watermark row of the worker of topic. Last write wins
// by design: workers belong to one process (Config.Enabled), and if two
// processes do race, a regressed watermark only widens the next rescan
// (more duplicates, never loss — each process advances only over rows it
// delivered itself).
func (s *stateStore) save(ctx context.Context, topic string, w watermark) error {
	if !w.ok {
		return fmt.Errorf("jobs: refusing to persist a zero watermark for %q", topic)
	}
	row := watermarkRow{Topic: topic, WatermarkAt: w.At.UTC(), WatermarkID: w.ID}
	err := s.d.Unsafe(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "topic"}},
		DoUpdates: clause.AssignmentColumns([]string{"watermark_at", "watermark_id", "updated_at"}),
	}).Create(&row).Error
	if err != nil {
		return fmt.Errorf("jobs: save the watermark of %q: %w", topic, err)
	}
	return nil
}

// all returns every persisted watermark keyed by topic. The cleanup
// decides per topic what each row may authorise (round-1 review: a bare
// count/min over the table let residual rows stand in for lagging
// workers).
func (s *stateStore) all(ctx context.Context) (map[string]watermark, error) {
	var rows []watermarkRow
	if err := s.d.Unsafe(ctx).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("jobs: list the watermarks: %w", err)
	}
	out := make(map[string]watermark, len(rows))
	for _, r := range rows {
		out[r.Topic] = watermark{At: r.WatermarkAt, ID: r.WatermarkID, ok: true}
	}
	return out, nil
}
