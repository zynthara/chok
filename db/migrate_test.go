package db_test

import (
	"context"
	"errors"
	"hash/fnv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
)

func files(m map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for name, body := range m {
		out[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return out
}

// versioned is a fresh database in migrate: versioned.
func versioned(t *testing.T) db.Config {
	cfg := dbtest.Config(t)
	cfg.Migrate = "versioned"
	return cfg
}

func states(t *testing.T, d *db.DB) map[string]string {
	t.Helper()
	sts, err := d.MigrationStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, s := range sts {
		out[s.Owner+"/"+s.Version] = s.State
	}
	return out
}

const (
	v1 = "20260101000000"
	v2 = "20260102000000"
	v3 = "20260103000000"
)

func TestMigrations_ApplyOnceAndLedger(t *testing.T) {
	cfg := versioned(t)
	fsys := files(map[string]string{
		v1 + "_create_books.sql": "CREATE TABLE books (id INT PRIMARY KEY, title VARCHAR(50) NOT NULL);",
		v2 + "_seed.sql":         "INSERT INTO books (id, title) VALUES (1, 'one'); INSERT INTO books (id, title) VALUES (2, 'two');",
		"README.md":              "not a migration",
	})
	d := dbtest.OpenConfig(t, cfg, db.Migrations(fsys))
	var n int64
	if err := d.Unsafe(t.Context()).Raw("SELECT COUNT(*) FROM books").Scan(&n).Error; err != nil || n != 2 {
		t.Fatalf("applied: %d %v", n, err)
	}
	if got := states(t, d); got["app/"+v1] != "applied" || got["app/"+v2] != "applied" || len(got) != 2 {
		t.Fatalf("status: %v", got)
	}
	if err := d.Migrate(t.Context()); err != nil {
		t.Fatalf("reapplying is a no-op: %v", err)
	}
	if err := d.Unsafe(t.Context()).Raw("SELECT COUNT(*) FROM books").Scan(&n).Error; err != nil || n != 2 {
		t.Fatalf("each file runs once: %d %v", n, err)
	}
}

func TestMigrations_NewFileAppliesIncrementally(t *testing.T) {
	cfg := versioned(t)
	first := files(map[string]string{v1 + "_a.sql": "CREATE TABLE inc_a (id INT PRIMARY KEY);"})
	d := dbtest.OpenConfig(t, cfg, db.Migrations(first))
	_ = d.Close(t.Context())
	second := files(map[string]string{
		v1 + "_a.sql": "CREATE TABLE inc_a (id INT PRIMARY KEY);",
		v2 + "_b.sql": "CREATE TABLE inc_b (id INT PRIMARY KEY);",
	})
	d = dbtest.OpenConfig(t, cfg, db.Migrations(second))
	if !d.Unsafe(t.Context()).Migrator().HasTable("inc_b") || states(t, d)["app/"+v2] != "applied" {
		t.Fatal("the new file runs on the next open")
	}
}

func TestMigrations_DriftRefuses(t *testing.T) {
	cfg := versioned(t)
	d := dbtest.OpenConfig(t, cfg, db.Migrations(files(map[string]string{v1 + "_a.sql": "CREATE TABLE drift_a (id INT PRIMARY KEY);"})))
	_ = d.Close(t.Context())
	_, err := db.Open(cfg, db.Migrations(files(map[string]string{v1 + "_a.sql": "CREATE TABLE drift_a (id BIGINT PRIMARY KEY);"})))
	if !errors.Is(err, db.ErrChecksumDrift) || !strings.Contains(err.Error(), v1+"_a") {
		t.Fatalf("an edited applied file must refuse: %v", err)
	}
}

func TestMigrations_ChecksumIgnoresCRLF(t *testing.T) {
	cfg := versioned(t)
	d := dbtest.OpenConfig(t, cfg, db.Migrations(files(map[string]string{v1 + "_a.sql": "CREATE TABLE crlf_a (id INT PRIMARY KEY);\n"})))
	_ = d.Close(t.Context())
	if _, err := db.Open(cfg, db.Migrations(files(map[string]string{v1 + "_a.sql": "CREATE TABLE crlf_a (id INT PRIMARY KEY);\r\n"}))); err != nil {
		t.Fatalf("a checkout with CRLF line ends is the same file: %v", err)
	}
}

func TestMigrations_MissingFileRefuses(t *testing.T) {
	cfg := versioned(t)
	d := dbtest.OpenConfig(t, cfg, db.Migrations(files(map[string]string{
		v1 + "_a.sql": "CREATE TABLE gone_a (id INT PRIMARY KEY);",
		v2 + "_b.sql": "CREATE TABLE gone_b (id INT PRIMARY KEY);",
	})))
	_ = d.Close(t.Context())
	_, err := db.Open(cfg, db.Migrations(files(map[string]string{v1 + "_a.sql": "CREATE TABLE gone_a (id INT PRIMARY KEY);"})))
	if err == nil || !strings.Contains(err.Error(), "files are gone") {
		t.Fatalf("got %v", err)
	}
}

func TestMigrations_OutOfOrderRefuses(t *testing.T) {
	cfg := versioned(t)
	d := dbtest.OpenConfig(t, cfg, db.Migrations(files(map[string]string{v2 + "_b.sql": "CREATE TABLE ooo_b (id INT PRIMARY KEY);"})))
	_ = d.Close(t.Context())
	_, err := db.Open(cfg, db.Migrations(files(map[string]string{
		v1 + "_a.sql": "CREATE TABLE ooo_a (id INT PRIMARY KEY);",
		v2 + "_b.sql": "CREATE TABLE ooo_b (id INT PRIMARY KEY);",
	})))
	if err == nil || !strings.Contains(err.Error(), "older than the applied") {
		t.Fatalf("got %v", err)
	}
}

func TestMigrations_FileRules(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"stray name":     files(map[string]string{"001_init.sql": "SELECT 1;"}),
		"duplicate":      files(map[string]string{v1 + "_a.sql": "SELECT 1;", v1 + "_b.sql": "SELECT 1;"}),
		"empty":          files(map[string]string{v1 + "_a.sql": "-- nothing here\n"}),
		"bad characters": files(map[string]string{v1 + "_a b.sql": "SELECT 1;"}),
	}
	for name, fsys := range cases {
		if _, err := db.Open(versioned(t), db.Migrations(fsys)); err == nil {
			t.Errorf("%s: must fail", name)
		}
	}
}

func TestMigrations_FailureRollsBackOrResumes(t *testing.T) {
	cfg := versioned(t)
	bad := files(map[string]string{
		v1 + "_ok.sql":  "CREATE TABLE fail_ok (id INT PRIMARY KEY);",
		v2 + "_bad.sql": "CREATE TABLE fail_half (id INT PRIMARY KEY); THIS IS NOT SQL;",
	})
	_, err := db.Open(cfg, db.Migrations(bad))
	if err == nil || !strings.Contains(err.Error(), "statement 2 of 2") {
		t.Fatalf("the second statement fails: %v", err)
	}
	probe := dbtest.OpenConfig(t, func() db.Config { c := cfg; c.Migrate = "auto"; return c }())
	has := probe.Unsafe(t.Context()).Migrator().HasTable("fail_half")
	if dbtest.Driver() == "mysql" {
		if !has {
			t.Fatal("MySQL commits DDL statement by statement")
		}
	} else if has {
		t.Fatal("SQLite and Postgres apply a file in one transaction")
	}
}

// TestApplyMigrations_MySQLPartialDDL: MySQL commits each DDL statement, so
// a file that fails halfway records how far it got and the next run
// resumes there once the failing statement is fixed.
func TestApplyMigrations_MySQLPartialDDL(t *testing.T) {
	if dbtest.Driver() != "mysql" {
		t.Skip("MySQL lane only (CHOK_TEST_DRIVER=mysql)")
	}
	cfg := versioned(t)
	bad := files(map[string]string{v1 + "_partial.sql": "CREATE TABLE partial_a (id BIGINT PRIMARY KEY); THIS IS NOT SQL; CREATE TABLE partial_c (id BIGINT PRIMARY KEY);"})
	if _, err := db.Open(cfg, db.Migrations(bad)); err == nil {
		t.Fatal("the second statement fails")
	}
	off := cfg
	off.Migrate = "off"
	if _, err := db.Open(off, db.Migrations(bad)); !errors.Is(err, db.ErrNotMigrated) || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("a partial migration is not applied: %v", err)
	}
	fixed := files(map[string]string{v1 + "_partial.sql": "CREATE TABLE partial_a (id BIGINT PRIMARY KEY); CREATE TABLE partial_b (id BIGINT PRIMARY KEY); CREATE TABLE partial_c (id BIGINT PRIMARY KEY);"})
	d := dbtest.OpenConfig(t, cfg, db.Migrations(fixed))
	m := d.Unsafe(t.Context()).Migrator()
	if !m.HasTable("partial_a") || !m.HasTable("partial_b") || !m.HasTable("partial_c") {
		t.Fatal("the rerun resumes after statement 1 and finishes")
	}
	if st := states(t, d)["app/"+v1]; st != "applied" {
		t.Fatalf("state %s", st)
	}
	_ = d.Close(t.Context())
	changed := files(map[string]string{v1 + "_partial.sql": "CREATE TABLE partial_x (id BIGINT PRIMARY KEY);"})
	if _, err := db.Open(cfg, db.Migrations(changed)); !errors.Is(err, db.ErrChecksumDrift) {
		t.Fatalf("once applied, the file is fixed: %v", err)
	}
}

