// Package dbx carries what db knows about a model's tables to store
// without widening db's public API: the SoftUnique indexes, which store
// uses to name the field of a duplicate-key error.
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
