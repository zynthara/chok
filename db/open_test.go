package db

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/db/internal/testlane"
)

// writeTestCA writes a self-signed CA certificate PEM and returns its
// path — enough for mysqlTLSConfig's pool-building path (no live MySQL
// needed to verify registration mechanics).
func writeTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "chok-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	var buf strings.Builder
	if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMySQLTLSConfig_NoCA_PassesThroughTLSString(t *testing.T) {
	for _, mode := range []string{"", "true", "skip-verify", "preferred"} {
		got, err := mysqlTLSConfig(&MySQLOptions{Host: "h", TLS: mode})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if got != mode {
			t.Fatalf("mode %q must pass through, got %q", mode, got)
		}
	}
}

func TestMySQLTLSConfig_CACert_RegistersVerifyingConfig(t *testing.T) {
	ca := writeTestCA(t)
	host := "db-tls-test.internal"
	name, err := mysqlTLSConfig(&MySQLOptions{Host: host, TLS: "skip-verify", CACert: ca})
	if err != nil {
		t.Fatal(err)
	}
	// CACert takes precedence over the TLS string (toffs v0.4.2
	// semantics): the returned value is the per-host registration key,
	// not "skip-verify".
	if name != "chok-mysql-"+host {
		t.Fatalf("registration key must be per-host, got %q", name)
	}

	// The key must wire into the DSN — that's how the driver picks the
	// registered verifying config up.
	dsn := (&gomysql.Config{User: "u", Net: "tcp", Addr: "h:3306", DBName: "d", TLSConfig: name}).FormatDSN()
	if !strings.Contains(dsn, "tls=chok-mysql-"+host) {
		t.Fatalf("DSN must reference the registered TLS config, got %s", dsn)
	}

	// Re-registering the same host must be idempotent (in-process
	// restarts, repeated Opens).
	if _, err := mysqlTLSConfig(&MySQLOptions{Host: host, CACert: ca}); err != nil {
		t.Fatalf("re-register same host: %v", err)
	}
}

func TestMySQLTLSConfig_BadCA(t *testing.T) {
	if _, err := mysqlTLSConfig(&MySQLOptions{Host: "h", CACert: "/nonexistent/ca.pem"}); err == nil {
		t.Fatal("missing CA file must error")
	}
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mysqlTLSConfig(&MySQLOptions{Host: "h", CACert: garbage}); err == nil {
		t.Fatal("PEM-free CA file must error")
	}
}

func TestReadOnly_DriverSessionDefaults(t *testing.T) {
	mysqlCfg := mysqlDriverConfig(&MySQLOptions{Host: "h", Port: 3306, Database: "d"}, "", true)
	if mysqlCfg.Params["transaction_read_only"] != "1" {
		t.Fatalf("mysql read-only session default missing: %#v", mysqlCfg.Params)
	}
	for name, dsn := range map[string]string{
		"url":     "postgres://u:p@localhost/d?sslmode=disable",
		"keyword": "host='localhost' user='u' password='p' dbname='d' sslmode='disable'",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := postgresConnConfig(dsn, true)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RuntimeParams["default_transaction_read_only"] != "on" {
				t.Fatalf("postgres read-only runtime param missing: %#v", cfg.RuntimeParams)
			}
		})
	}
}

var errAbort = errors.New("abort")

type readOnlyHookItem struct {
	Model
	Code    string
	HookRan *bool `gorm:"-"`
}

func (readOnlyHookItem) RIDPrefix() string { return "roh" }

func (i *readOnlyHookItem) BeforeCreate(*gorm.DB) error {
	*i.HookRan = true
	return nil
}

