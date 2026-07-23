package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/zynthara/chok/v2/db"
)

// relayState is one relay's persisted watermark (table
// outbox_relay_state, PK relay_name). It is deliberately not a chok
// model: the row is rewritten on every advance (no append-only fit)
// and has no external identity (no RID) — like audit.Log it rides the
// battery's own migration, and all access stays inside this package
// through the sanctioned h.Unsafe hatch.
//
// (WatermarkAt, WatermarkID) is the (created_at, internal PK) position
// of the last settled row whose whole scan-order prefix is delivered.
// A row exists only after the first advance, so WatermarkAt is never
// written as the zero time (MySQL would reject it).
type relayState struct {
	RelayName   string    `gorm:"primaryKey;type:varchar(128)"`
	WatermarkAt time.Time `gorm:"not null"`
	WatermarkID uint      `gorm:"not null"`
	UpdatedAt   time.Time `gorm:"autoUpdateTime"`
}

// TableName pins the battery table name.
func (relayState) TableName() string { return "outbox_relay_state" }

// watermark is the in-process shape of a relay's persisted position.
// ok=false means "no state row yet" — scan from the beginning of the
// table. At and ID always originate from database read-backs (a
// scanned row's created_at/id or a loaded state row), never from
// Go-generated times, so comparisons stay inside one rounding domain
// (MySQL stores millisecond precision; a Go-side nanosecond value
// would compare unequal to its own round-trip).
type watermark struct {
	At time.Time
	ID uint
	ok bool
}

// covers reports whether the position (at, id) is at or before the
// watermark — i.e. already delivered in a settled prefix. Zero-value
// watermarks cover nothing.
func (w watermark) covers(at time.Time, id uint) bool {
	if !w.ok {
		return false
	}
	if at.Before(w.At) {
		return true
	}
	return at.Equal(w.At) && id <= w.ID
}

// after reports whether (at, id) advances past the watermark.
func (w watermark) after(at time.Time, id uint) bool {
	return !w.covers(at, id)
}

// stateStore reads and upserts relay watermarks.
type stateStore struct {
	h *db.DB
}

// load returns the persisted watermark for relay name (ok=false when
// no row exists yet).
func (s *stateStore) load(ctx context.Context, name string) (watermark, error) {
	var row relayState
	err := s.h.Unsafe(ctx).Where("relay_name = ?", name).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return watermark{}, nil
	}
	if err != nil {
		return watermark{}, fmt.Errorf("outbox: load relay state %q: %w", name, err)
	}
	return watermark{At: row.WatermarkAt, ID: row.WatermarkID, ok: true}, nil
}

// save upserts the watermark row for relay name. Last write wins by
// design: the scheduler scope is single-instance, and if two instances
// do race, a regressed watermark only widens the next rescan (more
// duplicates, never loss — each instance advances only over rows it
// delivered itself).
func (s *stateStore) save(ctx context.Context, name string, w watermark) error {
	if !w.ok {
		return fmt.Errorf("outbox: refusing to persist a zero watermark for relay %q", name)
	}
	row := relayState{RelayName: name, WatermarkAt: w.At, WatermarkID: w.ID}
	err := s.h.Unsafe(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "relay_name"}},
		DoUpdates: clause.AssignmentColumns([]string{"watermark_at", "watermark_id", "updated_at"}),
	}).Create(&row).Error
	if err != nil {
		return fmt.Errorf("outbox: save relay state %q: %w", name, err)
	}
	return nil
}

// minWatermark returns the oldest WatermarkAt across ALL state rows
// and the row count. The cleanup sweep deletes only below this floor —
// including rows of decommissioned relays, which therefore block
// cleanup until their state row is removed by hand (documented; the
// safe direction is to keep undelivered messages, not to guess).
func (s *stateStore) minWatermark(ctx context.Context) (time.Time, int64, error) {
	var rows []relayState
	if err := s.h.Unsafe(ctx).Find(&rows).Error; err != nil {
		return time.Time{}, 0, fmt.Errorf("outbox: list relay state: %w", err)
	}
	var min time.Time
	for i, r := range rows {
		if i == 0 || r.WatermarkAt.Before(min) {
			min = r.WatermarkAt
		}
	}
	return min, int64(len(rows)), nil
}