func TestMySQLPartialDDL_ChangedPrefixRefuses(t *testing.T) {
	if dbtest.Driver() != "mysql" {
		t.Skip("MySQL lane only (CHOK_TEST_DRIVER=mysql)")
	}
	cfg := versioned(t)
	if _, err := db.Open(cfg, db.Migrations(files(map[string]string{v1 + "_p.sql": "CREATE TABLE prefix_a (id BIGINT PRIMARY KEY); BROKEN;"}))); err == nil {
		t.Fatal("must fail")
	}
	_, err := db.Open(cfg, db.Migrations(files(map[string]string{v1 + "_p.sql": "CREATE TABLE prefix_other (id BIGINT PRIMARY KEY); SELECT 1;"})))
	if err == nil || !strings.Contains(err.Error(), "those statements have changed") {
		t.Fatalf("resuming needs the statements that ran: %v", err)
	}
}

func TestMigrations_OffChecksAndTouchesNothing(t *testing.T) {
	cfg := versioned(t)
	fsys := files(map[string]string{v1 + "_a.sql": "CREATE TABLE off_a (id INT PRIMARY KEY);"})
	off := cfg
	off.Migrate = "off"
	if _, err := db.Open(off, db.Migrations(fsys)); !errors.Is(err, db.ErrNotMigrated) {
		t.Fatalf("pending files stop a migrate: off process: %v", err)
	}
	d := dbtest.OpenConfig(t, off, db.Tables(db.Table(&note{})))
	if d.Unsafe(t.Context()).Migrator().HasTable(&note{}) {
		t.Fatal("off creates nothing")
	}
	dbtest.OpenConfig(t, cfg, db.Migrations(fsys))
	dbtest.OpenConfig(t, off, db.Migrations(fsys))
}

