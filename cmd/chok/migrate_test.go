package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	// Fresh project: status shows pending + the whitelist.
	out, err := runMigrate(t, "status", "--config", cfgPath, "--dir", migDir)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pending  0001_widgets") {
		t.Fatalf("status must list the pending migration:\n%s", out)
	}
	for _, tbl := range []string{"users", "identities", "audit_logs", "casbin_rule", "schema_migrations"} {
		if !strings.Contains(out, tbl) {
			t.Fatalf("status must present the framework whitelist (missing %s):\n%s", tbl, out)
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

func TestMigrateStatusCheckAndRepairAcceptDrift(t *testing.T) {
	cfgPath, migDir := writeProject(t)
	if out, err := runMigrate(t, "status", "--check", "--config", cfgPath, "--dir", migDir); err == nil || !strings.Contains(out, "pending") {
		t.Fatalf("pending status --check must fail after rendering state: err=%v\n%s", err, out)
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
	out, err := runMigrate(t,
		"repair", "accept-drift", "1",
		"--checksum", files[0].Checksum,
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
