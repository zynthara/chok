package db

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/internal/conf"
)

func TestConfig_Validate(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"bad driver", Config{Driver: "oracle", Migrate: "auto"}, "driver: must be one of"},
		{"bad mode", Config{Driver: "sqlite", Migrate: "sometimes", SQLite: SQLiteConfig{Path: "x"}}, "migrate: must be one of"},
		{"sqlite path", Config{Driver: "sqlite", Migrate: "auto"}, "sqlite.path must not be empty"},
		{"mysql host", Config{Driver: "mysql", Migrate: "auto", MySQL: MySQLConfig{Database: "d"}}, "mysql.host must not be empty"},
		{"mysql database", Config{Driver: "mysql", Migrate: "auto", MySQL: MySQLConfig{Host: "h"}}, "mysql.database must not be empty"},
		{"mysql tls", Config{Driver: "mysql", Migrate: "auto", MySQL: MySQLConfig{Host: "h", Database: "d", TLS: "maybe"}}, "mysql.tls: must be one of"},
		{"pg both", Config{Driver: "postgres", Migrate: "auto", Postgres: PostgresConfig{DSN: "x", Database: "d"}}, "mutually exclusive"},
		{"pg none", Config{Driver: "postgres", Migrate: "auto"}, "postgres needs dsn or database"},
		{"pg sslmode", Config{Driver: "postgres", Migrate: "auto", Postgres: PostgresConfig{Database: "d", SSLMode: "sometimes"}}, "postgres.ssl_mode: must be one of"},
		{"admin role", Config{Driver: "sqlite", Migrate: "auto", SQLite: SQLiteConfig{Path: "x"}, Store: StorePolicy{AdminRoles: []string{" "}}}, "admin_roles[0] must not be empty"},
		{"page sizes", Config{Driver: "sqlite", Migrate: "auto", SQLite: SQLiteConfig{Path: "x"}, Store: StorePolicy{DefaultPageSize: 50, MaxPageSize: 10}}, "exceeds store.max_page_size"},
		{"negative", Config{Driver: "sqlite", Migrate: "auto", SQLite: SQLiteConfig{Path: "x"}, SlowThreshold: -time.Second}, "slow_threshold: must be at least 0"},
	}
	for _, c := range cases {
		err := conf.Check(&c.cfg)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
	ok := Config{Driver: "sqlite", Migrate: "auto", SQLite: SQLiteConfig{Path: "x"}}
	if err := conf.Check(&ok); err != nil {
		t.Fatalf("valid config: %v", err)
	}
}

func TestConfig_StringMasksSecrets(t *testing.T) {
	cfg := Config{MySQL: MySQLConfig{Password: "hunter2"}, Postgres: PostgresConfig{DSN: "postgres://u:hunter2@h/db", Password: "hunter2"}}
	for _, s := range []string{fmt.Sprint(cfg), fmt.Sprintf("%+v", cfg), cfg.MySQL.String(), cfg.Postgres.String()} {
		if strings.Contains(s, "hunter2") {
			t.Fatalf("secret leaked: %s", s)
		}
	}
}

func TestPostgresKeywordDSN_QuotesAwkwardValues(t *testing.T) {
	dsn := postgresKeywordDSN(PostgresConfig{Host: "h", Username: "u", Password: `p a's\s`, Database: "d", SSLMode: "disable"})
	for _, want := range []string{`host='h'`, `port='5432'`, `password='p a\'s\\s'`, `dbname='d'`, `sslmode='disable'`, `timezone='UTC'`} {
		if !strings.Contains(dsn, want) {
			t.Errorf("dsn %s lacks %s", dsn, want)
		}
	}
}

func TestSQLiteDSN_InjectsConcurrencyDefaults(t *testing.T) {
	w, err := sqliteDSN("app.db", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"_txlock=immediate", "synchronous%28NORMAL%29", "busy_timeout%285000%29", "foreign_keys%281%29"} {
		if !strings.Contains(w, want) {
			t.Errorf("writer dsn %s lacks %s", w, want)
		}
	}
	r, _ := sqliteDSN("app.db", false)
	if strings.Contains(r, "_txlock") {
		t.Errorf("readers need no immediate transactions: %s", r)
	}
	m, _ := sqliteDSN(":memory:", true)
	if strings.Contains(m, "synchronous") || !strings.Contains(m, "foreign_keys") {
		t.Errorf("memory dsn: %s", m)
	}
}

func TestSQLiteDSN_UserSettingsWin(t *testing.T) {
	w, err := sqliteDSN("app.db?_pragma=busy_timeout(10000)&_txlock=deferred", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(w, "busy_timeout") != 1 || !strings.Contains(w, "busy_timeout%2810000%29") || !strings.Contains(w, "_txlock=deferred") {
		t.Fatalf("got %s", w)
	}
}

func TestSQLiteDSN_LegacyMattnParamsRejected(t *testing.T) {
	for _, p := range []string{"_busy_timeout=5000", "_fk=1", "_journal_mode=WAL"} {
		if _, err := sqliteDSN("app.db?"+p, true); err == nil || !strings.Contains(err.Error(), "mattn") {
			t.Errorf("%s: got %v", p, err)
		}
	}
}

func TestMySQLTLS_NoCAPassesThrough(t *testing.T) {
	name, err := mysqlTLS(MySQLConfig{TLS: "skip-verify"})
	if err != nil || name != "skip-verify" {
		t.Fatalf("got %q %v", name, err)
	}
}

func TestMySQLTLS_CACertRegistersVerifyingConfig(t *testing.T) {
	path := writeCA(t)
	name, err := mysqlTLS(MySQLConfig{Host: "db.internal", CACert: path})
	if err != nil || name != "chok-db.internal" {
		t.Fatalf("got %q %v", name, err)
	}
}

func TestMySQLTLS_BadCA(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mysqlTLS(MySQLConfig{CACert: bad}); err == nil || !strings.Contains(err.Error(), "no PEM") {
		t.Fatalf("got %v", err)
	}
	if _, err := mysqlTLS(MySQLConfig{CACert: filepath.Join(t.TempDir(), "missing.pem")}); err == nil {
		t.Fatal("a missing file is an error")
	}
}

func writeCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, IsCA: true,
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
