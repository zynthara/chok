package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zynthara/chok/v2/db"
)

func runMigrate(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := migrateCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := rootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestMigrateCreate_SequencesFiles(t *testing.T) {
	dir := t.TempDir()

	if _, err := runMigrate(t, "create", "init_users", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := runMigrate(t, "create", "add_posts", "--dir", dir); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"0001_init_users.sql", "0002_add_posts.sql"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("want %v, got %v", want, names)
	}

	if _, err := runMigrate(t, "create", "Bad Name!", "--dir", dir); err == nil {
		t.Fatal("non-snake-case names must be rejected")
	}
}

func writeProject(t *testing.T) (cfgPath, migDir string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	cfgPath = filepath.Join(dir, "chok.yaml")
	cfg := "db:\n  driver: sqlite\n  migrate: versioned\n  sqlite:\n    path: " + dbPath + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	migDir = filepath.Join(dir, "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sql := "CREATE TABLE cli_widgets (id BIGINT PRIMARY KEY, label VARCHAR(100));\n"
	if err := os.WriteFile(filepath.Join(migDir, "0001_widgets.sql"), []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, migDir
}

func TestMigrateUpAndStatus_EndToEnd(t *testing.T) {
	cfgPath, migDir := writeProject(t)

	// Fresh project: status shows pending + the built-in table catalog.
	out, err := runMigrate(t, "status", "--config", cfgPath, "--dir", migDir)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pending  0001_widgets") {
		t.Fatalf("status must list the pending migration:\n%s", out)
	}
	for _, tbl := range db.FrameworkTables() {
		if !strings.Contains(out, tbl) {
			t.Fatalf("status must present the framework table catalog (missing %s):\n%s", tbl, out)
		}
	}

	// up applies it...
	out, err = runMigrate(t, "up", "--config", cfgPath, "--dir", migDir)
	if err != nil {
		t.Fatalf("up: %v\n%s", err, out)
	}
	if !strings.Contains(out, "applied  0001_widgets.sql") {
		t.Fatalf("up must report the applied file:\n%s", out)
	}

	// ...idempotently...
	out, err = runMigrate(t, "up", "--config", cfgPath, "--dir", migDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "up to date") {
		t.Fatalf("second up must be a no-op:\n%s", out)
	}

	// ...and status flips to applied.
	out, err = runMigrate(t, "status", "--config", cfgPath, "--dir", migDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "applied  0001_widgets") || strings.Contains(out, "pending") {
		t.Fatalf("status after up must show applied and nothing pending:\n%s", out)
	}
}

func TestMigrateOwnedSequences_EndToEnd(t *testing.T) {
	cfgPath, migDir := writeProject(t)
	out, err := runMigrate(t, "up", "--component", "account", "--config", cfgPath, "--dir", migDir)
	if err != nil {
		t.Fatalf("account up: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[account] applied  0001_init.sql") || !strings.Contains(out, "[account] applied  0002_backfill_has_password.sql") {
		t.Fatalf("account up output:\n%s", out)
	}
	out, err = runMigrate(t, "up", "--all-owned", "--config", cfgPath, "--dir", migDir)
	if err != nil {
		t.Fatalf("all-owned up: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[audit] applied  0001_init.sql") || !strings.Contains(out, "[authz] applied  0001_init.sql") {
		t.Fatalf("all-owned output:\n%s", out)
	}
	out, err = runMigrate(t, "status", "--config", cfgPath, "--dir", migDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"[account] ledger=schema_migrations_chok_account dialect=sqlite", "[audit] ledger=schema_migrations_chok_audit dialect=sqlite", "[authz] ledger=schema_migrations_chok_authz dialect=sqlite"} {
		if !strings.Contains(out, section) {
			t.Fatalf("status missing %q:\n%s", section, out)
		}
		for _, owner := range []string{"github.com/zynthara/chok/v2/account", "github.com/zynthara/chok/v2/audit", "github.com/zynthara/chok/v2/authz"} {
			if !strings.Contains(out, "owner="+owner) || !strings.Contains(out, "content=verified-by-binary") {
				t.Fatalf("status manifest panorama missing built-in owner %q:\n%s", owner, out)
			}
		}
	}
	if _, err := runMigrate(t, "up", "--component", "unknown", "--config", cfgPath, "--dir", migDir); err == nil {
		t.Fatal("unknown owned component must fail")
	}
}

func TestMigrateThirdPartyManifestStatusCheckAndClaimRepair(t *testing.T) {
	cfgPath, migDir := writeProject(t)
	if out, err := runMigrate(t, "up", "--config", cfgPath, "--dir", migDir); err != nil {
		t.Fatalf("application up: %v\n%s", err, out)
	}
	h, err := openFromConfig(&migrateFlags{config: cfgPath, dir: migDir})
	if err != nil {
		t.Fatal(err)
	}
	assets := fstest.MapFS{}
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		assets[dialect+"/0001_init.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE cli_third_party_item (id BIGINT PRIMARY KEY);")}
	}
	const oldOwner = "example.com/acme/billing"
	seq, err := db.OwnedSequence("billing", assets, db.Baseline{}, db.SequenceOwner(oldOwner), db.SequenceVersion("v1.2.3"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplySequence(t.Context(), h, seq); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()

	out, err := runMigrate(t, "status", "--config", cfgPath, "--dir", migDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"[billing]", "owner=" + oldOwner, "component=v1.2.3", "content=unverified", "ledger_state=present"} {
		if !strings.Contains(out, part) {
			t.Fatalf("third-party panorama missing %q:\n%s", part, out)
		}
	}
	if out, err := runMigrate(t, "status", "--check", "--config", cfgPath, "--dir", migDir); err == nil || !strings.Contains(err.Error(), "content is unavailable") {
		t.Fatalf("default check must fail closed on third-party content: err=%v\n%s", err, out)
	}
	if out, err := runMigrate(t, "status", "--check", "--ledger-health-only", "--config", cfgPath, "--dir", migDir); err != nil {
		t.Fatalf("healthy third-party ledger must pass explicit relaxed check: %v\n%s", err, out)
	}

	const newOwner = "example.com/acme/payments"
	out, err = runMigrate(t,
		"repair", "claim",
		"--kind", "billing",
		"--expected-owner", oldOwner,
		"--new-owner", newOwner,
		"--config", cfgPath,
	)
	if err != nil || !strings.Contains(out, "previous_owner="+oldOwner) || !strings.Contains(out, "new_owner="+newOwner) {
		t.Fatalf("repair claim: err=%v\n%s", err, out)
	}
	out, err = runMigrate(t, "status", "--config", cfgPath, "--dir", migDir)
	if err != nil || !strings.Contains(out, "owner="+newOwner) {
		t.Fatalf("status after claim transfer: err=%v\n%s", err, out)
	}
}

func TestCheckOwnedMigrationCatalogMatrix(t *testing.T) {
	healthy := &db.SequenceLedgerSnapshot{Kind: "billing", Ledger: "schema_migrations_chok_billing", Exists: true}
	unclaimed := []ownedMigrationCatalogRow{{snapshot: healthy, content: "unverified"}}
	if err := checkOwnedMigrationCatalog(unclaimed, false); err == nil || !strings.Contains(err.Error(), "unclaimed") {
		t.Fatalf("strict check must reject an unclaimed ledger, got %v", err)
	}
	if err := checkOwnedMigrationCatalog(unclaimed, true); err != nil {
		t.Fatalf("ledger-health-only must allow a healthy unclaimed ledger: %v", err)
	}

	highFloor := db.ManifestEntry{Kind: "billing", Ledger: healthy.Ledger, Owner: "example.com/acme/billing", EngineFloor: db.MigrationEngineGeneration + 1}
	incompatible := []ownedMigrationCatalogRow{{entry: &highFloor, snapshot: healthy, content: "unverified"}}
	for _, relaxed := range []bool{false, true} {
		if err := checkOwnedMigrationCatalog(incompatible, relaxed); err == nil || !strings.Contains(err.Error(), "requires engine generation") {
			t.Fatalf("engine floor must fail with relaxed=%t, got %v", relaxed, err)
		}
	}

	unverifiedLedger := *healthy
	unverifiedLedger.Unverified = 1
	if err := checkOwnedMigrationCatalog([]ownedMigrationCatalogRow{{snapshot: &unverifiedLedger, content: "unverified"}}, true); err == nil || !strings.Contains(err.Error(), "ledger health") {
		t.Fatalf("ledger-health-only must still reject checksum-unverified ledger rows, got %v", err)
	}

	missingLedger := *healthy
	missingLedger.Exists = false
	if err := checkOwnedMigrationCatalog([]ownedMigrationCatalogRow{{snapshot: &missingLedger, content: "unverified"}}, true); err == nil || !strings.Contains(err.Error(), "ledger is missing") {
		t.Fatalf("ledger-health-only must still reject a missing ledger, got %v", err)
	}
}

func TestMigrateStatusCheckAndRepairAcceptDrift(t *testing.T) {
	cfgPath, migDir := writeProject(t)
	out, err := runRoot(t, "migrate", "status", "--check", "--config", cfgPath, "--dir", migDir)
	if err == nil || !strings.Contains(out, "pending") {
		t.Fatalf("pending status --check must fail after rendering state: err=%v\n%s", err, out)
	}
	if !strings.Contains(out, "migration status is not clean") {
		t.Fatalf("status --check must retain the semantic error: err=%v\n%s", err, out)
	}
	if strings.Contains(out, "Usage:") {
		t.Fatalf("semantic errors must not print cobra usage:\n%s", out)
	}
	if out, err := runMigrate(t, "up", "--config", cfgPath, "--dir", migDir); err != nil {
		t.Fatalf("up: %v\n%s", err, out)
	}
	files, err := db.LoadMigrations(os.DirFS(migDir))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runMigrate(t, "status", "--check", "--config", cfgPath, "--dir", migDir); err != nil {
		t.Fatalf("clean status --check: %v\n%s", err, out)
	}

	path := filepath.Join(migDir, "0001_widgets.sql")
	if err := os.WriteFile(path, []byte("CREATE TABLE cli_widgets (id BIGINT PRIMARY KEY, label VARCHAR(100)); -- reviewed drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runMigrate(t, "status", "--check", "--config", cfgPath, "--dir", migDir); err == nil || !strings.Contains(out, "drift") {
		t.Fatalf("drifted status --check must fail: err=%v\n%s", err, out)
	}
	currentFiles, err := db.LoadMigrations(os.DirFS(migDir))
	if err != nil {
		t.Fatal(err)
	}
	out, err = runMigrate(t,
		"repair", "accept-drift", "1",
		"--checksum", files[0].Checksum,
		"--new-checksum", currentFiles[0].Checksum,
		"--reason", "reviewed comment-only migration change",
		"--config", cfgPath, "--dir", migDir,
	)
	if err != nil || !strings.Contains(out, "action=accept-drift") {
		t.Fatalf("repair accept-drift: err=%v\n%s", err, out)
	}
	if out, err := runMigrate(t, "status", "--check", "--config", cfgPath, "--dir", migDir); err != nil {
		t.Fatalf("accepted drift must pass --check: %v\n%s", err, out)
	}
}

func TestMigrateUp_MissingConfigErrors(t *testing.T) {
	if _, err := runMigrate(t, "up", "--config", "/nonexistent/chok.yaml", "--dir", t.TempDir()); err == nil {
		t.Fatal("an explicit missing config file must error")
	}
}
