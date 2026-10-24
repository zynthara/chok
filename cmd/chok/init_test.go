package main

import (
	"go/format"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v3/internal/dbtest"
)

// TestInit_GeneratedProjectBoots is the scaffold's self-check: chok init
// writes a project that vets, builds and serves with no edit. The test
// points the project's go.mod at this checkout with a replace directive
// (the scaffold itself never carries one), builds it, starts it on a
// random port against the lane's database, creates and lists a note
// through its routes, and stops it with SIGINT.
func TestInit_GeneratedProjectBoots(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a scaffolded project with the go command")
	}
	root := repoRoot(t)
	t.Chdir(t.TempDir())
	out, err := run(t, "init", "initapp")
	if err != nil {
		t.Fatalf("chok init: %v\n%s", err, out)
	}
	proj, _ := filepath.Abs("initapp")
	for _, f := range []string{"go.mod", "main.go", "chok.yaml", "Makefile", ".gitignore", "migrations/README.md"} {
		if _, err := os.Stat(filepath.Join(proj, f)); err != nil {
			t.Fatalf("chok init must write %s: %v", f, err)
		}
	}

	mainGo, _ := os.ReadFile(filepath.Join(proj, "main.go"))
	if n := strings.Count(string(mainGo), "\n"); n > 80 {
		t.Fatalf("main.go has %d lines; the scaffold's budget is 80", n)
	}
	if formatted, err := format.Source(mainGo); err != nil || string(formatted) != string(mainGo) {
		t.Fatalf("main.go is not gofmt-clean: %v", err)
	}
	gomod, _ := os.ReadFile(filepath.Join(proj, "go.mod"))
	for _, want := range []string{"module initapp\n", "\ngo 1.27\n", "\nrequire github.com/zynthara/chok/v3 v3.", "\ntool github.com/zynthara/chok/v3/cmd/chok\n"} {
		if !strings.Contains(string(gomod), want) {
			t.Fatalf("go.mod lacks %q:\n%s", want, gomod)
		}
	}
	if strings.Contains(string(gomod), "replace") {
		t.Fatalf("the scaffold's go.mod must not carry a replace directive:\n%s", gomod)
	}

	// Build against this checkout.
	f, err := os.OpenFile(filepath.Join(proj, "go.mod"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(f, "\nreplace github.com/zynthara/chok/v3 => "+root+"\n")
	_ = f.Close()
	goTool(t, proj, "mod", "tidy")
	goTool(t, proj, "vet", "./...")
	goTool(t, proj, "build", "-o", "initapp-bin", ".")

	app := exec.Command("./initapp-bin")
	app.Dir = proj
	lane := dbtest.Config(t)
	app.Env = append(os.Environ(), append([]string{"INITAPP_HTTP_ADDR=127.0.0.1:0", "INITAPP_HTTP_DRAIN_DELAY=0s"},
		dbEnv("INITAPP", lane)...)...)
	p := startProcess(t, app)
	addr := p.listening(t, 60*time.Second)
	base := "http://" + addr

	expect := func(method, path, body string, status int, contains string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != status || !strings.Contains(string(got), contains) {
			t.Fatalf("%s %s = %d %s; want %d containing %q", method, path, resp.StatusCode, got, status, contains)
		}
	}
	expect(http.MethodGet, "/healthz", "", http.StatusOK, `"status":"up"`)
	expect(http.MethodPost, "/api/v1/notes", `{"title":"hello","body":"from the scaffold"}`, http.StatusCreated, `"id":"note_`)
	expect(http.MethodGet, "/api/v1/notes?title=hello", "", http.StatusOK, `"total":1`)
	expect(http.MethodPost, "/api/v1/notes", `{"body":"no title"}`, http.StatusBadRequest, `"reason":"Validation"`)
	expect(http.MethodGet, "/openapi.json", "", http.StatusOK, `"/api/v1/notes/{id}"`)

	// The note is in the lane's database: the environment, not chok.yaml,
	// chose it.
	var n int64
	if err := dbtest.OpenConfig(t, lane).Unsafe(t.Context()).Table("notes").Count(&n).Error; err != nil || n != 1 {
		t.Fatalf("notes in the %s database: %d, %v", lane.Driver, n, err)
	}

	p.interrupt(t, 20*time.Second)
	if !strings.Contains(p.output(), `"msg":"chok: stopped"`) {
		t.Fatalf("no stop line:\n%s", p.output())
	}
}

// TestInit_RefusesBadNamesAndBusyDirectories: the name must make a
// directory, a binary and an environment prefix, and init never writes
// into a directory that has files.
func TestInit_RefusesBadNamesAndBusyDirectories(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, name := range []string{"1app", "my app", "../up", "", "a/b"} {
		if _, err := run(t, "init", name); err == nil {
			t.Errorf("chok init %q succeeded", name)
		}
	}
	if err := os.MkdirAll("taken", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("taken", "keep.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "init", "taken"); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("init into a directory with files: %v", err)
	}
	if _, err := os.Stat(filepath.Join("taken", "main.go")); err == nil {
		t.Fatal("init wrote into a directory that has files")
	}
	// An empty directory is fine, and --module sets the module path.
	if err := os.MkdirAll("empty", 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "init", "empty", "--module", "example.com/acme/empty"); err != nil {
		t.Fatalf("init into an empty directory: %v\n%s", err, out)
	}
	gomod, _ := os.ReadFile(filepath.Join("empty", "go.mod"))
	if !strings.HasPrefix(string(gomod), "module example.com/acme/empty\n") {
		t.Fatalf("--module: %s", gomod)
	}
	yaml, _ := os.ReadFile(filepath.Join("empty", "chok.yaml"))
	main, _ := os.ReadFile(filepath.Join("empty", "main.go"))
	if !strings.Contains(string(yaml), "EMPTY_HTTP_ADDR") || !strings.Contains(string(main), `chok.MustLoad[Config]("empty")`) {
		t.Fatal("the app name, not the module path, names the environment prefix")
	}
}

// TestInit_RequiresItsOwnRelease: a release command scaffolds a project
// on its own version; a source build falls back to the release this tree
// prepares and says so.
func TestInit_RequiresItsOwnRelease(t *testing.T) {
	for _, c := range []struct {
		version  string
		want     string
		fallback bool
	}{
		{"v3.0.0-alpha.1", "v3.0.0-alpha.1", false},
		{"v3.1.2", "v3.1.2", false},
		{"v3.0.0-20261025080000-0123456789ab", "v3.0.0-20261025080000-0123456789ab", false},
		{"v3.0.0-20261025080000-0123456789ab+dirty", releaseVersion, true},
		{"(devel)", releaseVersion, true},
	} {
		v, fallback := scaffoldVersion(build{Version: c.version})
		if v != c.want || fallback != c.fallback {
			t.Errorf("%s: %s %v, want %s %v", c.version, v, fallback, c.want, c.fallback)
		}
	}
}
