package db

import (
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	gomysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/plugin/dbresolver"
)

// openGorm dispatches on the validated driver discriminator. The
// second return is an auxiliary pool the handle must close alongside
// the primary one (the sqlite read pool); nil for the network drivers.
func openGorm(o *Options) (*gorm.DB, *sql.DB, error) {
	switch o.Driver {
	case "sqlite":
		return openSQLite(&o.SQLite, o.ReadOnly)
	case "mysql":
		gdb, err := openMySQL(&o.MySQL, o.ReadOnly)
		return gdb, nil, err
	case "postgres":
		gdb, err := openPostgres(&o.Postgres, o.ReadOnly)
		return gdb, nil, err
	default:
		// Unreachable after Validate; keep the error for direct callers.
		return nil, nil, fmt.Errorf("db: unsupported driver %q", o.Driver)
	}
}

// openSQLite opens the pure-Go SQLite stack (glebarez / modernc — no
// CGO). A file database gets the single-process production shape:
//
//   - a WRITE pool pinned to one connection with _txlock=immediate.
//     SQLite physically allows one writer per file; a second write
//     connection could only ever spin in busy_timeout. One connection
//     turns the Go pool into the fair write queue, and IMMEDIATE
//     takes the write lock at BEGIN so read-then-write transactions
//     never hit the un-retryable mid-flight lock upgrade.
//   - a READ pool of max(4, NumCPU) connections (max_open_conns
//     overrides): under WAL readers run on snapshots, in parallel
//     with the writer and each other.
//   - gorm.io/plugin/dbresolver routes per gorm callback — queries to
//     the read pool; creates/updates/deletes, transactions and raw
//     non-SELECT statements to the write pool. Callers see a single
//     handle; INSERT ... RETURNING stays on the write side because
//     routing keys on the callback, not on the SQL verb.
//
// A memory database cannot be split — every extra connection is a
// fresh empty database — so it keeps the pinned single-connection
// pool and no read replica.
func openSQLite(o *SQLiteOptions, readOnly bool) (*gorm.DB, *sql.DB, error) {
	if readOnly {
		dsn, err := sqliteReadOnlyDSN(o.Path)
		if err != nil {
			return nil, nil, err
		}
		gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			return nil, nil, fmt.Errorf("db: open read-only sqlite: %w", err)
		}
		if sqlDB, derr := gdb.DB(); derr == nil {
			readConns := o.MaxOpenConns
			if readConns <= 0 {
				readConns = max(4, runtime.NumCPU())
			}
			sqlDB.SetMaxOpenConns(readConns)
			sqlDB.SetMaxIdleConns(readConns)
			sqlDB.SetConnMaxLifetime(0)
			sqlDB.SetConnMaxIdleTime(0)
		}
		return gdb, nil, nil
	}
	if sqliteIsMemory(o.Path) {
		dsn, err := sqliteDSN(o.Path, false)
		if err != nil {
			return nil, nil, err
		}
		gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
			Logger: logger.Discard,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("db: open sqlite: %w", err)
		}
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
		return gdb, nil, nil
	}

	writeDSN, err := sqliteDSN(o.Path, true)
	if err != nil {
		return nil, nil, err
	}
	readDSN, err := sqliteDSN(o.Path, false)
	if err != nil {
		return nil, nil, err
	}

	gdb, err := gorm.Open(sqlite.Open(writeDSN), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("db: open sqlite: %w", err)
	}
	writePool, err := gdb.DB()
	if err != nil {
		return nil, nil, fmt.Errorf("db: sqlite write pool: %w", err)
	}
	writePool.SetMaxOpenConns(1)
	writePool.SetMaxIdleConns(1)
	writePool.SetConnMaxLifetime(0)
	writePool.SetConnMaxIdleTime(0)

	// WAL mode: readers run on snapshots instead of blocking the
	// writer. The pragma persists in the database file, so setting it
	// once on the write pool covers the read pool too — unlike the
	// per-connection DSN defaults above.
	if err := gdb.Exec("PRAGMA journal_mode=WAL").Error; err != nil {
		_ = writePool.Close()
		return nil, nil, fmt.Errorf("db: sqlite enable WAL: %w", err)
	}

	readPool, err := sql.Open(sqlite.DriverName, readDSN)
	if err != nil {
		_ = writePool.Close()
		return nil, nil, fmt.Errorf("db: sqlite read pool: %w", err)
	}
	readConns := o.MaxOpenConns
	if readConns <= 0 {
		readConns = max(4, runtime.NumCPU())
	}
	readPool.SetMaxOpenConns(readConns)
	readPool.SetMaxIdleConns(readConns) // reopening re-parses the schema; warm is free
	readPool.SetConnMaxLifetime(0)
	readPool.SetConnMaxIdleTime(0)

	if err := gdb.Use(dbresolver.Register(dbresolver.Config{
		Replicas: []gorm.Dialector{&sqlite.Dialector{Conn: readPool}},
	})); err != nil {
		_ = readPool.Close()
		_ = writePool.Close()
		return nil, nil, fmt.Errorf("db: sqlite read/write split: %w", err)
	}
	return gdb, readPool, nil
}

