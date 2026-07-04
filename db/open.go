package db

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openGorm dispatches on the validated driver discriminator.
func openGorm(o *Options) (*gorm.DB, error) {
	switch o.Driver {
	case "sqlite":
		return openSQLite(&o.SQLite)
	case "mysql":
		return openMySQL(&o.MySQL)
	case "postgres":
		return openPostgres(&o.Postgres)
	default:
		// Unreachable after Validate; keep the error for direct callers.
		return nil, fmt.Errorf("db: unsupported driver %q", o.Driver)
	}
}

func openSQLite(o *SQLiteOptions) (*gorm.DB, error) {
	dsn := o.Path
	if !sqliteIsMemory(o.Path) {
		dsn = sqliteDSNWithDefaults(o.Path)
	}
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("db: open sqlite: %w", err)
	}
	if sqliteIsMemory(o.Path) {
		// A memory database lives and dies with its connection: every
		// extra pool connection is a fresh empty database, and closing
		// the last one drops the data. Pin the pool to one immortal
		// connection so concurrent use (async sinks, parallel tests)
		// serializes instead of intermittently seeing "no such table".
		if sqlDB, derr := gdb.DB(); derr == nil {
			sqlDB.SetMaxOpenConns(1)
			sqlDB.SetMaxIdleConns(1)
			sqlDB.SetConnMaxLifetime(0)
			sqlDB.SetConnMaxIdleTime(0)
		}
		return gdb, nil
	}
	// WAL mode for concurrency (v1 behaviour carried over). The pragma
	// persists in the database file, so setting it once here covers
	// every connection — unlike the per-connection DSN defaults above.
	if err := gdb.Exec("PRAGMA journal_mode=WAL").Error; err != nil {
		return nil, fmt.Errorf("db: sqlite enable WAL: %w", err)
	}
	if o.MaxOpenConns > 0 {
		if sqlDB, derr := gdb.DB(); derr == nil {
			sqlDB.SetMaxOpenConns(o.MaxOpenConns)
			sqlDB.SetMaxIdleConns(o.MaxOpenConns)
		}
	}
	return gdb, nil
}

// sqliteDSNWithDefaults injects the per-connection concurrency defaults
// into a file-database DSN unless the user's path already sets them
// (driver aliases included):
//
//   - _txlock=immediate — write transactions take the write lock at
//     BEGIN. Under the driver default (deferred) a transaction that
//     reads first upgrades to the write lock mid-flight, and an
//     upgrade that loses the race fails SQLITE_BUSY immediately,
//     without honouring busy_timeout — the classic read-modify-write
//     trap under concurrency.
//   - _synchronous=NORMAL — safe under WAL (a crash can lose the last
//     checkpoint window, never corrupt the file) and several times
//     the write throughput of SQLite's default FULL.
//
// busy_timeout is not injected: the driver already defaults to 5000ms.
// A malformed query string is handed to the driver untouched — DSN
// error reporting stays the driver's job.
func sqliteDSNWithDefaults(path string) string {
	base, query, _ := strings.Cut(path, "?")
	vals, err := url.ParseQuery(query)
	if err != nil {
		return path
	}
	inject := func(key, val string, aliases ...string) {
		for _, k := range append(aliases, key) {
			if vals.Has(k) {
				return
			}
		}
		vals.Set(key, val)
	}
	inject("_txlock", "immediate")
	inject("_synchronous", "NORMAL", "_sync")
	return base + "?" + vals.Encode()
}

// sqliteIsMemory reports whether the path denotes an in-memory SQLite
// database (":memory:", "file::memory:...", or any DSN carrying
// mode=memory).
func sqliteIsMemory(path string) bool {
	return path == ":memory:" ||
		strings.HasPrefix(path, "file::memory:") ||
		strings.Contains(path, "mode=memory")
}

func openMySQL(o *MySQLOptions) (*gorm.DB, error) {
	tlsName, err := mysqlTLSConfig(o)
	if err != nil {
		return nil, err
	}

	cfg := &gomysql.Config{
		User:                 o.Username,
		Passwd:               o.Password,
		Net:                  "tcp",
		Addr:                 fmt.Sprintf("%s:%d", o.Host, o.Port),
		DBName:               o.Database,
		Params:               map[string]string{"charset": "utf8mb4"},
		ParseTime:            true,
		Loc:                  time.Local,
		AllowNativePasswords: true,
	}
	if tlsName != "" {
		cfg.TLSConfig = tlsName
	}

	gdb, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("db: open mysql: %w", err)
	}
	applyPool(gdb, o.MaxOpenConns, o.MaxIdleConns, o.ConnMaxLifetime, o.ConnMaxIdleTime)
	return gdb, nil
}

// mysqlTLSConfig resolves the value for gomysql.Config.TLSConfig
// (toffs v0.4.2 port). With no CACert it returns o.TLS verbatim — a
// go-sql-driver built-in name ("true"/"skip-verify"/"preferred") or
// "" for a plaintext connection. With CACert set it loads the PEM,
// registers a verifying tls.Config keyed by host (so a managed
// database presenting a private-CA certificate validates), and
// returns that registration key, which takes precedence over o.TLS.
// The registry is process-global and never unregistered; re-opening
// the same host overwrites idempotently.
func mysqlTLSConfig(o *MySQLOptions) (string, error) {
	if o.CACert == "" {
		return o.TLS, nil
	}
	pem, err := os.ReadFile(o.CACert)
	if err != nil {
		return "", fmt.Errorf("db: read mysql ca_cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return "", fmt.Errorf("db: mysql ca_cert %q contained no PEM certificates", o.CACert)
	}
	name := "chok-mysql-" + o.Host
	if err := gomysql.RegisterTLSConfig(name, &tls.Config{
		RootCAs:    pool,
		ServerName: o.Host,
	}); err != nil {
		return "", fmt.Errorf("db: register mysql tls config: %w", err)
	}
	return name, nil
}

func openPostgres(o *PostgresOptions) (*gorm.DB, error) {
	dsn := o.DSN
	if dsn == "" {
		dsn = postgresKeywordDSN(o)
	}
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("db: open postgres: %w", err)
	}
	applyPool(gdb, o.MaxOpenConns, o.MaxIdleConns, o.ConnMaxLifetime, o.ConnMaxIdleTime)
	return gdb, nil
}

// postgresKeywordDSN renders discrete fields as a libpq keyword-value
// DSN. Every value is single-quoted so passwords containing spaces,
// '=' or quotes survive parsing.
func postgresKeywordDSN(o *PostgresOptions) string {
	var b strings.Builder
	kv := func(k, v string) {
		if v == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteString("='")
		b.WriteString(strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v))
		b.WriteByte('\'')
	}
	kv("host", o.Host)
	kv("port", fmt.Sprintf("%d", o.Port))
	kv("user", o.Username)
	kv("password", o.Password)
	kv("dbname", o.Database)
	kv("sslmode", o.SSLMode)
	kv("sslrootcert", o.CACert)
	return b.String()
}

func applyPool(gdb *gorm.DB, maxOpen, maxIdle int, maxLifetime, maxIdleTime time.Duration) {
	sqlDB, err := gdb.DB()
	if err != nil {
		return // pool tuning is best-effort; connectivity errors surface on first use
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(maxLifetime)
	sqlDB.SetConnMaxIdleTime(maxIdleTime)
}
