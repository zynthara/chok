// Package dbtest is the shared test-support opener behind the M3
// SQLite+Postgres dual-run (SPEC §5.3 / §12.6 budget: the database
// matrix covers the store/db packages only).
//
// Driver selection is environment-driven so the same test binaries run
// both lanes:
//
//	(default)                     — in-memory SQLite
//	CHOK_TEST_DRIVER=postgres     — Postgres at CHOK_TEST_PG_DSN;
//	                                skipped when the DSN is unset
//	                                (local machines without PG)
//
// Each Postgres-lane test gets its own throwaway schema (search_path
// pinned via DSN, dropped in cleanup), so tests never see each other's
// tables even against one shared database.
//
// Open returns the v2 thin handle (*db.DB) so tests outside db/ never
// touch gorm; raw SQL assertions ride h.Unsafe(ctx), the sanctioned
// escape hatch. Lane provisioning lives in db/internal/testlane so
// db's own internal tests share it without an import cycle.
package dbtest

import (
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/internal/testlane"
)

// Env variable names (also read by CI).
const (
	DriverEnv = testlane.DriverEnv
	PGDSNEnv  = testlane.PGDSNEnv
)

// Driver reports the lane the current process runs in: "sqlite"
// (default) or "postgres".
func Driver() string { return testlane.Driver() }

// Open returns a fresh thin handle for the active lane, isolated per
// test and cleaned up automatically. Unknown drivers fail the test;
// the postgres lane skips when CHOK_TEST_PG_DSN is unset. Construction
// rides the public db.Open path so tests exercise the same opener as
// production.
func Open(t testing.TB) *db.DB {
	t.Helper()
	var (
		h   *db.DB
		err error
	)
	switch testlane.Driver() {
	case "sqlite":
		h, err = db.Open(db.Options{Driver: "sqlite", SQLite: db.SQLiteOptions{Path: ":memory:"}})
	case "postgres":
		h, err = db.Open(db.Options{Driver: "postgres", Postgres: db.PostgresOptions{DSN: testlane.PostgresDSN(t)}})
	default:
		t.Fatalf("dbtest: unknown %s=%q (want sqlite or postgres)", DriverEnv, testlane.Driver())
	}
	if err != nil {
		t.Fatalf("dbtest: open %s: %v", testlane.Driver(), err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}
