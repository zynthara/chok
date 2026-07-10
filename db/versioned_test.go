package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/zynthara/chok/v2/db/internal/testlane"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestFrameworkTables_ReturnsSortedCallerSafeCopy(t *testing.T) {
	tables := FrameworkTables()
	if !slices.IsSorted(tables) {
		t.Fatalf("framework table catalog must be sorted: %v", tables)
	}
	if len(tables) == 0 {
		t.Fatal("framework table catalog must not be empty")
	}
	wantFirst := tables[0]
	tables[0] = "caller_mutation"
	if got := FrameworkTables()[0]; got != wantFirst {
		t.Fatalf("caller mutation changed generated catalog: got %q want %q", got, wantFirst)
	}
}

func migFS(files map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for name, sql := range files {
		out[name] = &fstest.MapFile{Data: []byte(sql)}
	}
	return out
}

// --- LoadMigrations ---------------------------------------------------

func TestLoadMigrations_ParsesSortedByVersion(t *testing.T) {
	ms, err := LoadMigrations(migFS(map[string]string{
		"0002_posts.sql":  "CREATE TABLE m_posts (id BIGINT PRIMARY KEY);",
		"0001_users.sql":  "CREATE TABLE m_users (id BIGINT PRIMARY KEY);",
		"0010_extras.sql": "CREATE TABLE m_extras (id BIGINT PRIMARY KEY);",
		"README.md":       "not a migration",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("want 3 migrations, got %d", len(ms))
	}
	wantOrder := []int64{1, 2, 10}
	wantNames := []string{"users", "posts", "extras"}
	for i := range ms {
		if ms[i].Version != wantOrder[i] || ms[i].Name != wantNames[i] {
			t.Fatalf("order wrong at %d: %+v", i, ms[i])
		}
	}
}

func TestLoadMigrations_StraySQLFileFailsFast(t *testing.T) {
	_, err := LoadMigrations(migFS(map[string]string{
		"0001_ok.sql":  "SELECT 1;",
		"init-all.sql": "SELECT 2;",
	}))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("stray .sql must fail fast, got %v", err)
	}
}

func TestLoadMigrations_DuplicateVersion(t *testing.T) {
	_, err := LoadMigrations(migFS(map[string]string{
		"0001_a.sql": "SELECT 1;",
		"001_b.sql":  "SELECT 2;", // same numeric version, different padding
	}))
	if err == nil || !strings.Contains(err.Error(), "duplicate migration version") {
		t.Fatalf("duplicate version must error, got %v", err)
	}
}

func TestLoadMigrations_ChecksumNormalizesCRLF(t *testing.T) {
	lf, err := LoadMigrations(migFS(map[string]string{
		"0001_a.sql": "CREATE TABLE a (id INT);\n-- done\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	crlf, err := LoadMigrations(migFS(map[string]string{
		"0001_a.sql": "CREATE TABLE a (id INT);\r\n-- done\r\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if lf[0].Checksum != crlf[0].Checksum {
		t.Fatalf("CRLF normalization must prevent false drift: %s != %s", lf[0].Checksum, crlf[0].Checksum)
	}
}

// --- ApplyMigrations / MigrationsStatus -------------------------------

func TestApplyMigrations_AppliesOnceAndLedgers(t *testing.T) {
	h := wrapForTest(openTestDB(t))
	ctx := context.Background()
	fsys := migFS(map[string]string{
		"0001_users.sql": `
			CREATE TABLE vm_users (id BIGINT PRIMARY KEY, email VARCHAR(200));
			CREATE UNIQUE INDEX uk_vm_users_email ON vm_users (email);`,
		"0002_posts.sql": "CREATE TABLE vm_posts (id BIGINT PRIMARY KEY);",
	})

	done, err := ApplyMigrations(ctx, h, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 {
		t.Fatalf("want 2 applied, got %d", len(done))
	}
	if !h.gdb.Migrator().HasTable("vm_users") || !h.gdb.Migrator().HasTable("vm_posts") {
		t.Fatal("migrated tables missing")
	}

	// Second run: nothing pending, nothing reapplied.
	again, err := ApplyMigrations(ctx, h, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second run must be a no-op, applied %d", len(again))
	}

	st, err := MigrationsStatus(ctx, h, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Applied) != 2 || len(st.Pending) != 0 {
		t.Fatalf("status wrong: %+v", st)
	}
	if st.Applied[0].Version != 1 || st.Applied[0].Name != "users" {
		t.Fatalf("ledger row wrong: %+v", st.Applied[0])
	}
	if st.Applied[0].AppliedAt.IsZero() {
		t.Fatal("applied_at must be recorded")
	}
	// The built-in table catalog must ride along on every status surface.
	want := strings.Join(FrameworkTables(), ",")
	if got := strings.Join(st.FrameworkTables, ","); got != want {
		t.Fatalf("framework table catalog missing from status: %s", got)
	}
}

func TestMigrationsStatus_LegacyLedgerIsReadOnlyAndUnverified(t *testing.T) {
	h := wrapForTest(openTestDB(t))
	if err := h.gdb.Exec(
		"CREATE TABLE schema_migrations (version BIGINT PRIMARY KEY, name VARCHAR(255) NOT NULL, applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)",
	).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.gdb.Exec("INSERT INTO schema_migrations (version, name) VALUES (1, 'legacy')").Error; err != nil {
		t.Fatal(err)
	}
	fsys := migFS(map[string]string{"0001_legacy.sql": "SELECT 1;"})

	st, err := MigrationsStatus(context.Background(), h, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Applied) != 1 || len(st.Unverified) != 1 || st.Clean() {
		t.Fatalf("legacy row must be applied but unverified: %+v", st)
	}
	if h.gdb.Migrator().HasColumn(ledgerTable, "checksum") {
		t.Fatal("status must not upgrade the legacy ledger")
	}

	report, err := ApplyMigrationsWithReport(context.Background(), h, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Adopted) != 1 || report.Adopted[0].Checksum == "" {
		t.Fatalf("up must establish exactly one legacy checksum baseline: %+v", report)
	}
	st, err = MigrationsStatus(context.Background(), h, fsys)
	if err != nil || !st.Clean() {
		t.Fatalf("adopted legacy ledger must be clean: status=%+v err=%v", st, err)
	}
}

func TestApplyMigrations_NewFileAppliesIncrementally(t *testing.T) {
	h := wrapForTest(openTestDB(t))
	ctx := context.Background()
	v1 := migFS(map[string]string{"0001_a.sql": "CREATE TABLE inc_a (id BIGINT PRIMARY KEY);"})
	if _, err := ApplyMigrations(ctx, h, v1); err != nil {
		t.Fatal(err)
	}
	v2 := migFS(map[string]string{
		"0001_a.sql": "CREATE TABLE inc_a (id BIGINT PRIMARY KEY);",
		"0002_b.sql": "CREATE TABLE inc_b (id BIGINT PRIMARY KEY);",
	})
	done, err := ApplyMigrations(ctx, h, v2)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0].Version != 2 {
		t.Fatalf("only 0002 should apply, got %+v", done)
	}
}

func TestApplyMigrations_DriftedLedgerRefuses(t *testing.T) {
	h := wrapForTest(openTestDB(t))
	ctx := context.Background()
	if _, err := ApplyMigrations(ctx, h, migFS(map[string]string{
		"0001_a.sql": "CREATE TABLE drift_a (id BIGINT PRIMARY KEY);",
	})); err != nil {
		t.Fatal(err)
	}
	// The applied file vanishes from the set.
	_, err := ApplyMigrations(ctx, h, migFS(map[string]string{
		"0002_b.sql": "CREATE TABLE drift_b (id BIGINT PRIMARY KEY);",
	}))
	if err == nil || !strings.Contains(err.Error(), "drifted history") {
		t.Fatalf("missing applied file must refuse, got %v", err)
	}
	st, statusErr := MigrationsStatus(ctx, h, migFS(map[string]string{
		"0002_b.sql": "CREATE TABLE drift_b (id BIGINT PRIMARY KEY);",
	}))
	if statusErr != nil || len(st.Missing) != 1 {
		t.Fatalf("status must diagnose a missing file without failing: status=%+v err=%v", st, statusErr)
	}
}

func TestApplyMigrations_FailureStopsAndKeepsLedgerConsistent(t *testing.T) {
	h := wrapForTest(openTestDB(t))
	ctx := context.Background()
	fsys := migFS(map[string]string{
		"0001_ok.sql":  "CREATE TABLE fail_ok (id BIGINT PRIMARY KEY);",
		"0002_bad.sql": "CREATE TABLE fail_bad (id BIGINT PRIMARY KEY); THIS IS NOT SQL;",
		"0003_new.sql": "CREATE TABLE fail_never (id BIGINT PRIMARY KEY);",
	})
	done, err := ApplyMigrations(ctx, h, fsys)
	if err == nil || !strings.Contains(err.Error(), "0002_bad.sql") {
		t.Fatalf("bad migration must surface its file, got %v", err)
	}
	if len(done) != 1 || done[0].Version != 1 {
		t.Fatalf("only 0001 should have applied, got %+v", done)
	}
	st, err := MigrationsStatus(ctx, h, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Applied) != 1 || len(st.Dirty) != 1 || len(st.Pending) != 1 {
		t.Fatalf("ledger must retain the failed attempt as dirty: %+v", st)
	}
	if h.gdb.Migrator().HasTable("fail_never") {
		t.Fatal("0003 must not run after 0002 failed")
	}
	var fences int64
	if err := h.gdb.Raw("SELECT COUNT(*) FROM schema_migrations WHERE version = 0").Scan(&fences).Error; err != nil {
		t.Fatal(err)
	}
	if fences != 1 {
		t.Fatalf("dirty state must retain the old-engine fence, got %d", fences)
	}
	if _, err := ApplyMigrations(ctx, h, fsys); err == nil || !strings.Contains(err.Error(), "dirty migration") {
		t.Fatalf("a second up must refuse dirty state, got %v", err)
	}

	fixed := migFS(map[string]string{
		"0001_ok.sql":  "CREATE TABLE fail_ok (id BIGINT PRIMARY KEY);",
		"0002_bad.sql": "CREATE TABLE fail_bad (id BIGINT PRIMARY KEY); CREATE TABLE fail_repaired (id BIGINT PRIMARY KEY);",
		"0003_new.sql": "CREATE TABLE fail_never (id BIGINT PRIMARY KEY);",
	})
	if _, err := RepairMigration(ctx, h, fixed, RepairOptions{
		Action: RepairRetry, Version: 2, ExpectedChecksum: strings.Repeat("0", 64),
		Reason: "stale operator view",
	}); err == nil || !strings.Contains(err.Error(), "checksum changed") {
		t.Fatalf("repair must CAS the inspected checksum, got %v", err)
	}
	if _, err := RepairMigration(ctx, h, fixed, RepairOptions{
		Action: RepairRetry, Version: 2, ExpectedChecksum: st.Dirty[0].Checksum,
		Reason: "restored transactional database after failed test migration",
	}); err != nil {
		t.Fatal(err)
	}
	done, err = ApplyMigrations(ctx, h, fixed)
	if err != nil || len(done) != 2 {
		t.Fatalf("retry must allow the corrected file and later migration: done=%+v err=%v", done, err)
	}
}

func TestRepairMigration_MarkAppliedAndAcceptDrift(t *testing.T) {
	h := wrapForTest(openTestDB(t))
	ctx := context.Background()
	bad := migFS(map[string]string{
		"0001_manual.sql": "CREATE TABLE repair_manual (id BIGINT PRIMARY KEY); THIS IS NOT SQL;",
	})
	if _, err := ApplyMigrations(ctx, h, bad); err == nil {
		t.Fatal("bad migration must fail")
	}
	st, err := MigrationsStatus(ctx, h, bad)
	if err != nil || len(st.Dirty) != 1 {
		t.Fatalf("want one dirty migration: status=%+v err=%v", st, err)
	}
	// SQLite rolled the file back; emulate the operator manually completing
	// every intended effect before choosing mark-applied.
	if err := h.gdb.Exec("CREATE TABLE repair_manual (id BIGINT PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RepairMigration(ctx, h, bad, RepairOptions{
		Action: RepairMarkApplied, Version: 1, ExpectedChecksum: st.Dirty[0].Checksum,
		Reason: "verified the table and completed the migration manually",
	}); err != nil {
		t.Fatal(err)
	}
	st, err = MigrationsStatus(ctx, h, bad)
	if err != nil || !st.Clean() {
		t.Fatalf("mark-applied must produce a clean row: status=%+v err=%v", st, err)
	}

	changed := migFS(map[string]string{
		"0001_manual.sql": "CREATE TABLE repair_manual (id BIGINT PRIMARY KEY); -- intentionally rebaselined\n",
	})
	st, err = MigrationsStatus(ctx, h, changed)
	if err != nil || len(st.Drift) != 1 {
		t.Fatalf("changed file must drift: status=%+v err=%v", st, err)
	}
	oldChecksum := st.Drift[0].Ledger
	if _, err := RepairMigration(ctx, h, changed, RepairOptions{
		Action: RepairAcceptDrift, Version: 1, ExpectedChecksum: oldChecksum,
		Reason: "reviewed and approved the comment-only file change",
	}); err != nil {
		t.Fatal(err)
	}
	st, err = MigrationsStatus(ctx, h, changed)
	if err != nil || !st.Clean() {
		t.Fatalf("accepted drift must be clean: status=%+v err=%v", st, err)
	}
}

func TestApplyMigrations_MySQLPartialDDL(t *testing.T) {
	dsn := os.Getenv("CHOK_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("CHOK_TEST_MYSQL_DSN is unset")
	}
	cfg, err := gomysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("chok_migration_%d_%d", os.Getpid(), time.Now().UnixNano())
	adminCfg := *cfg
	adminCfg.DBName = ""
	admin, err := sql.Open("mysql", adminCfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec("CREATE DATABASE `" + dbName + "`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE `" + dbName + "`") })
	cfg.DBName = dbName
	cfg.ParseTime = true
	gdb, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if pool, err := gdb.DB(); err == nil {
		t.Cleanup(func() { _ = pool.Close() })
	}
	h := wrapForTest(gdb)
	ctx := context.Background()
	bad := migFS(map[string]string{
		"0001_partial.sql": "CREATE TABLE mysql_partial (id BIGINT PRIMARY KEY); THIS IS NOT SQL;",
	})
	if _, err := ApplyMigrations(ctx, h, bad); err == nil {
		t.Fatal("MySQL migration must fail on the second statement")
	}
	if !gdb.Migrator().HasTable("mysql_partial") {
		t.Fatal("MySQL first DDL must remain committed after the later statement fails")
	}
	st, err := MigrationsStatus(ctx, h, bad)
	if err != nil || len(st.Dirty) != 1 {
		t.Fatalf("partial MySQL DDL must retain dirty state: status=%+v err=%v", st, err)
	}
	if _, err := ApplyMigrations(ctx, h, bad); err == nil || !strings.Contains(err.Error(), "dirty migration") {
		t.Fatalf("rerun must fail closed on MySQL dirty state, got %v", err)
	}
	if err := gdb.Exec("DROP TABLE mysql_partial").Error; err != nil {
		t.Fatal(err)
	}
	fixed := migFS(map[string]string{
		"0001_partial.sql": "CREATE TABLE mysql_partial (id BIGINT PRIMARY KEY);",
	})
	if _, err := RepairMigration(ctx, h, fixed, RepairOptions{
		Action: RepairRetry, Version: 1, ExpectedChecksum: st.Dirty[0].Checksum,
		Reason: "removed partially committed MySQL DDL before retry",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyMigrations(ctx, h, fixed); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationsStatus_OutOfOrderAndNameDrift(t *testing.T) {
	h := wrapForTest(openTestDB(t))
	ctx := context.Background()
	base := migFS(map[string]string{
		"0001_a.sql": "CREATE TABLE order_a (id BIGINT PRIMARY KEY);",
		"0003_c.sql": "CREATE TABLE order_c (id BIGINT PRIMARY KEY);",
	})
	if _, err := ApplyMigrations(ctx, h, base); err != nil {
		t.Fatal(err)
	}
	outOfOrder := migFS(map[string]string{
		"0001_a.sql": "CREATE TABLE order_a (id BIGINT PRIMARY KEY);",
		"0002_b.sql": "CREATE TABLE order_b (id BIGINT PRIMARY KEY);",
		"0003_c.sql": "CREATE TABLE order_c (id BIGINT PRIMARY KEY);",
	})
	st, err := MigrationsStatus(ctx, h, outOfOrder)
	if err != nil || len(st.OutOfOrder) != 1 || len(st.Pending) != 0 {
		t.Fatalf("late lower version must be out-of-order: status=%+v err=%v", st, err)
	}
	if _, err := ApplyMigrations(ctx, h, outOfOrder); err == nil || !strings.Contains(err.Error(), "out-of-order") {
		t.Fatalf("up must reject out-of-order migration, got %v", err)
	}

	renamed := migFS(map[string]string{
		"0001_renamed.sql": "CREATE TABLE order_a (id BIGINT PRIMARY KEY);",
		"0003_c.sql":       "CREATE TABLE order_c (id BIGINT PRIMARY KEY);",
	})
	st, err = MigrationsStatus(ctx, h, renamed)
	if err != nil || len(st.NameDrift) != 1 {
		t.Fatalf("same version under a new name must be diagnosed: status=%+v err=%v", st, err)
	}
}

func TestApplyMigrations_EmptyFileErrors(t *testing.T) {
	h := wrapForTest(openTestDB(t))
	_, err := ApplyMigrations(context.Background(), h, migFS(map[string]string{
		"0001_empty.sql": "-- only a comment\n",
	}))
	if err == nil || !strings.Contains(err.Error(), "no statements") {
		t.Fatalf("statement-free migration must error, got %v", err)
	}
	st, statusErr := MigrationsStatus(context.Background(), h, migFS(map[string]string{
		"0001_empty.sql": "-- only a comment\n",
	}))
	if statusErr != nil || len(st.Dirty) != 0 {
		t.Fatalf("preflight-invalid file must not leave dirty state: status=%+v err=%v", st, statusErr)
	}
}

func TestMigrationsStatus_NoLedgerReadsAllPending(t *testing.T) {
	h := wrapForTest(openTestDB(t))
	st, err := MigrationsStatus(context.Background(), h, migFS(map[string]string{
		"0001_a.sql": "SELECT 1;",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Applied) != 0 || len(st.Pending) != 1 {
		t.Fatalf("fresh database must be all-pending: %+v", st)
	}
}

func TestApplyMigrations_FinalizationRequiresDirtyRow(t *testing.T) {
	h := wrapForTest(openTestDB(t))
	fsys := migFS(map[string]string{
		"0001_tamper.sql": "DELETE FROM schema_migrations WHERE version = 1; CREATE TABLE finalize_guard (id BIGINT PRIMARY KEY);",
	})
	_, err := ApplyMigrations(context.Background(), h, fsys)
	if err == nil || !strings.Contains(err.Error(), "ownership lost") {
		t.Fatalf("final UPDATE must fail when its dirty row vanished, got %v", err)
	}
	if h.gdb.Migrator().HasTable("finalize_guard") {
		t.Fatal("transactional dialect must roll schema back when final ledger CAS misses")
	}
	st, statusErr := MigrationsStatus(context.Background(), h, fsys)
	if statusErr != nil || len(st.Dirty) != 1 {
		t.Fatalf("original dirty marker must survive the rolled-back tamper: status=%+v err=%v", st, statusErr)
	}
}

func TestApplyOne_SQLiteRefreshesLeaseAtTransactionEnd(t *testing.T) {
	if testlane.Driver() != "sqlite" {
		t.Skip("sqlite-lane only")
	}
	h := wrapForTest(openTestDB(t))
	ctx := context.Background()
	if err := ensureLedgerBase(h.gdb); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireMigrationLock(ctx, h.gdb)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.release()
	if err := ensureLedgerColumns(h.gdb); err != nil {
		t.Fatal(err)
	}
	migrations, err := LoadMigrations(migFS(map[string]string{
		"0001_refresh.sql": "UPDATE schema_migrations SET applied_at = '1970-01-01T00:00:00Z' WHERE version = 0; CREATE TABLE lease_refresh_guard (id BIGINT PRIMARY KEY);",
	}))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	if err := applyOne(ctx, h.gdb, migrations[0], lock.owner); err != nil {
		t.Fatal(err)
	}
	var refreshed time.Time
	if err := h.gdb.Raw(
		"SELECT applied_at FROM schema_migrations WHERE version = 0 AND name = ?",
		lock.owner,
	).Scan(&refreshed).Error; err != nil {
		t.Fatal(err)
	}
	if refreshed.Before(started) {
		t.Fatalf("transaction-end refresh did not replace the stale in-transaction timestamp: got %s, started %s", refreshed, started)
	}
}

// --- statement splitter ------------------------------------------------

func TestSplitSQLStatements(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  int
		check func(t *testing.T, stmts []string)
	}{
		{"two plain", "CREATE TABLE a (id INT); CREATE TABLE b (id INT);", 2, nil},
		{"trailing no semi", "SELECT 1; SELECT 2", 2, nil},
		{"semicolon in string", "INSERT INTO t VALUES ('a;b'); SELECT 1;", 2, func(t *testing.T, s []string) {
			if !strings.Contains(s[0], "'a;b'") {
				t.Fatalf("string literal split: %q", s[0])
			}
		}},
		{"escaped quote doubling", "INSERT INTO t VALUES ('it''s; fine'); SELECT 1;", 2, nil},
		{"backslash escape", `INSERT INTO t VALUES ('a\'; b'); SELECT 1;`, 2, nil},
		{"line comment", "SELECT 1; -- comment; with semicolons\nSELECT 2;", 2, nil},
		{"block comment", "SELECT 1; /* multi;\nline; */ SELECT 2;", 2, nil},
		{"quoted identifier", `CREATE TABLE "we;ird" (id INT); SELECT 1;`, 2, nil},
		{"backtick identifier", "CREATE TABLE `we;ird` (id INT); SELECT 1;", 2, nil},
		{"dollar quoted body", `CREATE FUNCTION f() RETURNS void AS $$
BEGIN
  UPDATE t SET x = 1;
  UPDATE t SET y = 2;
END;
$$ LANGUAGE plpgsql; SELECT 1;`, 2, func(t *testing.T, s []string) {
			if !strings.Contains(s[0], "SET y = 2;") {
				t.Fatalf("dollar-quoted body split: %q", s[0])
			}
		}},
		{"tagged dollar quote", `DO $tag$ SELECT 1; SELECT 2; $tag$; SELECT 3;`, 2, nil},
		{"only comments", "-- nothing\n/* here */", 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitSQLStatements(tt.in)
			if len(got) != tt.want {
				t.Fatalf("want %d statements, got %d: %q", tt.want, len(got), got)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

// --- migration lock branches -------------------------------------------

// TestMigrationLock_PostgresAdvisory proves cross-connection mutual
// exclusion: while one session holds the advisory lock, a second
// ApplyMigrations blocks until release (postgres lane only).
func TestMigrationLock_PostgresAdvisory(t *testing.T) {
	if testlane.Driver() != "postgres" {
		t.Skip("postgres-lane only (CHOK_TEST_DRIVER=postgres)")
	}
	gdb := openTestDB(t)
	h := wrapForTest(gdb)
	ctx := context.Background()

	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	holder, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, "SELECT pg_advisory_lock($1)", pgAdvisoryLockKey); err != nil {
		t.Fatal(err)
	}

	fsys := migFS(map[string]string{"0001_a.sql": "CREATE TABLE lock_a (id BIGINT PRIMARY KEY);"})

	var mu sync.Mutex
	var appliedAt time.Time
	donech := make(chan error, 1)
	go func() {
		_, err := ApplyMigrations(ctx, h, fsys)
		mu.Lock()
		appliedAt = time.Now()
		mu.Unlock()
		donech <- err
	}()

	select {
	case err := <-donech:
		t.Fatalf("ApplyMigrations must block while the advisory lock is held (returned %v)", err)
	case <-time.After(300 * time.Millisecond):
	}

	releasedAt := time.Now()
	if _, err := holder.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", pgAdvisoryLockKey); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-donech:
		if err != nil {
			t.Fatalf("ApplyMigrations after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ApplyMigrations still blocked after lock release")
	}
	mu.Lock()
	defer mu.Unlock()
	if appliedAt.Before(releasedAt) {
		t.Fatal("migration finished before the lock was released")
	}
}

func TestMigrationLock_SQLiteLeaseSerializesHandles(t *testing.T) {
	if testlane.Driver() != "sqlite" {
		t.Skip("sqlite-lane only")
	}
	path := t.TempDir() + "/migration-lease.db"
	h1, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	defer h1.Close()
	h2, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()

	lock1, err := acquireMigrationLock(context.Background(), h1.gdb)
	if err != nil {
		t.Fatal(err)
	}
	st, err := MigrationsStatus(context.Background(), h1, migFS(map[string]string{}))
	if err != nil || st.Fence == nil || st.Fence.Owner != lock1.owner || st.Clean() {
		t.Fatalf("status must expose the live sqlite fence: status=%+v err=%v", st, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := acquireMigrationLock(ctx, h2.gdb); err == nil || !strings.Contains(err.Error(), "sqlite migration lease") {
		t.Fatalf("second handle must wait for the live sqlite lease, got %v", err)
	}
	lock1.release()

	lock2, err := acquireMigrationLock(context.Background(), h2.gdb)
	if err != nil {
		t.Fatalf("lease must be acquirable after release: %v", err)
	}
	lock2.release()

	stale, err := acquireMigrationLock(context.Background(), h1.gdb)
	if err != nil {
		t.Fatal(err)
	}
	if err := h1.gdb.Exec(
		"UPDATE schema_migrations SET applied_at = ? WHERE version = 0 AND name = ?",
		time.Unix(0, 0).UTC(), stale.owner,
	).Error; err != nil {
		t.Fatal(err)
	}
	takeoverCtx, takeoverCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer takeoverCancel()
	taken, err := acquireMigrationLock(takeoverCtx, h2.gdb)
	if err != nil {
		t.Fatalf("stale lease must be reclaimable: %v", err)
	}
	stale.release() // owner CAS must not delete the replacement lease.
	if err := verifyMigrationLease(h2.gdb, taken.owner); err != nil {
		t.Fatalf("old owner release removed the replacement lease: %v", err)
	}
	taken.release()
}
