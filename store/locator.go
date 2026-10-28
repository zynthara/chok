package store

import (
	"fmt"
	"strconv"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v3/store/where"
)

// Locator picks the row, or rows, an operation targets.
type Locator interface {
	apply(s locatorTarget, tx *gorm.DB) (*gorm.DB, error)
	String() string
}

// locatorTarget is what a locator needs from its store.
type locatorTarget interface {
	table() string
	fields() map[string]string
}

// RID locates by public ID.
func RID(rid string) Locator { return ridLocator(rid) }

// ID locates by numeric primary key.
func ID(id uint) Locator { return idLocator(id) }

// Where locates by filters; at least one is required, so an update or
// delete never targets every row by accident (MissingConditionsError).
func Where(opts ...where.Option) Locator { return whereLocator(opts) }

type ridLocator string

func (r ridLocator) apply(s locatorTarget, tx *gorm.DB) (*gorm.DB, error) {
	return tx.Where(s.table()+".rid = ?", string(r)), nil
}

func (r ridLocator) String() string { return "rid " + string(r) }

type idLocator uint

func (i idLocator) apply(s locatorTarget, tx *gorm.DB) (*gorm.DB, error) {
	return tx.Where(s.table()+".id = ?", uint(i)), nil
}

func (i idLocator) String() string { return "id " + strconv.FormatUint(uint64(i), 10) }

type whereLocator []where.Option

func (w whereLocator) apply(s locatorTarget, tx *gorm.DB) (*gorm.DB, error) {
	cfg := &where.Config{FiltersOnly: true}
	var err error
	for _, o := range w {
		if tx, err = o(tx, cfg, s.fields()); err != nil {
			return nil, err
		}
	}
	if !cfg.HasFilter {
		return nil, &MissingConditionsError{}
	}
	return tx, nil
}

func (w whereLocator) String() string { return fmt.Sprintf("where (%d conditions)", len(w)) }

func (s *Store[T]) table() string             { return s.schema.Table }
func (s *Store[T]) fields() map[string]string { return s.columns }
