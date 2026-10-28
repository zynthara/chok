package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zynthara/chok/v3/internal/conf"
)

// TestDocsGen_ConfigReferenceIsCurrent keeps docs/config.md from rotting:
// it must be exactly what chok docs gen writes from the Config types and
// their comments, as configkeys keeps the key budget honest.
func TestDocsGen_ConfigReferenceIsCurrent(t *testing.T) {
	root := repoRoot(t)
	want, err := configReference(root)
	if err != nil {
		t.Fatal(err)
	}
	have, err := os.ReadFile(filepath.Join(root, configDoc))
	if err != nil {
		t.Fatal(err)
	}
	if string(have) != string(want) {
		t.Fatalf("%s is not current: run go run ./cmd/chok docs gen", configDoc)
	}
	if out, err := run(t, "docs", "gen", "--check", "--root", root); err != nil || !strings.HasSuffix(out, "is current\n") {
		t.Fatalf("docs gen --check: %v\n%s", err, out)
	}
}

// TestDocsGen_MarksSecretsAndRules: every key of the eight sections is a row
// exactly once under its section's path, secrets are marked, validate
// rules and defaults shown, and comments come from the field — in an
// anonymous struct too, or shared with the field above.
func TestDocsGen_MarksSecretsAndRules(t *testing.T) {
	doc, err := configReference(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for line := range strings.Lines(string(doc)) {
		if strings.HasPrefix(line, "| `") {
			key := strings.Trim(strings.Fields(line)[1], "`")
			if _, dup := rows[key]; dup {
				t.Errorf("key %s listed twice", key)
			}
			rows[key] = line
		}
	}
	total := 0
	for _, s := range configSections {
		total += len(conf.Keys(s.typ))
	}
	if len(rows) != total || total != 168 {
		t.Fatalf("%d rows for %d keys; the budget counts 168", len(rows), total)
	}
	for key, want := range map[string][]string{
		"auth.signing_key":                    {"| **secret** |", "at least 32 bytes"},
		"auth.providers.<name>.client_secret": {"**secret**"},
		"cache.redis.password":                {"**secret**"},
		"db.postgres.dsn":                     {"**secret**"},
		"db.driver":                           {"`oneof=sqlite mysql postgres`"},
		"log.level":                           {"| `info` |", "`oneof=debug info warn warning error`", "SIGHUP"},
		"http.addr":                           {"| string | `:8080` |"},
		"telemetry.metrics.enabled":           {"| bool | `true` |", "Prometheus text format"},
		"telemetry.tracing.endpoint":          {"&lt;endpoint&gt;/v1/traces"},
		"auth.login_rate_limit":               {"| int | `10` |", "LoginRateWindow and LoginRateLimit throttle"},
		"log.files[].max_size_mb":             {"| int | `100` | `gte=0` |"},
		"jobs.enabled":                        {"| bool | `true` |", "enqueue-only"},
		"cache.breaker.probe_timeout":         {"| duration | `2s` |"},
		"auth.providers.<name>.hosted_domain": {"Google Workspace"},
		"db.store.owner_scope":                {"nil means true"},
		"realtime.path":                       {"| string | `/realtime` |", "{Path}/ticket"},
		"realtime.allowed_origins":            {"| list of string |", "path.Match"},
		"realtime.ping_interval":              {"| duration | `25s` |"},
		"realtime.replay":                     {"| int | `256` |", "0 turns resuming off"},
		"realtime.channel":                    {"| string | `chok:realtime` |"},
		"auth.passwordless.code_length":       {"| int | `6` |", "`omitempty,min=6,max=10`"},
		"auth.passwordless.auto_register":     {"| bool |", "nil means true"},
		"auth.passwordless.only":              {"| bool |", "change-password are not mounted", "It needs Enabled"},
		"auth.reset_url":                      {"| string |", "{prefix}/reset-password"},
		"auth.cookie.same_site":               {"| string | `lax` |", "`omitempty,oneof=lax strict none`", "none needs Secure"},
		"auth.cookie.secure":                  {"| bool |", "nil means true"},
		"auth.cookie.name":                    {"| string | `chok_token` |"},
		"mail.transport":                      {"| string | `smtp` |", "`omitempty,oneof=smtp dir log`", ".eml file"},
		"mail.timeout":                        {"| duration | `15s` |"},
		"mail.smtp.port":                      {"| int | `587` |"},
		"mail.smtp.password":                  {"| **secret** |", "AUTH PLAIN"},
		"mail.smtp.tls":                       {"| string | `starttls` |", "`omitempty,oneof=starttls implicit none`", "loopback"},
	} {
		row, ok := rows[key]
		if !ok {
			t.Errorf("no row for %s", key)
			continue
		}
		for _, w := range want {
			if !strings.Contains(row, w) {
				t.Errorf("%s: %q lacks %q", key, row, w)
			}
		}
	}
	for key, row := range rows {
		if strings.Contains(row, "**secret**") != slices.Contains([]string{"auth.signing_key", "auth.providers.<name>.client_secret", "cache.redis.password", "db.mysql.password", "db.postgres.dsn", "db.postgres.password", "mail.smtp.password"}, key) {
			t.Errorf("secret mark of %s: %s", key, row)
		}
	}
}

// copyTree copies the non-test Go files and go.mod docs gen reads, and
// docs/config.md, from the checkout into dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	for _, dir := range []string{".", "db", "identity", "jobs", "telemetry", "cache", "realtime", "mail", "docs"} {
		entries, err := os.ReadDir(filepath.Join(src, dir))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dst, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			n := e.Name()
			keep := (strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go")) || n == "go.mod" || (dir == "docs" && n == "config.md")
			if e.IsDir() || !keep {
				continue
			}
			in, err := os.Open(filepath.Join(src, dir, n))
			if err != nil {
				t.Fatal(err)
			}
			out, err := os.Create(filepath.Join(dst, dir, n))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(out, in); err != nil {
				t.Fatal(err)
			}
			_ = in.Close()
			_ = out.Close()
		}
	}
}

