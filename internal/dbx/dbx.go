// Package dbx carries what db knows about a model's tables to store and
// the batteries without widening db's public API: the SoftUnique indexes,
// which store uses to name the field of a duplicate-key error, the plain
// tables a battery declares for its own schema, and the statement
// observer telemetry attaches to a handle.
package dbx

import (
	"database/sql"
	"reflect"
	"sync"
	"time"

	"gorm.io/gorm"
)

// Index is one SoftUnique index.
type Index struct {
	Name    string
	Columns []string
}

var indexes sync.Map // reflect.Type → []Index

// SetIndexes records the SoftUnique indexes of model type t.
func SetIndexes(t reflect.Type, idx []Index) { indexes.Store(t, idx) }

// Indexes returns the SoftUnique indexes recorded for model type t.
func Indexes(t reflect.Type) []Index {
	v, _ := indexes.Load(t)
	idx, _ := v.([]Index)
	return idx
}

// PlainTable, set by package db when it initializes, declares the table
// of a model that embeds no db model base — an append-only log, or a
// schema another library defines, such as casbin_rule — for a battery's
// RegisterSchema. It returns a db.TableSpec and panics on a model that
// db.Table takes, or on one without a primary key.
var PlainTable func(model any) any

// Observer watches every statement of a database handle; telemetry is the
// one implementation. Before runs just ahead of the statement and After
// right after it, both with gorm's statement in tx; op names the gorm
// callback chain: create, query, update, delete, row or raw.
type Observer interface {
	Before(tx *gorm.DB, op string)
	After(tx *gorm.DB, op string)
}

// Observed describes the handle an Observer is attached to.
type Observed struct {
	// Driver is the dialect: sqlite, mysql or postgres.
	Driver string
	// SlowThreshold is db.slow_threshold; 0 turns the slow-query log off.
	SlowThreshold time.Duration
	// Primary is the handle's connection pool; Readers is the separate
	// read pool of a SQLite file, nil otherwise.
	Primary, Readers *sql.DB
}

// Observe, set by package db when it initializes, attaches o to the
// *db.DB d: from then on every statement of d runs o, in place of the
// observer attached before (nil detaches it). It returns what o needs to
// know about d.
var Observe func(d any, o Observer) Observed
