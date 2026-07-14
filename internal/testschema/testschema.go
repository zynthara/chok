// Package testschema contains repository-internal assertions that compare a
// component's schema declaration with the tables created by its real kernel
// migration lifecycle.
package testschema

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	AssertOwnershipForMode(t, h, component, db.MigrateVersioned)
}

// AssertOwnershipForMode compares actual tables with the component's potential
// ownership declaration. Owned migration ledgers are present only in
// versioned mode; auto and off therefore exclude them from the expected set.
func AssertOwnershipForMode(t testing.TB, h *db.DB, component kernel.Component, mode string) {
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
		if component.Describe().Kind != "db" && table == "schema_migrations" {
			continue // owned by the required db component, not this battery
		}
		if component.Describe().Kind != "db" && table == "schema_migrations_chok_manifest" {
			continue // shared db infrastructure, not the sequence-owning component
		}
		actual = append(actual, table)
	}
	expected := make([]string, 0, len(component.Describe().Schema.Tables))
	for _, table := range component.Describe().Schema.Tables {
		if mode != db.MigrateVersioned && strings.HasPrefix(table, "schema_migrations_chok_") {
			continue
		}
		// The db component owns the manifest, but application-only databases
		// do not create it until their first owned sequence is applied.
		if table == "schema_migrations_chok_manifest" && !slices.Contains(actual, table) {
			continue
		}
		expected = append(expected, table)
	}
	sort.Strings(actual)
	sort.Strings(expected)
	if !slices.Equal(actual, expected) {
		t.Fatalf("%s schema ownership mismatch: declared=%v actual=%v", component.Describe().Kind, expected, actual)
	}
}

// UpdateBaselineIfRequested rewrites the calling package's embedded baseline
// fingerprint from the live AutoMigrate schema when CHOK_UPDATE_BASELINES=1
// is set, then skips the test: fingerprints are embedded at build time, so
// the stale compiled-in copy must not fail the regeneration run.
// Regeneration is therefore a two-pass flow — run the equivalence tests once
// with the variable set (each reachable lane writes its own dialect file
// under migrations/baseline/), then rerun without it so the gates verify the
// result. Without the variable this is a no-op.
//
// It is a local maintainer flow by construction: any value other than
// exactly "1" fails loudly instead of silently green-skipping the
// equivalence gates, and it refuses to run when the CI environment
// variable is present.
func UpdateBaselineIfRequested(t testing.TB, fingerprint string) {
	t.Helper()
	switch value := os.Getenv("CHOK_UPDATE_BASELINES"); value {
	case "":
		return
	case "1":
	default:
		t.Fatalf("testschema: CHOK_UPDATE_BASELINES must be unset or exactly %q, got %q — refusing to guess", "1", value)
	}
	if os.Getenv("CI") != "" {
		t.Fatal("testschema: baseline regeneration is a local maintainer flow — refusing to rewrite fingerprints under CI")
	}
	path, err := baselineUpdateTarget(fingerprint)
	if err != nil {
		t.Fatalf("testschema: %v", err)
	}
	if err := os.WriteFile(path, []byte(fingerprint+"\n"), 0o644); err != nil {
		t.Fatalf("testschema: rewrite %s: %v", path, err)
	}
	t.Skipf("testschema: %s rewritten from the live AutoMigrate schema — rerun without CHOK_UPDATE_BASELINES to verify", path)
}

// baselineUpdateTarget validates the fingerprint's dialect header against
// the blessed dialect set and derives the baseline file it may rewrite.
// The allowlist doubles as path-traversal protection: the dialect value
// never reaches the filesystem unless it is one of the three known names.
func baselineUpdateTarget(fingerprint string) (string, error) {
	var head struct {
		Dialect string `json:"dialect"`
	}
	if err := json.Unmarshal([]byte(fingerprint), &head); err != nil {
		return "", fmt.Errorf("fingerprint is not a catalog snapshot: %w", err)
	}
	switch head.Dialect {
	case "sqlite", "mysql", "postgres":
		return filepath.Join("migrations", "baseline", head.Dialect+".json"), nil
	default:
		return "", fmt.Errorf("fingerprint dialect %q is not one of sqlite|mysql|postgres", head.Dialect)
	}
}