// TestDocsGen_CheckReportsDrift: --check fails on a hand-edited
// docs/config.md and on a changed field comment, writes nothing, and passes
// again once docs gen has rewritten the file; outside a chok checkout docs
// gen refuses to run.
func TestDocsGen_CheckReportsDrift(t *testing.T) {
	root := t.TempDir()
	copyTree(t, repoRoot(t), root)
	if _, err := run(t, "docs", "gen", "--check", "--root", root); err != nil {
		t.Fatalf("a fresh copy is current: %v", err)
	}
	doc := filepath.Join(root, configDoc)
	if err := os.WriteFile(doc, []byte("# edited by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "docs", "gen", "--check", "--root", root); err == nil || !strings.Contains(err.Error(), "is not current") {
		t.Fatalf("--check over an edited file: %v", err)
	}
	if raw, _ := os.ReadFile(doc); string(raw) != "# edited by hand\n" {
		t.Fatal("--check wrote the file")
	}
	if out, err := run(t, "docs", "gen", "--root", root); err != nil || !strings.HasSuffix(out, "config.md written\n") {
		t.Fatalf("docs gen: %v\n%s", err, out)
	}
	if _, err := run(t, "docs", "gen", "--check", "--root", root); err != nil {
		t.Fatalf("after docs gen: %v", err)
	}

	src := filepath.Join(root, "config.go")
	raw, _ := os.ReadFile(src)
	changed := strings.Replace(string(raw), "// Level is the one setting SIGHUP applies while running.", "// Level is how much is logged.", 1)
	if changed == string(raw) {
		t.Fatal("the Level comment moved; update the test")
	}
	if err := os.WriteFile(src, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "docs", "gen", "--check", "--root", root); err == nil {
		t.Fatal("--check passes after a field comment changed")
	}

	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "go.mod"), []byte("module example.com/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "docs", "gen", "--root", other); err == nil || !strings.Contains(err.Error(), "not a chok checkout") {
		t.Fatalf("docs gen outside chok: %v", err)
	}
}
