package db

import (
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v3/internal/dbx"
)

// Open hooks each gorm callback chain once, before and after the callback
// that runs the statement, and the hooks call the observer attached to
// the handle, if any. Telemetry attaches one through dbx.Observe; as the
// hooks are in place from the start, attaching it never races the
// statements already running.

type observer struct{ o dbx.Observer }

func init() {
	dbx.Observe = func(d any, o dbx.Observer) dbx.Observed {
		h := d.(*DB)
		if o == nil {
			h.obs.Store(nil)
		} else {
			h.obs.Store(&observer{o: o})
		}
		primary, _ := h.gdb.DB()
		return dbx.Observed{Driver: h.cfg.Driver, SlowThreshold: h.cfg.SlowThreshold, Primary: primary, Readers: h.readers}
	}
}

// installObserver registers the hooks on gdb for d.
func (d *DB) installObserver(gdb *gorm.DB) error {
	before := func(op string) func(*gorm.DB) {
		return func(tx *gorm.DB) {
			if ob := d.obs.Load(); ob != nil {
				ob.o.Before(tx, op)
			}
		}
	}
	after := func(op string) func(*gorm.DB) {
		return func(tx *gorm.DB) {
			if ob := d.obs.Load(); ob != nil {
				ob.o.After(tx, op)
			}
		}
	}
	cb := gdb.Callback()
	err := errors.Join(
		cb.Create().Before("gorm:create").Register("chok:observe_before_create", before("create")),
		cb.Create().After("gorm:create").Register("chok:observe_after_create", after("create")),
		cb.Query().Before("gorm:query").Register("chok:observe_before_query", before("query")),
		cb.Query().After("gorm:query").Register("chok:observe_after_query", after("query")),
		cb.Update().Before("gorm:update").Register("chok:observe_before_update", before("update")),
		cb.Update().After("gorm:update").Register("chok:observe_after_update", after("update")),
		cb.Delete().Before("gorm:delete").Register("chok:observe_before_delete", before("delete")),
		cb.Delete().After("gorm:delete").Register("chok:observe_after_delete", after("delete")),
		cb.Row().Before("gorm:row").Register("chok:observe_before_row", before("row")),
		cb.Row().After("gorm:row").Register("chok:observe_after_row", after("row")),
		cb.Raw().Before("gorm:raw").Register("chok:observe_before_raw", before("raw")),
		cb.Raw().After("gorm:raw").Register("chok:observe_after_raw", after("raw")),
	)
	if err != nil {
		return fmt.Errorf("db: install the statement hooks: %w", err)
	}
	return nil
}
