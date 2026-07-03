// Package choktest provides test utilities for applications built on chok.
//
// It eliminates the boilerplate of setting up in-memory databases, loggers,
// and stores that recurs in every test file. Typical usage:
//
//	func TestMyFeature(t *testing.T) {
//	    h := choktest.NewTestDB(t, &model.User{}, &model.Post{})
//	    s := store.New[model.Post](h, choktest.NopLogger())
//	    // ... test against s ...
//	}
package choktest

import (
	"context"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store"
)

// NewTestDB opens an in-memory SQLite database, auto-migrates all
// provided models, and registers a cleanup that closes the DB when the
// test finishes. Fails the test on any error.
//
// v2 returns the thin *db.DB handle (the store.New input); raw gorm
// access, when a test truly needs it, is h.Unsafe(ctx).
func NewTestDB(t *testing.T, models ...any) *db.DB {
	t.Helper()
	h, err := db.Open(db.Options{
		Driver: "sqlite",
		SQLite: db.SQLiteOptions{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("choktest: open sqlite: %v", err)
	}
	if len(models) > 0 {
		// Raw AutoMigrate (not db.Table specs): test models aren't
		// required to satisfy the framework's model validation.
		if err := h.Unsafe(context.Background()).AutoMigrate(models...); err != nil {
			t.Fatalf("choktest: auto migrate: %v", err)
		}
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
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
	h := NewTestDB(t, zero)
	return store.New[T](h, NopLogger(), opts...)
}
