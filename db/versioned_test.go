package db

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zynthara/chok/v2/db/internal/testlane"
)

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
	// The whitelist must ride along on every status surface.
	want := strings.Join(FrameworkTables(), ",")
	if got := strings.Join(st.FrameworkTables, ","); got != want {
		t.Fatalf("framework whitelist missing from status: %s", got)
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
	if len(st.Applied) != 1 || len(st.Pending) != 2 {
		t.Fatalf("ledger must record only the successful file: %+v", st)
	}
	if h.gdb.Migrator().HasTable("fail_never") {
		t.Fatal("0003 must not run after 0002 failed")
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

// TestMigrationLock_SQLiteNoop pins the third branch: on sqlite the
// lock is a documented no-op — acquire and release must both succeed
// instantly with no side effects.
func TestMigrationLock_SQLiteNoop(t *testing.T) {
	if testlane.Driver() != "sqlite" {
		t.Skip("sqlite-lane only")
	}
	gdb := openTestDB(t)
	release, err := acquireMigrationLock(context.Background(), gdb)
	if err != nil {
		t.Fatal(err)
	}
	release() // must not panic or error
}