func TestOpen_SQLiteHandleWorks(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: ":memory:"}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	ctx := context.Background()
	if err := h.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := h.Migrate(ctx, Table(&TestItem{})); err != nil {
		t.Fatalf("handle Migrate: %v", err)
	}
	if err := h.Unsafe(ctx).Create(&TestItem{Code: "H1"}).Error; err != nil {
		t.Fatalf("Unsafe create: %v", err)
	}

	// Unsafe must be transaction-aware: inside RunInTx it returns the
	// tx handle, so raw escape-hatch code joins the transaction and a
	// rollback takes its writes with it.
	err = h.RunInTx(ctx, func(txCtx context.Context) error {
		if err := h.Unsafe(txCtx).Create(&TestItem{Code: "H2"}).Error; err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("expected abort error, got %v", err)
	}
	var count int64
	h.Unsafe(ctx).Model(&TestItem{}).Count(&count)
	if count != 1 {
		t.Fatalf("Unsafe write inside rolled-back tx must vanish; rows=%d", count)
	}
}

func TestOpen_ValidatesFirst(t *testing.T) {
	if _, err := Open(Options{Driver: "sqlite"}); err == nil {
		t.Fatal("Open must validate options (empty sqlite path)")
	}
	if _, err := Open(Options{Driver: "oracle"}); err == nil {
		t.Fatal("Open must reject unknown drivers")
	}
}

func TestReadOnly_SQLiteGuardsEveryWritePath(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "readonly.db")
	writable, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.Migrate(ctx, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}
	if err := writable.Unsafe(ctx).Create(&TestItem{Code: "seed"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := Open(Options{Driver: "sqlite", ReadOnly: true, SQLite: SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	if !ro.ReadOnly() {
		t.Fatal("ReadOnly must report configured capability")
	}
	hookRan := false
	if err := ro.Unsafe(ctx).Create(&readOnlyHookItem{Code: "hook", HookRan: &hookRan}).Error; !errors.Is(err, ErrReadOnly) {
		t.Fatalf("hooked create: want ErrReadOnly, got %v", err)
	}
	if hookRan {
		t.Fatal("read-only callback must run before GORM model hooks")
	}
	var count int64
	if err := ro.Unsafe(ctx).Model(&TestItem{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("read query: count=%d err=%v", count, err)
	}
	for name, err := range map[string]error{
		"create":     ro.Unsafe(ctx).Create(&TestItem{Code: "blocked"}).Error,
		"exec":       ro.Unsafe(ctx).Exec("INSERT INTO test_items (code) VALUES ('blocked')").Error,
		"raw insert": ro.Unsafe(ctx).Raw("INSERT INTO test_items (code) VALUES ('blocked') RETURNING id").Scan(&struct{ ID uint }{}).Error,
		"with write": ro.Unsafe(ctx).Raw("WITH x AS (SELECT 1) DELETE FROM test_items RETURNING id").Scan(&struct{ ID uint }{}).Error,
		"locking":    ro.Unsafe(ctx).Raw("SELECT * FROM test_items FOR UPDATE").Scan(&[]TestItem{}).Error,
		"transaction": ro.RunInTx(ctx, func(context.Context) error {
			return nil
		}),
		"migrate": ro.Migrate(ctx, Table(&TestItem{})),
	} {
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s: want ErrReadOnly, got %v", name, err)
		}
	}

	sqlDB, err := ro.gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only=OFF"); err != nil {
		t.Fatalf("disabling soft query_only guard should be allowed so mode=ro is tested: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO test_items (code) VALUES ('raw-bypass')"); err == nil {
		t.Fatal("mode=ro must reject writes even after query_only is disabled")
	}
}

func TestReadOnly_ForeignTransactionDoesNotReplaceHandle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "affinity.db")
	writable, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writable.Close() })
	if err := writable.Migrate(ctx, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}
	ro, err := Open(Options{Driver: "sqlite", ReadOnly: true, SQLite: SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	err = writable.RunInTx(ctx, func(txCtx context.Context) error {
		var n int64
		if err := ro.Unsafe(txCtx).Model(&TestItem{}).Count(&n).Error; err != nil {
			return err
		}
		return ro.Unsafe(txCtx).Create(&TestItem{Code: "must-not-hit-primary"}).Error
	})
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("foreign writable tx must not bypass read-only handle, got %v", err)
	}
}