func sqliteReadOnlyDSN(path string) (string, error) {
	dsn, err := sqliteDSN(path, false)
	if err != nil {
		return "", err
	}
	base, query, _ := strings.Cut(dsn, "?")
	vals, err := url.ParseQuery(query)
	if err != nil {
		return "", fmt.Errorf("db: sqlite: parse read-only DSN: %w", err)
	}
	if mode := vals.Get("mode"); mode != "" && mode != "ro" {
		return "", fmt.Errorf("db: sqlite: read_only conflicts with mode=%s; use mode=ro", mode)
	}
	vals.Set("mode", "ro")
	pragmas := vals["_pragma"][:0]
	for _, p := range vals["_pragma"] {
		if sqlitePragmaName(p) != "query_only" {
			pragmas = append(pragmas, p)
		}
	}
	vals["_pragma"] = append(pragmas, "query_only(1)")
	if !strings.HasPrefix(base, "file:") {
		base = "file:" + base
	}
	return base + "?" + vals.Encode(), nil
}

// sqliteLegacyParams maps mattn/go-sqlite3 DSN spellings (the CGO
// driver chok used before the pure-Go swap) to the pragma names the
// glebarez driver understands. They are rejected loudly at Open: the
// new driver ignores parameters it does not know, so a path carried
// over from the CGO era would otherwise silently lose its tuning.
var sqliteLegacyParams = map[string]string{
	"_busy_timeout": "busy_timeout",
	"_timeout":      "busy_timeout",
	"_journal_mode": "journal_mode",
	"_journal":      "journal_mode",
	"_synchronous":  "synchronous",
	"_sync":         "synchronous",
	"_foreign_keys": "foreign_keys",
	"_fk":           "foreign_keys",
}

// sqliteDSN renders the path with chok's per-connection defaults
// injected, respecting anything the user's own query string already
// sets:
//
//   - _pragma=foreign_keys(1) — referential integrity on. Postgres
//     always enforces declared constraints; SQLite's default (off)
//     would let the two lanes diverge silently.
//   - _pragma=synchronous(NORMAL) — safe under WAL (a crash can lose
//     the last checkpoint window, never corrupt the file) and several
//     times the write throughput of SQLite's default FULL. File
//     databases only; meaningless for memory.
//   - _txlock=immediate — write side only (see openSQLite).
//
// busy_timeout is not injected: the driver already defaults to 5000ms
// (glebarez matches mattn here). A malformed query string is handed
// to the driver untouched — DSN error reporting stays the driver's
// job.
func sqliteDSN(path string, write bool) (string, error) {
	base, query, _ := strings.Cut(path, "?")
	vals, err := url.ParseQuery(query)
	if err != nil {
		return path, nil
	}
	for k, canonical := range sqliteLegacyParams {
		if vals.Has(k) {
			return "", fmt.Errorf("db: sqlite: DSN parameter %q is a mattn/go-sqlite3 spelling the pure-Go driver silently ignores; use _pragma=%s(...) instead", k, canonical)
		}
	}
	pragma := func(name, arg string) {
		for _, v := range vals["_pragma"] {
			if sqlitePragmaName(v) == name {
				return // the user's setting wins
			}
		}
		vals.Add("_pragma", name+"("+arg+")")
	}
	if !sqliteIsMemory(path) {
		if write && !vals.Has("_txlock") {
			vals.Set("_txlock", "immediate")
		}
		pragma("synchronous", "NORMAL")
	}
	pragma("foreign_keys", "1")
	return base + "?" + vals.Encode(), nil
}

// sqlitePragmaName extracts the pragma identifier from a _pragma DSN
// value: "busy_timeout(5000)", "busy_timeout=5000" and a bare
// "busy_timeout" all name busy_timeout.
func sqlitePragmaName(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.IndexAny(v, "(= \t"); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(v)
}

// sqliteIsMemory reports whether the path denotes an in-memory SQLite
// database (":memory:", "file::memory:...", or any DSN carrying
// mode=memory).
func sqliteIsMemory(path string) bool {
	return path == ":memory:" ||
		strings.HasPrefix(path, "file::memory:") ||
		strings.Contains(path, "mode=memory")
}

func openMySQL(o *MySQLOptions, readOnly bool) (*gorm.DB, error) {
	tlsName, err := mysqlTLSConfig(o)
	if err != nil {
		return nil, err
	}

	cfg, err := mysqlDriverConfig(o, tlsName, readOnly)
	if err != nil {
		return nil, err
	}

	var dialector gorm.Dialector
	var ownedPool *sql.DB
	if cfg.Loc == time.UTC {
		// Default UTC baseline: the DSN round trip is lossless
		// (FormatDSN omits loc entirely for UTC) — the #17-verified
		// path, byte-identical to before the time_zone knob existed.
		dialector = mysql.Open(cfg.FormatDSN())
	} else {
		connector, err := mysqlFixedOffsetConnector(cfg)
		if err != nil {
			return nil, fmt.Errorf("db: open mysql: %w", err)
		}
		ownedPool = sql.OpenDB(connector)
		dialector = mysql.New(mysql.Config{Conn: ownedPool})
	}

	gdb, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		if ownedPool != nil {
			_ = ownedPool.Close()
		}
		return nil, fmt.Errorf("db: open mysql: %w", err)
	}
	applyPool(gdb, o.MaxOpenConns, o.MaxIdleConns, o.ConnMaxLifetime, o.ConnMaxIdleTime)
	return gdb, nil
}

