// Package dbx carries what db knows about a model's tables to store and
// the batteries without widening db's public API: the SoftUnique indexes,
// which store uses to name the field of a duplicate-key error, and the
// plain tables a battery declares for its own schema.
package dbx

import (
	"reflect"
	"sync"
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