func TestReadOnly_PostgresDriverBackstop(t *testing.T) {
	if testlane.Driver() != "postgres" {
		t.Skip("Postgres lane only")
	}
	ctx := context.Background()
	dsn := testlane.PostgresDSN(t)
	writable, err := Open(Options{Driver: "postgres", Postgres: PostgresOptions{DSN: dsn}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writable.Close() })
	if err := writable.Migrate(ctx, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}

	ro, err := Open(Options{Driver: "postgres", ReadOnly: true, Postgres: PostgresOptions{DSN: dsn}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	sqlDB, err := ro.gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, "INSERT INTO test_items (code) VALUES ('driver-bypass')"); err == nil {
		t.Fatal("Postgres startup runtime parameter must reject naked database/sql writes")
	}
}

func TestReadOnly_MySQLDriverBackstop(t *testing.T) {
	dsn := os.Getenv("CHOK_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("CHOK_TEST_MYSQL_DSN is unset")
	}
	cfg, err := gomysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	table := fmt.Sprintf("chok_readonly_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE TABLE " + table + " (id BIGINT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP TABLE IF EXISTS " + table) })

	roCfg := cfg.Clone()
	if roCfg.Params == nil {
		roCfg.Params = make(map[string]string)
	}
	roCfg.Params["transaction_read_only"] = "1"
	ro, err := sql.Open("mysql", roCfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	if err := ro.Ping(); err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Exec("INSERT INTO " + table + " (id) VALUES (1)"); err == nil {
		t.Fatal("MySQL transaction_read_only session default must reject naked database/sql writes")
	}
}

func TestSQLiteReadOnlyDSN_RejectsWritableMode(t *testing.T) {
	if _, err := sqliteReadOnlyDSN("file:test.db?mode=rwc"); err == nil {
		t.Fatal("read_only must reject a conflicting writable SQLite mode")
	}
}

// TestOpen_MemorySQLiteSurvivesConcurrentPoolUse pins the :memory:
// pool fix: without capping the pool to one immortal connection, every
// connection database/sql opens beyond the first is a fresh empty
// database, and concurrent use (an async sink goroutine next to the
// caller) intermittently fails with "no such table". Eight goroutines
// hammering queries force pool growth deterministically pre-fix.
func TestOpen_MemorySQLiteSurvivesConcurrentPoolUse(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: ":memory:"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	ctx := context.Background()
	if err := h.Migrate(ctx, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				if err := h.Unsafe(ctx).Create(&TestItem{Code: fmt.Sprintf("g%d-%d", g, i)}).Error; err != nil {
					errCh <- err
					return
				}
				var n int64
				if err := h.Unsafe(ctx).Model(&TestItem{}).Count(&n).Error; err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		t.Fatalf("concurrent pool use over :memory: must not lose the schema: %v", err)
	}
}

// TestOpenSQLite_InjectsConcurrencyDefaults pins the file-database DSN
// defaults on both pools: synchronous lands on NORMAL (WAL-safe,
// several times FULL's write throughput), foreign_keys is enforced
// (Postgres-lane parity), and the driver's own busy_timeout default
// stays at 5000ms (glebarez matches mattn) — a driver upgrade
// silently dropping any of these should fail here.
func TestOpenSQLite_InjectsConcurrencyDefaults(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: filepath.Join(t.TempDir(), "d.db")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	ctx := context.Background()

	// Write pool (PRAGMA rides the write side — see maintenance.go).
	var sync int
	if err := h.Unsafe(ctx).Raw("PRAGMA synchronous").Scan(&sync).Error; err != nil {
		t.Fatal(err)
	}
	if sync != 1 { // 1 = NORMAL
		t.Fatalf("synchronous = %d, want 1 (NORMAL)", sync)
	}
	var busy int
	if err := h.Unsafe(ctx).Raw("PRAGMA busy_timeout").Scan(&busy).Error; err != nil {
		t.Fatal(err)
	}
	if busy != 5000 {
		t.Fatalf("busy_timeout = %d, want the driver default 5000", busy)
	}
	var journal string
	if err := h.Unsafe(ctx).Raw("PRAGMA journal_mode").Scan(&journal).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}
	var fk int
	if err := h.Unsafe(ctx).Raw("PRAGMA foreign_keys").Scan(&fk).Error; err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1 (write pool)", fk)
	}

	// Read pool: per-connection pragmas must land there too.
	if h.readPool == nil {
		t.Fatal("file database must run the read/write split")
	}
	for pragma, want := range map[string]int{"foreign_keys": 1, "busy_timeout": 5000} {
		var got int
		if err := h.readPool.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("read pool %s = %d, want %d", pragma, got, want)
		}
	}
}