func TestMigrations_VersionedWithTablesNeedsFiles(t *testing.T) {
	if _, err := db.Open(versioned(t), db.Tables(db.Table(&note{}))); err == nil || !strings.Contains(err.Error(), "no migrations") {
		t.Fatalf("got %v", err)
	}
}

func TestRegisterSchema_OwnersShareOneLedger(t *testing.T) {
	cfg := versioned(t)
	app := files(map[string]string{v1 + "_app.sql": "CREATE TABLE own_app (id INT PRIMARY KEY);"})
	battery := files(map[string]string{v1 + "_battery.sql": "CREATE TABLE own_battery (id INT PRIMARY KEY);"})
	d := dbtest.OpenConfig(t, cfg, db.Migrations(app))
	if err := d.RegisterSchema("identity", nil, battery); err != nil {
		t.Fatal(err)
	}
	got := states(t, d)
	if got["app/"+v1] != "applied" || got["identity/"+v1] != "applied" {
		t.Fatalf("the same version under two owners is two migrations: %v", got)
	}
	if err := d.RegisterSchema("identity", nil, battery); err == nil {
		t.Fatal("an owner registers once")
	}
	if err := d.RegisterSchema("Bad Name", nil, nil); err == nil {
		t.Fatal("owner names are plain")
	}
	if err := d.MarkApplied(t.Context(), v1); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("a version two owners share is ambiguous: %v", err)
	}
}

