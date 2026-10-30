// Package dbtest gives each test a throwaway database of the dialect
// CHOK_TEST_DRIVER names: sqlite (the default; a file in t.TempDir, so
// the production pool layout is exercised), postgres (a fresh schema in
// the database of CHOK_TEST_PG_DSN) or mysql (a fresh chok_test_<rand>
// database on the server of CHOK_TEST_MYSQL_DSN). Cleanup drops it. A
// lane whose DSN is missing fails rather than silently skipping.
//
// choktest.DB wraps it for applications; the batteries' own tests use it
// directly.
package dbtest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // the pgx database/sql driver for the admin connection

	"github.com/zynthara/chok/db"
)

// The environment of the database lanes.
const (
	DriverEnv   = "CHOK_TEST_DRIVER"
	PGDSNEnv    = "CHOK_TEST_PG_DSN"
	MySQLDSNEnv = "CHOK_TEST_MYSQL_DSN"
)

// Driver is the lane's dialect: sqlite, postgres or mysql.
func Driver() string {
	if d := strings.ToLower(os.Getenv(DriverEnv)); d != "" {
		return d
	}
	return "sqlite"
}

// Config returns a db.Config pointing at a fresh database of the lane's
// dialect, migrate auto, which cleanup drops.
func Config(t testing.TB) db.Config {
	t.Helper()
	switch Driver() {
	case "sqlite":
		return db.Config{Driver: "sqlite", Migrate: "auto", SQLite: db.SQLiteConfig{Path: filepath.Join(t.TempDir(), "test.db")}}
	case "postgres":
		return db.Config{Driver: "postgres", Migrate: "auto", Postgres: db.PostgresConfig{DSN: postgresSchema(t)}}
	case "mysql":
		return db.Config{Driver: "mysql", Migrate: "auto", MySQL: mysqlDatabase(t)}
	}
	t.Fatalf("dbtest: %s=%q; want sqlite, postgres or mysql", DriverEnv, Driver())
	return db.Config{}
}

// Open opens a fresh database with opts and closes it at cleanup.
func Open(t testing.TB, opts ...db.Option) *db.DB {
	t.Helper()
	return OpenConfig(t, Config(t), opts...)
}

// OpenConfig opens cfg, typically Config(t) adjusted, and closes it at
// cleanup.
func OpenConfig(t testing.TB, cfg db.Config, opts ...db.Option) *db.DB {
	t.Helper()
	// Warnings and errors go to the test's output, shown when it fails.
	opts = append([]db.Option{db.Logger(slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelWarn})))}, opts...)
	d, err := db.Open(cfg, opts...)
	if err != nil {
		t.Fatalf("dbtest: open %s: %v", cfg.Driver, err)
	}
	t.Cleanup(func() { _ = d.Close(t.Context()) })
	return d
}

func suffix(t testing.TB) string {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%d_%s", os.Getpid(), hex.EncodeToString(b[:]))
}

// postgresSchema creates a schema and returns the DSN with search_path
// set to it.
func postgresSchema(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(PGDSNEnv)
	if dsn == "" {
		t.Fatalf("dbtest: %s=postgres needs %s (set -a; source .env; set +a)", DriverEnv, PGDSNEnv)
	}
	schema := "chok_test_" + suffix(t)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("dbtest: postgres: %v", err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA "` + schema + `"`); err != nil {
		_ = admin.Close()
		t.Fatalf("dbtest: postgres: create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA "` + schema + `" CASCADE`); err != nil {
			t.Logf("dbtest: postgres: drop schema %s: %v", schema, err)
		}
		_ = admin.Close()
	})
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + schema
}

// mysqlDatabase creates a database and returns the config for it.
func mysqlDatabase(t testing.TB) db.MySQLConfig {
	t.Helper()
	dsn := os.Getenv(MySQLDSNEnv)
	if dsn == "" {
		t.Fatalf("dbtest: %s=mysql needs %s (set -a; source .env; set +a)", DriverEnv, MySQLDSNEnv)
	}
	cfg, err := gomysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("dbtest: %s: %v", MySQLDSNEnv, err)
	}
	name := "chok_test_" + suffix(t)
	admin := *cfg
	admin.DBName = ""
	conn, err := sql.Open("mysql", admin.FormatDSN())
	if err != nil {
		t.Fatalf("dbtest: mysql: %v", err)
	}
	if _, err := conn.Exec("CREATE DATABASE `" + name + "` DEFAULT CHARACTER SET utf8mb4"); err != nil {
		_ = conn.Close()
		t.Fatalf("dbtest: mysql: create database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Logf("dbtest: mysql: drop database %s: %v", name, err)
		}
		_ = conn.Close()
	})
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		t.Fatalf("dbtest: mysql address %q: %v", cfg.Addr, err)
	}
	p, _ := strconv.Atoi(port)
	return db.MySQLConfig{Host: host, Port: p, Username: cfg.User, Password: cfg.Passwd, Database: name}
}