// TestOpenSQLite_UserDSNOverridesDefaults: explicit _pragma DSN
// parameters win over the injected defaults, both value spellings.
func TestOpenSQLite_UserDSNOverridesDefaults(t *testing.T) {
	for name, tc := range map[string]struct {
		params string
		want   int
	}{
		"parens": {"_pragma=synchronous(2)", 2}, // FULL
		"equals": {"_pragma=synchronous=0", 0},  // OFF
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "d.db") + "?" + tc.params
			h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: path}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.Close() })
			var sync int
			if err := h.Unsafe(context.Background()).Raw("PRAGMA synchronous").Scan(&sync).Error; err != nil {
				t.Fatal(err)
			}
			if sync != tc.want {
				t.Fatalf("synchronous = %d, want %d (user DSN must win)", sync, tc.want)
			}
		})
	}
}

// TestOpenSQLite_LegacyMattnParamsRejected: mattn/go-sqlite3 DSN
// spellings fail Open with a pointer to the _pragma form. The pure-Go
// driver ignores parameters it does not know, so accepting them would
// silently drop the user's tuning — fail-fast beats silent drift.
func TestOpenSQLite_LegacyMattnParamsRejected(t *testing.T) {
	for _, params := range []string{
		"_synchronous=NORMAL", "_sync=2", "_busy_timeout=100",
		"_journal_mode=WAL", "_foreign_keys=1",
	} {
		path := filepath.Join(t.TempDir(), "d.db") + "?" + params
		_, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: path}})
		if err == nil || !strings.Contains(err.Error(), "_pragma") {
			t.Fatalf("params %q: want rejection pointing at _pragma, got %v", params, err)
		}
	}
}

// TestOpenSQLite_ForeignKeysEnforced: declared references reject
// orphan rows — the same behaviour the Postgres lane always had.
// Covers both the file and the memory shape (the test lane).
func TestOpenSQLite_ForeignKeysEnforced(t *testing.T) {
	for name, path := range map[string]string{
		"file":   filepath.Join(t.TempDir(), "d.db"),
		"memory": ":memory:",
	} {
		t.Run(name, func(t *testing.T) {
			h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: path}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.Close() })
			ctx := context.Background()
			if err := h.Unsafe(ctx).Exec(`CREATE TABLE fk_parents (id INTEGER PRIMARY KEY)`).Error; err != nil {
				t.Fatal(err)
			}
			if err := h.Unsafe(ctx).Exec(`CREATE TABLE fk_children (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES fk_parents(id))`).Error; err != nil {
				t.Fatal(err)
			}
			err = h.Unsafe(ctx).Exec(`INSERT INTO fk_children (id, parent_id) VALUES (1, 42)`).Error
			if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
				t.Fatalf("orphan insert must fail the FOREIGN KEY constraint, got %v", err)
			}
		})
	}
}

