package db

import (
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// openGorm opens the dialect; for a SQLite file it also returns the read
// pool that the read router sends queries to.
func openGorm(cfg *Config, gcfg *gorm.Config) (*gorm.DB, *sql.DB, error) {
	switch cfg.Driver {
	case "sqlite":
		return openSQLite(cfg, gcfg)
	case "mysql":
		gdb, err := openMySQL(cfg, gcfg)
		return gdb, nil, err
	case "postgres":
		gdb, err := openPostgres(cfg, gcfg)
		return gdb, nil, err
	}
	return nil, nil, fmt.Errorf("db: unsupported driver %q (use sqlite, mysql or postgres)", cfg.Driver)
}

// openSQLite opens a file database as one writer connection (BEGIN
// IMMEDIATE, so writers queue instead of failing with SQLITE_BUSY) plus a
// pool of readers that WAL lets run beside it. An in-memory database is a
// single connection: every connection would otherwise see its own empty
// database.
func openSQLite(cfg *Config, gcfg *gorm.Config) (*gorm.DB, *sql.DB, error) {
	path := cfg.SQLite.Path
	if sqliteIsMemory(path) {
		dsn, err := sqliteDSN(path, false)
		if err != nil {
			return nil, nil, err
		}
		gdb, err := gorm.Open(sqlite.Open(dsn), gcfg)
		if err != nil {
			return nil, nil, fmt.Errorf("db: open sqlite: %w", err)
		}
		pool, _ := gdb.DB()
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)
		pool.SetConnMaxLifetime(0)
		pool.SetConnMaxIdleTime(0)
		return gdb, nil, nil
	}
	writeDSN, err := sqliteDSN(path, true)
	if err != nil {
		return nil, nil, err
	}
	readDSN, err := sqliteDSN(path, false)
	if err != nil {
		return nil, nil, err
	}
	gdb, err := gorm.Open(sqlite.Open(writeDSN), gcfg)
	if err != nil {
		return nil, nil, fmt.Errorf("db: open sqlite: %w", err)
	}
	writer, _ := gdb.DB()
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)
	writer.SetConnMaxIdleTime(0)
	if err := enableWAL(writer); err != nil {
		_ = writer.Close()
		return nil, nil, fmt.Errorf("db: sqlite: enable WAL: %w", err)
	}
	readers, err := sql.Open(sqlite.DriverName, readDSN)
	if err != nil {
		_ = writer.Close()
		return nil, nil, fmt.Errorf("db: sqlite: open the read pool: %w", err)
	}
	n := cfg.Pool.MaxOpenConns
	if n <= 0 || n > 64 {
		n = max(4, runtime.NumCPU())
	}
	readers.SetMaxOpenConns(n)
	readers.SetMaxIdleConns(n) // reopening re-parses the schema; keep them warm
	readers.SetConnMaxLifetime(0)
	readers.SetConnMaxIdleTime(0)
	if err := routeReads(gdb, readers); err != nil {
		_ = readers.Close()
		_ = writer.Close()
		return nil, nil, err
	}
	return gdb, readers, nil
}

