// Package testseq provides migration-sequence test infrastructure shared by
// chok's built-in batteries. It is internal so none of this test surface
// becomes part of the framework API.
package testseq

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/zynthara/chok/v2/db"
)

// Observation is one normalized behavior result. Keep traces limited to
// stable categories and relationships: never store generated RIDs, delete
// tokens, timestamps, driver error strings, or raw auto-increment values.
type Observation struct {
	Step     string
	Error    string
	OK       bool
	Rows     int64
	Rank     int
	JSON     string
	Business string
}

// Trace is a deterministic behavior trajectory suitable for cross-database
// and cross-migration-path comparison.
type Trace []Observation

// EqualTrace reports whether two normalized traces are identical.
func EqualTrace(left, right Trace) bool { return slices.Equal(left, right) }

// CanonicalJSON decodes and re-encodes JSON so object key order and
// dialect-specific JSON storage normalization do not affect trace equality.
func CanonicalJSON(t testing.TB, raw []byte) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("testseq: decode JSON: %v", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("testseq: encode canonical JSON: %v", err)
	}
	return string(canonical)
}

// PrefixFS copies the common three-dialect prefix ending at upTo into an
// in-memory filesystem. The caller constructs a new db.Sequence from it with
// an empty baseline; the production sequence remains opaque and unchanged.
func PrefixFS(t testing.TB, root fs.FS, upTo int64) fs.FS {
	t.Helper()
	if upTo <= 0 {
		t.Fatalf("testseq: prefix frontier must be positive, got %d", upTo)
	}
	out := fstest.MapFS{}
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		dir, err := fs.Sub(root, dialect)
		if err != nil {
			t.Fatalf("testseq: select %s migrations: %v", dialect, err)
		}
		migrations, err := db.LoadMigrations(dir)
		if err != nil {
			t.Fatalf("testseq: load %s migrations: %v", dialect, err)
		}
		copied := 0
		for _, migration := range migrations {
			if migration.Version > upTo {
				continue
			}
			raw, err := fs.ReadFile(dir, migration.File)
			if err != nil {
				t.Fatalf("testseq: read %s/%s: %v", dialect, migration.File, err)
			}
			out[dialect+"/"+migration.File] = &fstest.MapFile{Data: append([]byte(nil), raw...)}
			copied++
		}
		if copied == 0 {
			t.Fatalf("testseq: %s prefix through version %d is empty", dialect, upTo)
		}
	}
	return out
}

// UpgradeSpec supplies the data and behavior portions of an upgrade test.
// Seed and VerifyUpgrade run only on upgrade paths (P2/P3); Trace runs on all
// enabled paths and is compared with the fresh P1 trajectory.
type UpgradeSpec struct {
	Seed             func(testing.TB, *db.DB)
	VerifyUpgrade    func(testing.TB, *db.DB)
	Trace            func(testing.TB, *db.DB) Trace
	PrepareAdoptable func(testing.TB, *db.DB)
}

// RunUpgradeMatrix runs a fresh apply, an exact versioned-prefix upgrade, and
// an optional AutoMigrate-baseline adoption. Every enabled upgrade path must
// converge to the fresh owned-table fingerprint and behavior trace.
//
// A zero prefix is accepted only for a one-file full sequence. Otherwise the
// prefix must have the same identity as full and be a strict byte-identical
// migration prefix; this prevents a future migration from silently leaving P2
// disabled or a full sequence from masquerading as an N-1 frontier.
func RunUpgradeMatrix(
	t testing.TB,
	open func(testing.TB) *db.DB,
	full, prefix db.Sequence,
	spec UpgradeSpec,
) {
	t.Helper()
	if full.Kind() == "" {
		t.Fatal("testseq: full sequence is zero")
	}
	if len(full.OwnedTables()) == 0 {
		t.Fatalf("testseq: full sequence %q has no owned tables for convergence fingerprinting", full.Kind())
	}

	ctx := t.Context()
	fresh := open(t)
	fullBefore, err := db.SequenceStatus(ctx, fresh, full)
	if err != nil {
		t.Fatalf("testseq: inspect full sequence: %v", err)
	}
	if len(fullBefore.Pending) == 0 {
		t.Fatalf("testseq: full sequence %q has no migrations", full.Kind())
	}
	validatePrefix(t, fresh, full, prefix, fullBefore.Pending)

	freshReport, err := db.ApplySequence(ctx, fresh, full)
	if err != nil {
		t.Fatalf("testseq: P1 fresh apply: %v", err)
	}
	if len(freshReport.Adopted) != 0 || len(freshReport.Applied) != len(fullBefore.Pending) {
		t.Fatalf("testseq: P1 report = %+v", freshReport)
	}
	assertClean(t, "P1", fresh, full)
	freshFingerprint := fingerprint(t, "P1", fresh, full.OwnedTables())
	var freshTrace Trace
	if spec.Trace != nil {
		freshTrace = spec.Trace(t, fresh)
	}

	if prefix.Kind() != "" {
		upgraded := open(t)
		if _, err := db.ApplySequence(ctx, upgraded, prefix); err != nil {
			t.Fatalf("testseq: P2 prefix apply: %v", err)
		}
		if spec.Seed != nil {
			spec.Seed(t, upgraded)
		}
		if _, err := db.ApplySequence(ctx, upgraded, full); err != nil {
			t.Fatalf("testseq: P2 full apply: %v", err)
		}
		assertClean(t, "P2", upgraded, full)
		if spec.VerifyUpgrade != nil {
			spec.VerifyUpgrade(t, upgraded)
		}
		assertConverged(t, "P2", freshFingerprint, upgraded, full.OwnedTables())
		if spec.Trace != nil {
			assertTrace(t, "P2", freshTrace, spec.Trace(t, upgraded))
		}
	}

	if spec.PrepareAdoptable != nil {
		adoptedDB := open(t)
		spec.PrepareAdoptable(t, adoptedDB)
		assertNoSequenceMetadata(t, adoptedDB, full)
		report, err := db.ApplySequence(ctx, adoptedDB, full)
		if err != nil {
			t.Fatalf("testseq: P3 full apply: %v", err)
		}
		if len(report.Adopted) == 0 {
			t.Fatalf("testseq: P3 did not adopt a baseline: %+v", report)
		}
		for _, row := range report.Adopted {
			if row.Provenance != "baseline" {
				t.Fatalf("testseq: P3 adopted row provenance = %q, want baseline", row.Provenance)
			}
		}
		assertClean(t, "P3", adoptedDB, full)
		if spec.VerifyUpgrade != nil {
			spec.VerifyUpgrade(t, adoptedDB)
		}
		assertConverged(t, "P3", freshFingerprint, adoptedDB, full.OwnedTables())
		if spec.Trace != nil {
			assertTrace(t, "P3", freshTrace, spec.Trace(t, adoptedDB))
		}
	}
}