// TestOpenSQLite_ConcurrentReadModifyWrite_NoBusy is the behavioural
// proof of _txlock=immediate: transactions that read before writing
// take the write lock at BEGIN and queue under busy_timeout. Under the
// driver default (deferred) the mid-transaction lock upgrade fails
// SQLITE_BUSY immediately — ten writers hammering one row made that
// near-certain before the DSN default landed.
func TestOpenSQLite_ConcurrentReadModifyWrite_NoBusy(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: filepath.Join(t.TempDir(), "d.db")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	ctx := context.Background()
	if err := h.Migrate(ctx, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}
	if err := h.Unsafe(ctx).Create(&TestItem{Code: "seed"}).Error; err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 10)
	for g := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 5 {
				err := h.RunInTx(ctx, func(txCtx context.Context) error {
					var item TestItem
					if err := h.Unsafe(txCtx).Where("id = ?", 1).First(&item).Error; err != nil {
						return err
					}
					return h.Unsafe(txCtx).Model(&TestItem{}).Where("id = ?", 1).
						Update("code", fmt.Sprintf("g%d-%d", g, i)).Error
				})
				if err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		t.Fatalf("read-modify-write transactions must queue, not fail busy: %v", err)
	}
}

// TestOpenSQLite_SplitPoolSizes: the write pool is always exactly one
// connection (that is what makes writers queue fairly in Go instead
// of colliding on the file lock); max_open_conns sizes the read pool,
// defaulting to max(4, NumCPU).
func TestOpenSQLite_SplitPoolSizes(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{
		Path:         filepath.Join(t.TempDir(), "d.db"),
		MaxOpenConns: 3,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	writePool, err := h.Unsafe(context.Background()).DB()
	if err != nil {
		t.Fatal(err)
	}
	if got := writePool.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("write pool MaxOpenConnections = %d, want the single writer", got)
	}
	if h.readPool == nil {
		t.Fatal("file database must run the read/write split")
	}
	if got := h.readPool.Stats().MaxOpenConnections; got != 3 {
		t.Fatalf("read pool MaxOpenConnections = %d, want max_open_conns (3)", got)
	}

	hDef, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: filepath.Join(t.TempDir(), "d.db")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hDef.Close() })
	if got, want := hDef.readPool.Stats().MaxOpenConnections, max(4, runtime.NumCPU()); got != want {
		t.Fatalf("default read pool MaxOpenConnections = %d, want max(4, NumCPU) = %d", got, want)
	}
}

// TestOpenSQLite_ReadsBypassTheWriteQueue is the behavioural proof of
// the read/write split: while an open transaction occupies the single
// write connection, plain queries must still answer — dbresolver
// routes them to the read pool, where WAL snapshots do not care about
// the writer. If routing regressed to the write pool, the read below
// would block behind the held connection until its context expired.
func TestOpenSQLite_ReadsBypassTheWriteQueue(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: filepath.Join(t.TempDir(), "d.db")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	ctx := context.Background()
	if err := h.Migrate(ctx, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}
	if err := h.Unsafe(ctx).Create(&TestItem{Code: "seed"}).Error; err != nil {
		t.Fatal(err)
	}

	held := make(chan struct{})
	release := make(chan struct{})
	txErr := make(chan error, 1)
	go func() {
		txErr <- h.RunInTx(ctx, func(txCtx context.Context) error {
			if err := h.Unsafe(txCtx).Create(&TestItem{Code: "writer"}).Error; err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var n int64
	err = h.Unsafe(readCtx).Model(&TestItem{}).Count(&n).Error
	close(release)
	if err != nil {
		t.Fatalf("read must bypass the occupied write connection: %v", err)
	}
	if n != 1 {
		t.Fatalf("snapshot read saw %d rows, want 1 (uncommitted write invisible)", n)
	}
	if err := <-txErr; err != nil {
		t.Fatalf("held transaction: %v", err)
	}
}

// TestOpenSQLite_CloseClosesReadPool pins the split's lifetime: the
// read pool dies with the handle (dbresolver has no close of its own
// — leaking it would bleed file descriptors across Open/Close cycles,
// tests above all).
func TestOpenSQLite_CloseClosesReadPool(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: filepath.Join(t.TempDir(), "d.db")}})
	if err != nil {
		t.Fatal(err)
	}
	if h.readPool == nil {
		t.Fatal("file database must run the read/write split")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.readPool.Ping(); err == nil {
		t.Fatal("read pool must be closed with the handle")
	}
}