// enableWAL switches a file database to WAL. The switch needs a moment of
// exclusive access, and SQLite answers SQLITE_BUSY at once rather than
// waiting out busy_timeout when another process holds the file, as when
// several replicas start together, so it retries for up to five seconds.
func enableWAL(pool *sql.DB) error {
	var mode string
	if err := pool.QueryRow("PRAGMA journal_mode").Scan(&mode); err == nil && strings.EqualFold(mode, "wal") {
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for wait := 5 * time.Millisecond; ; wait = min(2*wait, 200*time.Millisecond) {
		_, err := pool.Exec("PRAGMA journal_mode=WAL")
		if err == nil || !strings.Contains(err.Error(), "SQLITE_BUSY") || time.Now().After(deadline) {
			return err
		}
		time.Sleep(wait)
	}
}

// routeReads sends queries that run outside a transaction and take no row
// lock to the read pool; everything else stays on the writer.
func routeReads(gdb *gorm.DB, readers *sql.DB) error {
	toReaders := func(tx *gorm.DB) {
		if _, inTx := tx.Statement.ConnPool.(gorm.TxCommitter); inTx {
			return
		}
		if _, locking := tx.Statement.Clauses["FOR"]; locking {
			return
		}
		if tx.Statement.SQL.Len() > 0 && !isSelect(tx.Statement.SQL.String()) {
			return
		}
		tx.Statement.ConnPool = readers
	}
	if err := gdb.Callback().Query().Before("gorm:query").Register("chok:read_pool", toReaders); err != nil {
		return fmt.Errorf("db: sqlite: install the read router: %w", err)
	}
	if err := gdb.Callback().Row().Before("gorm:row").Register("chok:read_pool", toReaders); err != nil {
		return fmt.Errorf("db: sqlite: install the read router: %w", err)
	}
	return nil
}

func isSelect(sql string) bool {
	s := strings.TrimLeft(sql, " \t\r\n(")
	return len(s) >= 6 && (strings.EqualFold(s[:6], "select") || strings.EqualFold(s[:4], "with"))
}

// sqliteLegacyParams are mattn/go-sqlite3 spellings the pure-Go driver
// ignores without a word; refusing them beats a silently missing pragma.
var sqliteLegacyParams = map[string]string{
	"_busy_timeout": "busy_timeout", "_timeout": "busy_timeout",
	"_journal_mode": "journal_mode", "_journal": "journal_mode",
	"_synchronous": "synchronous", "_sync": "synchronous",
	"_foreign_keys": "foreign_keys", "_fk": "foreign_keys",
}

// sqliteDSN adds the defaults a server needs unless the path sets them:
// foreign keys on, synchronous NORMAL and a busy timeout for files, and
// immediate transactions on the writer.
func sqliteDSN(path string, writer bool) (string, error) {
	base, query, _ := strings.Cut(path, "?")
	vals, err := url.ParseQuery(query)
	if err != nil {
		return "", fmt.Errorf("db: sqlite: path %q has an unreadable query: %w", path, err)
	}
	for k, canonical := range sqliteLegacyParams {
		if vals.Has(k) {
			return "", fmt.Errorf("db: sqlite: %s is a mattn/go-sqlite3 parameter the pure-Go driver ignores; write _pragma=%s(...)", k, canonical)
		}
	}
	pragma := func(name, arg string) {
		for _, v := range vals["_pragma"] {
			if pragmaName(v) == name {
				return
			}
		}
		vals.Add("_pragma", name+"("+arg+")")
	}
	if !sqliteIsMemory(path) {
		if writer && !vals.Has("_txlock") {
			vals.Set("_txlock", "immediate")
		}
		pragma("synchronous", "NORMAL")
		pragma("busy_timeout", "5000")
	}
	pragma("foreign_keys", "1")
	return base + "?" + vals.Encode(), nil
}

func pragmaName(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.IndexAny(v, "(= \t"); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(v)
}

func sqliteIsMemory(path string) bool {
	return path == ":memory:" || strings.HasPrefix(path, "file::memory:") || strings.Contains(path, "mode=memory")
}

func openMySQL(cfg *Config, gcfg *gorm.Config) (*gorm.DB, error) {
	m := cfg.MySQL
	tlsName, err := mysqlTLS(m)
	if err != nil {
		return nil, err
	}
	port := m.Port
	if port == 0 {
		port = 3306
	}
	dc := gomysql.NewConfig()
	dc.User, dc.Passwd, dc.Net = m.Username, m.Password, "tcp"
	dc.Addr = fmt.Sprintf("%s:%d", m.Host, port)
	dc.DBName = m.Database
	dc.ParseTime, dc.Loc = true, time.UTC
	dc.TLSConfig = tlsName
	dc.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+00:00'"}
	gdb, err := gorm.Open(mysql.New(mysql.Config{DSNConfig: dc, DSN: dc.FormatDSN()}), gcfg)
	if err != nil {
		return nil, fmt.Errorf("db: open mysql: %w", err)
	}
	applyPool(gdb, cfg.Pool)
	return gdb, nil
}

// mysqlTLS registers a verifying TLS configuration for CACert and returns
// its name; without one, TLS passes through as the driver's own value.
func mysqlTLS(m MySQLConfig) (string, error) {
	if m.CACert == "" {
		return m.TLS, nil
	}
	pem, err := os.ReadFile(m.CACert)
	if err != nil {
		return "", fmt.Errorf("db: mysql.ca_cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return "", fmt.Errorf("db: mysql.ca_cert %s holds no PEM certificate", m.CACert)
	}
	name := "chok-" + m.Host
	if err := gomysql.RegisterTLSConfig(name, &tls.Config{RootCAs: pool, ServerName: m.Host, MinVersion: tls.VersionTLS12}); err != nil {
		return "", fmt.Errorf("db: mysql: register TLS: %w", err)
	}
	return name, nil
}

func openPostgres(cfg *Config, gcfg *gorm.Config) (*gorm.DB, error) {
	dsn := cfg.Postgres.DSN
	if dsn == "" {
		dsn = postgresKeywordDSN(cfg.Postgres)
	} else {
		dsn = postgresUTC(dsn)
	}
	gdb, err := gorm.Open(postgres.Open(dsn), gcfg)
	if err != nil {
		return nil, fmt.Errorf("db: open postgres: %w", err)
	}
	applyPool(gdb, cfg.Pool)
	return gdb, nil
}

// postgresKeywordDSN builds a libpq keyword DSN, quoting every value so a
// password with spaces or quotes survives.
func postgresKeywordDSN(p PostgresConfig) string {
	var b strings.Builder
	kv := func(k, v string) {
		if v == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k + "='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'")
	}
	port := p.Port
	if port == 0 {
		port = 5432
	}
	kv("host", p.Host)
	kv("port", fmt.Sprint(port))
	kv("user", p.Username)
	kv("password", p.Password)
	kv("dbname", p.Database)
	kv("sslmode", p.SSLMode)
	kv("sslrootcert", p.CACert)
	kv("timezone", "UTC")
	return b.String()
}

// postgresUTC sets the session time zone of a given DSN to UTC unless it
// names one, so timestamps and date functions do not follow the host.
func postgresUTC(dsn string) string {
	if strings.Contains(strings.ToLower(dsn), "timezone") {
		return dsn
	}
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("timezone", "UTC")
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " timezone=UTC"
}

func applyPool(gdb *gorm.DB, p PoolConfig) {
	pool, err := gdb.DB()
	if err != nil {
		return
	}
	pool.SetMaxOpenConns(p.MaxOpenConns)
	pool.SetMaxIdleConns(p.MaxIdleConns)
	pool.SetConnMaxLifetime(p.ConnMaxLifetime)
	pool.SetConnMaxIdleTime(p.ConnMaxIdleTime)
}