// mysqlDriverConfig builds the driver config with the write baseline
// pinned on both halves — UTC by default, one fixed numeric offset
// under mysql.time_zone (both halves carry the SAME offset; named
// zones never reach here, Validate rejects them because DST would
// reintroduce the fold #17 eliminated).
func mysqlDriverConfig(o *MySQLOptions, tlsName string, readOnly bool) (*gomysql.Config, error) {
	loc, sessionTZ, err := parseMySQLTimeZone(o.TimeZone)
	if err != nil {
		return nil, err // unreachable after Validate; kept for direct callers
	}
	params := map[string]string{
		"charset": "utf8mb4",
		// Write baseline, server half — the same offset as the driver
		// Loc below. Session time_zone governs what the driver's Loc
		// cannot reach: SQL-evaluated timestamps (CURRENT_TIMESTAMP /
		// NOW(), which write the soft-delete deleted_at) and
		// TIMESTAMP-column conversion. Left unset it inherits the
		// server's global zone, forking those values onto a second
		// baseline whenever the server zone differs from the configured
		// one. A numeric offset needs no server tz tables.
		"time_zone": sessionTZ,
	}
	if readOnly {
		// go-sql-driver applies arbitrary Params as SET statements whenever a
		// connection is established. This is a session-level backstop; server
		// read-only credentials remain authoritative.
		params["transaction_read_only"] = "1"
	}
	return &gomysql.Config{
		User:      o.Username,
		Passwd:    o.Password,
		Net:       "tcp",
		Addr:      fmt.Sprintf("%s:%d", o.Host, o.Port),
		DBName:    o.Database,
		Params:    params,
		ParseTime: true,
		// Write baseline, driver half (arch-backlog #17; the offset
		// knob is the decision doc's §3-C addendum). DATETIME stores a
		// naked wall clock; Loc decides WHICH wall clock the driver
		// writes and how it parses one back. The default is UTC —
		// instant → wall clock stays injective (no DST fold) and
		// identical across processes, so ordering / range filters /
		// cursors / aggregates compare instants regardless of any
		// machine's TZ; time.Local (the v1 heritage) tied that
		// correctness to the deployment environment. A configured fixed
		// offset keeps every property except the rendering: offsets
		// never transition, and both baseline halves move together.
		Loc:                  loc,
		AllowNativePasswords: true,
		TLSConfig:            tlsName,
	}, nil
}

// mysqlFixedOffsetConnector hands the driver its Config directly —
// the fixed-offset baseline cannot ride a DSN string, because
// FormatDSN serialises loc=+08:00 and connection time then dies
// inside time.LoadLocation ("unknown time zone": a FixedZone name is
// not loadable — the #7 round-3 pin-test trap that forced the
// NewConnector shape). Skipping the DSN round trip skips one
// canonicalisation ParseDSN would have performed, which is replayed
// here by hand: a Params entry named charset moves into the config's
// dedicated charset slot (connection-time SET NAMES); handed over
// verbatim it would be replayed as SET charset=... and rejected by
// the server ("Unknown system variable"). time_zone and
// transaction_read_only stay in Params — the same replayed-SET
// channel the DSN path gives them.
func mysqlFixedOffsetConnector(cfg *gomysql.Config) (driver.Connector, error) {
	cfg = cfg.Clone()
	if charset := cfg.Params["charset"]; charset != "" {
		delete(cfg.Params, "charset")
		if err := cfg.Apply(gomysql.Charset(charset, "")); err != nil {
			return nil, err
		}
	}
	return gomysql.NewConnector(cfg)
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

func openPostgres(o *PostgresOptions, readOnly bool) (*gorm.DB, error) {
	dsn := o.DSN
	if dsn == "" {
		dsn = postgresKeywordDSN(o)
	}
	var dialector gorm.Dialector = postgres.Open(dsn)
	var ownedPool *sql.DB
	if readOnly {
		cfg, err := postgresConnConfig(dsn, true)
		if err != nil {
			return nil, fmt.Errorf("db: parse postgres DSN: %w", err)
		}
		ownedPool = stdlib.OpenDB(*cfg)
		dialector = postgres.New(postgres.Config{Conn: ownedPool})
	}
	gdb, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		if ownedPool != nil {
			_ = ownedPool.Close()
		}
		return nil, fmt.Errorf("db: open postgres: %w", err)
	}
	applyPool(gdb, o.MaxOpenConns, o.MaxIdleConns, o.ConnMaxLifetime, o.ConnMaxIdleTime)
	return gdb, nil
}

func postgresConnConfig(dsn string, readOnly bool) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if readOnly {
		if cfg.RuntimeParams == nil {
			cfg.RuntimeParams = make(map[string]string)
		}
		cfg.RuntimeParams["default_transaction_read_only"] = "on"
	}
	return cfg, nil
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
