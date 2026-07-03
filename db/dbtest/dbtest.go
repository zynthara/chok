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
// gorm types are confined to the db/ tree on purpose — this package
// exists so tests outside db/ don't have to touch gorm to get a
// database.
package dbtest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
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

// Open returns a fresh gorm handle for the active lane, isolated per
// test and cleaned up automatically. Unknown drivers fail the test;
// the postgres lane skips when CHOK_TEST_PG_DSN is unset.
func Open(t testing.TB) *gorm.DB {
	t.Helper()
	switch Driver() {
	case "sqlite":
		return openSQLite(t)
	case "postgres":
		return openPostgres(t)
	default:
		t.Fatalf("dbtest: unknown %s=%q (want sqlite or postgres)", DriverEnv, Driver())
		return nil
	}
}

func openSQLite(t testing.TB) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("dbtest: open sqlite: %v", err)
	}
	t.Cleanup(func() { closeGorm(t, gdb) })
	return gdb
}

func openPostgres(t testing.TB) *gorm.DB {
	t.Helper()
	dsn := os.Getenv(PGDSNEnv)
	if dsn == "" {
		t.Skipf("dbtest: %s=postgres but %s is unset — skipping (no local Postgres)", DriverEnv, PGDSNEnv)
	}

	schema := freshSchemaName(t)

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("dbtest: open postgres admin conn: %v", err)
	}
	if _, err := admin.Exec(fmt.Sprintf("CREATE SCHEMA %q", schema)); err != nil {
		_ = admin.Close()
		t.Fatalf("dbtest: create schema %s: %v", schema, err)
	}

	gdb, err := gorm.Open(postgres.Open(withSearchPath(t, dsn, schema)), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		_, _ = admin.Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE", schema))
		_ = admin.Close()
		t.Fatalf("dbtest: open postgres: %v", err)
	}

	t.Cleanup(func() {
		closeGorm(t, gdb)
		if _, err := admin.Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE", schema)); err != nil {
			t.Logf("dbtest: drop schema %s: %v", schema, err)
		}
		_ = admin.Close()
	})
	return gdb
}

// freshSchemaName builds a collision-free schema name per test.
func freshSchemaName(t testing.TB) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("dbtest: random schema suffix: %v", err)
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
			t.Fatalf("dbtest: parse %s: %v", PGDSNEnv, err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + schema
}

func closeGorm(t testing.TB, gdb *gorm.DB) {
	t.Helper()
	sqlDB, err := gdb.DB()
	if err != nil {
		return
	}
	if err := sqlDB.Close(); err != nil {
		t.Logf("dbtest: close: %v", err)
	}
}