func TestRegisterSchema_AutoMode(t *testing.T) {
	d := dbtest.Open(t)
	if err := d.RegisterSchema("jobs", []db.TableSpec{db.Table(&tag{}, db.SoftUnique("uk_jobs_tag", "name"))}, files(map[string]string{v1 + "_x.sql": "NOT EVEN SQL"})); err != nil {
		t.Fatalf("auto mode migrates the tables and ignores the SQL: %v", err)
	}
	if !d.Unsafe(t.Context()).Migrator().HasIndex(&tag{}, "uk_jobs_tag") {
		t.Fatal("battery tables get their SoftUnique indexes")
	}
}

func TestMarkApplied(t *testing.T) {
	cfg := versioned(t)
	fsys := files(map[string]string{v1 + "_a.sql": "CREATE TABLE mark_a (id INT PRIMARY KEY);", v2 + "_b.sql": "CREATE TABLE mark_b (id INT PRIMARY KEY);"})
	off := cfg
	off.Migrate = "off"
	// A schema repaired by hand: create mark_a outside the ledger, mark it.
	probe := dbtest.OpenConfig(t, func() db.Config { c := cfg; c.Migrate = "auto"; return c }())
	if err := probe.Unsafe(t.Context()).Exec("CREATE TABLE mark_a (id INT PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	d := dbtest.OpenConfig(t, func() db.Config { c := cfg; c.Migrate = "auto"; return c }(), db.Migrations(fsys))
	if err := d.MarkApplied(t.Context(), v1); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkApplied(t.Context(), v3); err == nil {
		t.Fatal("an unknown version is an error")
	}
	got := states(t, d)
	if got["app/"+v1] != "applied" || got["app/"+v2] != "pending" {
		t.Fatalf("status: %v", got)
	}
	dbtest.OpenConfig(t, cfg, db.Migrations(fsys)) // applies only v2
	if _, err := db.Open(off, db.Migrations(fsys)); err != nil {
		t.Fatalf("everything applied now: %v", err)
	}
}

func TestMigrationStatus_NoLedgerReadsAllPending(t *testing.T) {
	cfg := dbtest.Config(t)
	d := dbtest.OpenConfig(t, cfg, db.Migrations(files(map[string]string{v1 + "_a.sql": "SELECT 1;", v2 + "_b.sql": "SELECT 1;"})))
	sts, err := d.MigrationStatus(t.Context())
	if err != nil || len(sts) != 2 || sts[0].State != "pending" || sts[1].Version != v2 {
		t.Fatalf("got %+v %v", sts, err)
	}
}

func TestMigrations_DialectSubdirectory(t *testing.T) {
	fsys := files(map[string]string{
		"sqlite/" + v1 + "_t.sql":   "CREATE TABLE dial_t (id INTEGER PRIMARY KEY, n TEXT NOT NULL);",
		"mysql/" + v1 + "_t.sql":    "CREATE TABLE dial_t (id BIGINT PRIMARY KEY, n VARCHAR(10) NOT NULL);",
		"postgres/" + v1 + "_t.sql": "CREATE TABLE dial_t (id BIGINT PRIMARY KEY, n VARCHAR(10) NOT NULL);",
	})
	d := dbtest.OpenConfig(t, versioned(t), db.Migrations(fsys))
	if !d.Unsafe(t.Context()).Migrator().HasTable("dial_t") {
		t.Fatal("the dialect's directory is used")
	}
}

// TestMigrations_ConcurrentOpensApplyOnce: two processes starting at once
// both see the migration pending; the lock (Postgres advisory, MySQL
// GET_LOCK, SQLite's immediate transaction) lets one apply it.
func TestMigrations_ConcurrentOpensApplyOnce(t *testing.T) {
	cfg := versioned(t)
	fsys := files(map[string]string{v1 + "_counter.sql": "CREATE TABLE once_t (id INT PRIMARY KEY); INSERT INTO once_t (id) VALUES (1);"})
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			d, err := db.Open(cfg, db.Migrations(fsys))
			if err != nil {
				errs <- err
				return
			}
			_ = d.Close(context.Background())
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a concurrent open failed: %v", err)
	}
	d := dbtest.OpenConfig(t, cfg, db.Migrations(fsys))
	var n int64
	if err := d.Unsafe(t.Context()).Raw("SELECT COUNT(*) FROM once_t").Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("applied once: %d %v", n, err)
	}
}

