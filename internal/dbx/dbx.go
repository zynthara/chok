// Package dbx carries what db knows about a model's tables to store and
// the batteries without widening db's public API: the indexes db.Table
// declares, SoftUnique and Unique, which store uses to name the field of
// a duplicate-key error and to resolve an upsert's conflict target, the
// plain tables a battery declares for its own schema, the statement
// observer telemetry attaches to a handle, and the batteries' migration
// files the chok command applies.
package dbx

import (
	"database/sql"
	"io/fs"
	"reflect"
	"time"

	"gorm.io/gorm"
)

// Migrations maps the ledger owner of each battery schema — identity,
// rbac, jobs — to the versioned SQL files the battery registers with
// RegisterSchema. Each battery package adds its entry when it
// initializes, so the chok command, which links them, can report and
// apply a battery's migrations without the battery exporting them. It is
// written only during package initialization.
var Migrations = map[string]fs.FS{}

// Index is one index db.Table declares: a SoftUnique or a Unique.
type Index struct {
	Name    string
	Columns []string
	// Soft marks a SoftUnique, UNIQUE(Columns..., delete_token), whose
	// upsert conflict target gains delete_token; a Unique is
	// UNIQUE(Columns...), and Columns are its target as they are.
	Soft bool
}

// Indexes returns the index metadata of model type t on a database
// handle, SoftUnique and Unique alike. Set by db at initialization; no
// model metadata is shared across handles.
var Indexes func(d any, t reflect.Type) []Index

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

// Describe reads immutable metadata without publishing an observer.
var Describe func(d any) Observed