func validatePrefix(t testing.TB, h *db.DB, full, prefix db.Sequence, fullFiles []db.Migration) {
	t.Helper()
	if prefix.Kind() == "" {
		if len(fullFiles) != 1 {
			t.Fatalf("testseq: P2 may be skipped only for a one-file sequence; %q has %d files", full.Kind(), len(fullFiles))
		}
		return
	}
	if prefix.Kind() != full.Kind() || prefix.Ledger() != full.Ledger() || prefix.Owner() != full.Owner() {
		t.Fatalf("testseq: prefix identity (%s, %s, %s) differs from full (%s, %s, %s)",
			prefix.Kind(), prefix.Ledger(), prefix.Owner(), full.Kind(), full.Ledger(), full.Owner())
	}
	status, err := db.SequenceStatus(t.Context(), h, prefix)
	if err != nil {
		t.Fatalf("testseq: inspect prefix sequence: %v", err)
	}
	if len(status.Pending) == 0 || len(status.Pending) >= len(fullFiles) {
		t.Fatalf("testseq: prefix must be non-empty and strict: prefix=%d full=%d", len(status.Pending), len(fullFiles))
	}
	for i, migration := range status.Pending {
		want := fullFiles[i]
		if migration.Version != want.Version || migration.Name != want.Name || migration.Checksum != want.Checksum {
			t.Fatalf("testseq: prefix migration %d is %d_%s checksum=%s, want %d_%s checksum=%s",
				i, migration.Version, migration.Name, migration.Checksum, want.Version, want.Name, want.Checksum)
		}
	}
}

func assertNoSequenceMetadata(t testing.TB, h *db.DB, seq db.Sequence) {
	t.Helper()
	if h.Unsafe(t.Context()).Migrator().HasTable(seq.Ledger()) {
		t.Fatalf("testseq: P3 PrepareAdoptable left ledger %s", seq.Ledger())
	}
	entries, err := db.ManifestEntries(t.Context(), h)
	if err != nil {
		t.Fatalf("testseq: P3 inspect manifest: %v", err)
	}
	for _, entry := range entries {
		if entry.Kind == seq.Kind() {
			t.Fatalf("testseq: P3 PrepareAdoptable left manifest claim for %s", seq.Kind())
		}
	}
}

func assertClean(t testing.TB, path string, h *db.DB, seq db.Sequence) {
	t.Helper()
	status, err := db.SequenceStatus(t.Context(), h, seq)
	if err != nil || !status.Clean() {
		t.Fatalf("testseq: %s status = %+v, err=%v", path, status, err)
	}
	for _, row := range status.Applied {
		if row.Provenance != "applied" && row.Provenance != "baseline" {
			t.Fatalf("testseq: %s migration %d provenance = %q", path, row.Version, row.Provenance)
		}
	}
}

func fingerprint(t testing.TB, path string, h *db.DB, tables []string) string {
	t.Helper()
	value, err := db.SchemaFingerprint(t.Context(), h, tables)
	if err != nil {
		t.Fatalf("testseq: %s fingerprint: %v", path, err)
	}
	return value
}

func assertConverged(t testing.TB, path, fresh string, h *db.DB, tables []string) {
	t.Helper()
	if got := fingerprint(t, path, h, tables); got != fresh {
		t.Fatalf("testseq: %s schema did not converge to fresh\nfresh=%s\ngot=%s", path, fresh, got)
	}
}

func assertTrace(t testing.TB, path string, fresh, got Trace) {
	t.Helper()
	if !EqualTrace(fresh, got) {
		t.Fatalf("testseq: %s behavior differs from fresh\nfresh=%s\ngot=%s", path, formatTrace(fresh), formatTrace(got))
	}
}

func formatTrace(trace Trace) string {
	raw, err := json.Marshal(trace)
	if err != nil {
		return fmt.Sprintf("%+v", trace)
	}
	return string(raw)
}
