// Package choktest provides test utilities for applications built on chok.
//
// It eliminates the boilerplate of setting up in-memory databases, loggers,
// and stores that recurs in every test file. Typical usage:
//
//	func TestMyFeature(t *testing.T) {
//	    gdb := choktest.NewTestDB(t, &model.User{}, &model.Post{})
//	    s := store.New[model.Post](gdb, choktest.NopLogger())
//	    // ... test against s ...
//	}
package choktest

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/store"
)

// NewTestDB opens an in-memory SQLite database, auto-migrates all
// provided models, and registers a cleanup that closes the DB when the
// test finishes. Fails the test on any error.
func NewTestDB(t *testing.T, models ...any) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("choktest: open sqlite: %v", err)
	}
	if len(models) > 0 {
		if err := gdb.AutoMigrate(models...); err != nil {
			t.Fatalf("choktest: auto migrate: %v", err)
		}
	}
	t.Cleanup(func() { db.Close(gdb) })
	return gdb
}

// NopLogger returns a no-op logger suitable for test usage where log
// output should be suppressed.
func NopLogger() log.Logger { return log.Empty() }

// NewTestStore creates a Store[T] backed by an in-memory SQLite DB
// with the model auto-migrated. This is the fastest path for unit-
// testing Store-backed business logic.
func NewTestStore[T db.Modeler](t *testing.T, opts ...store.StoreOption) *store.Store[T] {
	t.Helper()
	var zero T
	gdb := NewTestDB(t, zero)
	return store.New[T](gdb, NopLogger(), opts...)
}