// TestMigrationLock_HeldLockBlocksUntilDeadline holds the database-wide
// lock on one session and shows a migration waits on it.
func TestMigrationLock_HeldLockBlocksUntilDeadline(t *testing.T) {
	var lock, unlock string
	var args []any
	switch dbtest.Driver() {
	case "postgres":
		h := fnv.New64a()
		h.Write([]byte("chok:migrations"))
		lock, unlock, args = "SELECT pg_advisory_lock($1)", "SELECT pg_advisory_unlock($1)", []any{int64(h.Sum64())}
	case "mysql":
		lock, unlock = "SELECT GET_LOCK('chok:migrations', 5)", "SELECT RELEASE_LOCK('chok:migrations')"
	default:
		t.Skip("SQLite serializes through its writer")
	}
	cfg := versioned(t)
	holder := dbtest.OpenConfig(t, func() db.Config { c := cfg; c.Migrate = "auto"; return c }())
	pool, err := holder.Unsafe(t.Context()).DB()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(t.Context(), lock, args...); err != nil {
		t.Fatal(err)
	}
	d := dbtest.OpenConfig(t, func() db.Config { c := cfg; c.Migrate = "auto"; return c }(), db.Migrations(files(map[string]string{v1 + "_l.sql": "CREATE TABLE lock_t (id INT PRIMARY KEY);"})))
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	err = d.MarkApplied(ctx, v1)
	if err == nil || !strings.Contains(err.Error(), "migration lock") {
		t.Fatalf("a held lock makes the migration wait and give up: %v", err)
	}
	if _, err := conn.ExecContext(t.Context(), unlock, args...); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkApplied(t.Context(), v1); err != nil {
		t.Fatalf("after release it proceeds: %v", err)
	}
}

// TestMigrationBehavior_VersionedMatchesAuto: a battery shipping SQL for
// its tables behaves like the same tables auto-migrated — the SoftUnique
// rejects a live duplicate and a soft delete frees it, on every dialect.
func TestMigrationBehavior_VersionedMatchesAuto(t *testing.T) {
	sql := map[string]string{
		"sqlite":   "CREATE TABLE tags (id INTEGER PRIMARY KEY AUTOINCREMENT, rid VARCHAR(24) NOT NULL UNIQUE, version INTEGER NOT NULL DEFAULT 1, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, delete_token VARCHAR(24) NOT NULL DEFAULT '', name VARCHAR(50) NOT NULL, scope VARCHAR(50) NOT NULL DEFAULT ''); CREATE UNIQUE INDEX uk_tag_name ON tags (name, scope, delete_token);",
		"mysql":    "CREATE TABLE tags (id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY, rid VARCHAR(24) NOT NULL UNIQUE, version BIGINT NOT NULL DEFAULT 1, created_at DATETIME(3), updated_at DATETIME(3), deleted_at DATETIME(3), delete_token VARCHAR(24) NOT NULL DEFAULT '', name VARCHAR(50) NOT NULL, scope VARCHAR(50) NOT NULL DEFAULT ''); CREATE UNIQUE INDEX uk_tag_name ON tags (name, scope, delete_token);",
		"postgres": "CREATE TABLE tags (id BIGSERIAL PRIMARY KEY, rid VARCHAR(24) NOT NULL UNIQUE, version BIGINT NOT NULL DEFAULT 1, created_at TIMESTAMPTZ, updated_at TIMESTAMPTZ, deleted_at TIMESTAMPTZ, delete_token VARCHAR(24) NOT NULL DEFAULT '', name VARCHAR(50) NOT NULL, scope VARCHAR(50) NOT NULL DEFAULT ''); CREATE UNIQUE INDEX uk_tag_name ON tags (name, scope, delete_token);",
	}
	auto := dbtest.Open(t, db.Tables(db.Table(&tag{}, db.SoftUnique("uk_tag_name", "name", "scope"))))
	ver := dbtest.OpenConfig(t, versioned(t), db.Migrations(files(map[string]string{v1 + "_tags.sql": sql[dbtest.Driver()]})))
	for name, d := range map[string]*db.DB{"auto": auto, "versioned": ver} {
		gdb := d.Unsafe(t.Context())
		first := &tag{Name: "go"}
		if err := gdb.Create(first).Error; err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := gdb.Create(&tag{Name: "go"}).Error; err == nil {
			t.Fatalf("%s: a live duplicate must conflict", name)
		}
		gdb.Model(first).Update("delete_token", "gone")
		gdb.Delete(first)
		if err := gdb.Create(&tag{Name: "go"}).Error; err != nil {
			t.Fatalf("%s: a deleted row frees the name: %v", name, err)
		}
	}
}
