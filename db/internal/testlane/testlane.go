// Package testlane provisions the per-test database lane behind the
// SQLite+Postgres dual-run without importing the db package — the
// dependency-free half of dbtest, shared by db's own internal tests
// (which cannot import dbtest: dbtest imports db, and an internal test
// closing that loop is an import cycle).
package testlane

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
)

// Env variable names (also read by CI).
const (
	DriverEnv = "CHOK_TEST_DRIVER"
	PGDSNEnv  = "CHOK_TEST_PG_DSN"
)

// Driver reports the lane the current process runs in: "sqlite"
// (default) or "postgres".
func Driver() string {
	d := strings.ToLower(os.Getenv(DriverEnv))
	if d == "" {
		return "sqlite"
	}
	return d
}

// PostgresDSN provisions a throwaway schema on the CHOK_TEST_PG_DSN
// database and returns a DSN pinned to it via search_path; the schema
// is dropped in test cleanup. Skips the test when the DSN is unset
// (local machines without Postgres).
func PostgresDSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(PGDSNEnv)
	if dsn == "" {
		t.Skipf("testlane: %s=postgres but %s is unset — skipping (no local Postgres)", DriverEnv, PGDSNEnv)
	}

	schema := freshSchemaName(t)

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("testlane: open postgres admin conn: %v", err)
	}
	if _, err := admin.Exec(fmt.Sprintf("CREATE SCHEMA %q", schema)); err != nil {
		_ = admin.Close()
		t.Fatalf("testlane: create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE", schema)); err != nil {
			t.Logf("testlane: drop schema %s: %v", schema, err)
		}
		_ = admin.Close()
	})
	return withSearchPath(t, dsn, schema)
}

// freshSchemaName builds a collision-free schema name per test.
func freshSchemaName(t testing.TB) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("testlane: random schema suffix: %v", err)
	}
	return fmt.Sprintf("chok_test_%d_%s", os.Getpid(), hex.EncodeToString(buf[:]))
}

// withSearchPath pins the connection's search_path to the throwaway
// schema. Handles both DSN shapes pgx accepts: URL form
// (postgres://...?k=v) gets a query parameter; keyword-value form gets
// a trailing key.
func withSearchPath(t testing.TB, dsn, schema string) string {
	t.Helper()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("testlane: parse %s: %v", PGDSNEnv, err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + schema
}
