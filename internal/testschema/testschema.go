// Package testschema contains repository-internal assertions that compare a
// component's schema declaration with the tables created by its real kernel
// migration lifecycle.
package testschema

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/kernel"
)

// AssertOwnership fails when the database's user-created table set differs
// from the component's Descriptor.Schema declaration. Each assertion must run
// against a fresh database containing only that component's owned schema.
func AssertOwnership(t testing.TB, h *db.DB, component kernel.Component) {
	t.Helper()
	tables, err := h.Unsafe(context.Background()).Migrator().GetTables()
	if err != nil {
		t.Fatalf("inspect database tables: %v", err)
	}
	actual := make([]string, 0, len(tables))
	for _, table := range tables {
		// SQLite creates sqlite_sequence for AUTOINCREMENT models; it is an
		// engine-owned implementation table, not component schema.
		if strings.HasPrefix(table, "sqlite_") {
			continue
		}
		actual = append(actual, table)
	}
	expected := append([]string(nil), component.Describe().Schema.Tables...)
	sort.Strings(actual)
	sort.Strings(expected)
	if !slices.Equal(actual, expected) {
		t.Fatalf("%s schema ownership mismatch: declared=%v actual=%v", component.Describe().Kind, expected, actual)
	}
}
