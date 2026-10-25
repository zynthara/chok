package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
	"github.com/zynthara/chok/v3/internal/dbx"
)

// project is a temporary application directory holding a chok.yaml whose
// db section points at the lane's database, next to sections the command
// must skip. The test's working directory is the project's.
func project(t *testing.T, migrate string) (dir string, cfg db.Config) {
	t.Helper()
	cfg = dbtest.Config(t)
	section := map[string]any{"driver": cfg.Driver, "migrate": migrate}
	switch cfg.Driver {
	case "sqlite":
		section["sqlite"] = map[string]any{"path": cfg.SQLite.Path}
	case "postgres":
		section["postgres"] = map[string]any{"dsn": cfg.Postgres.DSN}
	case "mysql":
		m := cfg.MySQL
		section["mysql"] = map[string]any{"host": m.Host, "port": m.Port, "username": m.Username, "password": m.Password, "database": m.Database}
	}
	doc, err := yaml.Marshal(map[string]any{
		"db":   section,
		"http": map[string]any{"addr": ":9999"},
		"auth": map[string]any{"token_ttl": "2h"},
		"mine": map[string]any{"anything": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("chok.yaml", doc, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("go.mod", []byte("module example.com/cliapp\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, cfg
}

// ddl is a statement creating table in the lane's dialect.
func ddl(driver, table string) string {
	switch driver {
	case "postgres":
		return "CREATE TABLE " + table + " (id BIGSERIAL PRIMARY KEY, title TEXT NOT NULL);\n"
	case "mysql":
		return "CREATE TABLE " + table + " (id BIGINT AUTO_INCREMENT PRIMARY KEY, title VARCHAR(200) NOT NULL);\n"
	}
	return "CREATE TABLE " + table + " (id INTEGER PRIMARY KEY, title TEXT NOT NULL);\n"
}

// create runs chok migrate create and writes body into each new file,
// returning the version.
func create(t *testing.T, name string, body func(dialect string) string) string {
	t.Helper()
	out, err := run(t, "migrate", "create", name)
	if err != nil {
		t.Fatalf("migrate create: %v\n%s", err, out)
	}
	version := ""
	for p := range strings.Lines(out) {
		p = strings.TrimSpace(p)
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(body(filepath.Base(filepath.Dir(p)))); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		version = filepath.Base(p)[:14]
	}
	return version
}

func hasTable(t *testing.T, cfg db.Config, table string) bool {
	t.Helper()
	return dbtest.OpenConfig(t, cfg).Unsafe(t.Context()).Migrator().HasTable(table)
}

// stateOf finds owner's migration version in a status listing.
func stateOf(t *testing.T, status, owner, version string) string {
	t.Helper()
	for l := range strings.Lines(status) {
		f := strings.Fields(l)
		if len(f) >= 4 && f[0] == owner && f[1] == version {
			return strings.Join(f[3:], " ")
		}
	}
	t.Fatalf("no %s %s in the status:\n%s", owner, version, status)
	return ""
}

// TestMigrateCreate_SequencesFiles: create writes one file per dialect
// with a 14-digit UTC version later than every one already there, keeps a
// top-level layout when the directory has one, takes --dialect, and
// refuses bad names.
func TestMigrateCreate_SequencesFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "migrations")
	now := time.Date(2026, 10, 25, 8, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))
	paths, err := createMigration(dir, "create_notes", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, "sqlite", "20261025000000_create_notes.sql"),
		filepath.Join(dir, "mysql", "20261025000000_create_notes.sql"),
		filepath.Join(dir, "postgres", "20261025000000_create_notes.sql"),
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths %q, want %q", paths, want)
	}
	raw, _ := os.ReadFile(paths[1])
	if !strings.HasPrefix(string(raw), "-- 20261025000000_create_notes (mysql)\n") {
		t.Fatalf("file header: %s", raw)
	}
	// The same second again, and a clock behind the newest: always later.
	paths, err = createMigration(dir, "add_index", nil, now)
	if err != nil || filepath.Base(paths[0]) != "20261025000001_add_index.sql" {
		t.Fatalf("second migration in the same second: %q %v", paths, err)
	}
	paths, err = createMigration(dir, "late", []string{"postgres"}, now.Add(-time.Hour))
	if err != nil || len(paths) != 1 || paths[0] != filepath.Join(dir, "postgres", "20261025000002_late.sql") {
		t.Fatalf("--dialect postgres with a clock behind: %q %v", paths, err)
	}

	flat := filepath.Join(t.TempDir(), "migrations")
	if err := os.MkdirAll(flat, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flat, "20261024000000_init.sql"), []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, err = createMigration(flat, "next", nil, now)
	if err != nil || !slices.Equal(paths, []string{filepath.Join(flat, "20261025000000_next.sql")}) {
		t.Fatalf("a top-level layout stays top-level: %q %v", paths, err)
	}

	for _, bad := range []string{"has space", "semi;colon", ""} {
		if _, err := createMigration(dir, bad, nil, now); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if _, err := createMigration(dir, "x", []string{"oracle"}, now); err == nil {
		t.Error("--dialect oracle accepted")
	}
}

// TestMigrateUpAndStatus_EndToEnd: on the lane's database, read from the
// db section of a chok.yaml that has other sections too and with the
// migrate mode from the environment, status lists the new migration as
// pending, up applies it once, and status lists it as applied.
func TestMigrateUpAndStatus_EndToEnd(t *testing.T) {
	_, cfg := project(t, "auto")
	t.Setenv("CLIAPP_DB_MIGRATE", "versioned") // the environment, as the app reads it
	v := create(t, "create_notes", func(dialect string) string { return ddl(dialect, "cli_notes") })

	out, err := run(t, "migrate", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "database: "+cfg.Driver+", migrate: versioned (chok.yaml)\n") {
		t.Fatalf("status header:\n%s", out)
	}
	if got := stateOf(t, out, "app", v); got != "pending" {
		t.Fatalf("before up: %s\n%s", got, out)
	}
	if hasTable(t, cfg, "cli_notes") {
		t.Fatal("status changed the database")
	}

	out, err = run(t, "migrate", "up")
	if err != nil {
		t.Fatalf("up: %v\n%s", err, out)
	}
	if out != "applied  app  "+v+"_create_notes\n" {
		t.Fatalf("up:\n%s", out)
	}
	if !hasTable(t, cfg, "cli_notes") {
		t.Fatal("up did not create the table")
	}
	out, err = run(t, "migrate", "status")
	if err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, out, "app", v); !strings.HasPrefix(got, "applied ") {
		t.Fatalf("after up: %s\n%s", got, out)
	}
	if out, err = run(t, "migrate", "up"); err != nil || out != "nothing to apply: every migration is applied\n" {
		t.Fatalf("a second up: %v\n%s", err, out)
	}
}

// TestMigrateUp_MissingConfigErrors: without a usable db section the
// command fails with the loader's message; with migrate: auto, up and
// mark-applied refuse, since the app migrates itself.
func TestMigrateUp_MissingConfigErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := run(t, "migrate", "up"); err == nil || !strings.Contains(err.Error(), "db.driver") {
		t.Fatalf("no configuration: %v", err)
	}
	if _, err := run(t, "migrate", "status", "--config", "nope.yaml"); err == nil || !strings.Contains(err.Error(), "nope.yaml") {
		t.Fatalf("a missing --config file: %v", err)
	}
	if err := os.WriteFile("typo.yaml", []byte("db:\n  driver: sqlite\n  migrat: versioned\nhttp:\n  adr: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "migrate", "status", "--config", "typo.yaml"); err == nil || !strings.Contains(err.Error(), "unknown key db.migrat (did you mean migrate?)") || strings.Contains(err.Error(), "http") {
		t.Fatalf("the db section is strict, the others are not read: %v", err)
	}

	project(t, "auto")
	for _, verb := range []string{"up", "mark-applied"} {
		args := []string{"migrate", verb}
		if verb == "mark-applied" {
			args = append(args, "20261025000000")
		}
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "db.migrate is auto in chok.yaml") {
			t.Fatalf("%s with migrate: auto: %v", verb, err)
		}
	}
	if out, err := run(t, "migrate", "status"); err != nil || !strings.Contains(out, "no versioned migrations") {
		t.Fatalf("status with migrate: auto and no files: %v\n%s", err, out)
	}
}

// TestMigrateMarkApplied_RecordsWithoutRunning: mark-applied records a
// pending migration as applied without running it; status then shows a
// changed applied file as drifted and a deleted one as missing, and up
// refuses both.
func TestMigrateMarkApplied_RecordsWithoutRunning(t *testing.T) {
	_, cfg := project(t, "versioned")
	v := create(t, "repaired_by_hand", func(dialect string) string { return ddl(dialect, "cli_repaired") })
	out, err := run(t, "migrate", "mark-applied", v)
	if err != nil {
		t.Fatalf("mark-applied: %v\n%s", err, out)
	}
	if out != "marked  app  "+v+"_repaired_by_hand  applied, without running it (it was pending)\n" {
		t.Fatalf("mark-applied:\n%s", out)
	}
	if hasTable(t, cfg, "cli_repaired") {
		t.Fatal("mark-applied ran the migration")
	}
	if _, err := run(t, "migrate", "mark-applied", "20990101000000"); err == nil || !strings.Contains(err.Error(), "no registered migration has version 20990101000000") {
		t.Fatalf("an unknown version: %v", err)
	}

	// Change the applied file: drifted, and up refuses.
	file := filepath.Join("migrations", cfg.Driver, v+"_repaired_by_hand.sql")
	if err := os.WriteFile(file, []byte(ddl(cfg.Driver, "cli_other")), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, "migrate", "status")
	if got := stateOf(t, out, "app", v); !strings.HasPrefix(got, "drifted ") {
		t.Fatalf("a changed applied file: %s\n%s", got, out)
	}
	if _, err := run(t, "migrate", "up"); !errors.Is(err, db.ErrChecksumDrift) {
		t.Fatalf("up over a drifted file: %v", err)
	}
	// Remove it: missing.
	for _, d := range dialects {
		_ = os.Remove(filepath.Join("migrations", d, v+"_repaired_by_hand.sql"))
	}
	out, _ = run(t, "migrate", "status")
	if got := stateOf(t, out, "app", v); !strings.HasPrefix(got, "missing ") {
		t.Fatalf("a deleted applied file: %s\n%s", got, out)
	}
}

// TestMigrateUp_StopsAtAFailingStatement: on MySQL, whose DDL commits as
// it runs, the statements before the failing one stay applied, status
// says how many ran, and the next up resumes after them; on SQLite and
// Postgres nothing of the file is applied.
func TestMigrateUp_StopsAtAFailingStatement(t *testing.T) {
	_, cfg := project(t, "versioned")
	v := create(t, "two_steps", func(dialect string) string {
		return ddl(dialect, "cli_first") + "CREATE TABLE cli_second (id INTEGER PRIMARY KEY,, title VARCHAR(20));\n"
	})
	if _, err := run(t, "migrate", "up"); err == nil || !strings.Contains(err.Error(), "statement 2 of 2") {
		t.Fatalf("up over a failing statement: %v", err)
	}
	out, _ := run(t, "migrate", "status")
	state := stateOf(t, out, "app", v)
	if cfg.Driver == "mysql" {
		if state != "partial (1 statements ran)" || !hasTable(t, cfg, "cli_first") {
			t.Fatalf("mysql after the failure: %q, cli_first %v\n%s", state, hasTable(t, cfg, "cli_first"), out)
		}
	} else if state != "pending" || hasTable(t, cfg, "cli_first") {
		t.Fatalf("%s after the failure: %q, cli_first %v", cfg.Driver, state, hasTable(t, cfg, "cli_first"))
	}
	// Fix the second statement; the first, already run on MySQL, is
	// unchanged, so up resumes after it.
	file := filepath.Join("migrations", cfg.Driver, v+"_two_steps.sql")
	raw, _ := os.ReadFile(file)
	fixed := strings.Replace(string(raw), "PRIMARY KEY,, title", "PRIMARY KEY, title", 1)
	if err := os.WriteFile(file, []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "migrate", "up"); err != nil || out != "applied  app  "+v+"_two_steps\n" {
		t.Fatalf("up after the fix: %v\n%s", err, out)
	}
	if !hasTable(t, cfg, "cli_first") || !hasTable(t, cfg, "cli_second") {
		t.Fatal("both tables after the fix")
	}
}

// TestMigrate_BatteryOwners: an application on migrate: off cannot start
// a battery whose migrations are pending, and the error names the
// command; up --battery applies them; from then on the ledger lists the
// battery, so status and up include it without the flag.
func TestMigrate_BatteryOwners(t *testing.T) {
	_, cfg := project(t, "off")
	off := cfg
	off.Migrate = "off"
	d := dbtest.OpenConfig(t, off)
	err := d.RegisterSchema("identity", nil, dbx.Migrations["identity"])
	if !errors.Is(err, db.ErrNotMigrated) || !strings.Contains(err.Error(), "chok migrate up --battery identity") {
		t.Fatalf("migrate: off with identity pending: %v", err)
	}

	if out, err := run(t, "migrate", "status"); err != nil || !strings.Contains(out, "no versioned migrations") {
		t.Fatalf("status before: %v\n%s", err, out)
	}
	out, err := run(t, "migrate", "up", "--battery", "identity")
	if err != nil {
		t.Fatalf("up --battery identity: %v\n%s", err, out)
	}
	if out != "applied  identity  20261024000001_create_users_and_identities\napplied  identity  20261024000002_create_audit_logs\napplied  identity  20261025000002_create_auth_login_codes\n" {
		t.Fatalf("up --battery identity:\n%s", out)
	}
	if err := dbtest.OpenConfig(t, off).RegisterSchema("identity", nil, dbx.Migrations["identity"]); err != nil {
		t.Fatalf("migrate: off once identity is applied: %v", err)
	}
	out, err = run(t, "migrate", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"20261024000001", "20261024000002", "20261025000002"} {
		if got := stateOf(t, out, "identity", v); !strings.HasPrefix(got, "applied ") {
			t.Fatalf("identity %s without --battery: %s\n%s", v, got, out)
		}
	}
	if strings.Contains(out, "jobs") || strings.Contains(out, "rbac") {
		t.Fatalf("status lists only the batteries in the ledger:\n%s", out)
	}
	if _, err := run(t, "migrate", "up", "--battery", "billing"); err == nil || !strings.Contains(err.Error(), "want one of identity, jobs, rbac") {
		t.Fatalf("an unknown battery: %v", err)
	}

	// An application migration that happens to share rbac's version:
	// marking that version needs the owner, and --owner rbac records
	// rbac's without running it while the application's stays pending.
	for _, d := range dialects {
		dir := filepath.Join("migrations", d)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "20261024000003_same_version.sql"), []byte(ddl(d, "cli_clash")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run(t, "migrate", "mark-applied", "20261024000003"); err == nil || !strings.Contains(err.Error(), "belongs to app and rbac; name one with --owner") {
		t.Fatalf("mark-applied of a shared version: %v", err)
	}
	out, err = run(t, "migrate", "mark-applied", "20261024000003", "--owner", "rbac")
	if err != nil || out != "marked  rbac  20261024000003_create_casbin_rule  applied, without running it (it was pending)\n" {
		t.Fatalf("mark-applied --owner rbac: %v\n%s", err, out)
	}
	if hasTable(t, cfg, "casbin_rule") {
		t.Fatal("mark-applied created casbin_rule")
	}
	if _, err := run(t, "migrate", "mark-applied", "20261024000003", "--owner", "billing"); err == nil || !strings.Contains(err.Error(), "want app or one of identity, jobs, rbac") {
		t.Fatalf("an unknown owner: %v", err)
	}
	out, _ = run(t, "migrate", "status")
	if got := stateOf(t, out, "rbac", "20261024000003"); !strings.HasPrefix(got, "applied ") {
		t.Fatalf("rbac once marked, listed from the ledger: %s\n%s", got, out)
	}
	if got := stateOf(t, out, "app", "20261024000003"); got != "pending" {
		t.Fatalf("the application's migration of the same version: %s\n%s", got, out)
	}
	if hasTable(t, cfg, "cli_clash") {
		t.Fatal("mark-applied --owner rbac ran the application's migration")
	}
}

// TestMigrate_KnowsEveryBatterySchema: the command carries the migrations
// of exactly the batteries that register a schema, each with files for
// every dialect, named as package db reads them, and no two battery
// migrations share a version.
func TestMigrate_KnowsEveryBatterySchema(t *testing.T) {
	if got := knownBatteries(); !slices.Equal(got, []string{"identity", "jobs", "rbac"}) {
		t.Fatalf("batteries %q", got)
	}
	name := regexp.MustCompile(`^\d{14}_[A-Za-z0-9_-]+\.sql$`)
	versions := map[string]string{} // version → battery
	for _, b := range knownBatteries() {
		var per [][]string
		for _, d := range dialects {
			entries, err := fs.ReadDir(dbx.Migrations[b], d)
			if err != nil {
				t.Fatalf("%s has no %s migrations: %v", b, d, err)
			}
			var files []string
			for _, e := range entries {
				if !name.MatchString(e.Name()) {
					t.Errorf("%s/%s/%s is not <version>_<name>.sql", b, d, e.Name())
				}
				files = append(files, e.Name())
			}
			per = append(per, files)
		}
		if len(per[0]) == 0 || !slices.Equal(per[0], per[1]) || !slices.Equal(per[0], per[2]) {
			t.Errorf("%s: the dialects have different migrations: %q", b, per)
		}
		// Every battery migration has a version of its own, so
		// db.MarkApplied finds exactly one without --owner.
		for _, f := range per[0] {
			v := f[:14]
			if other, dup := versions[v]; dup {
				t.Errorf("version %s belongs to both %s and %s", v, other, b)
			}
			versions[v] = b
		}
	}
}
