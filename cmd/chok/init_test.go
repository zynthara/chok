package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInit_GeneratedProjectBoots is the scaffold self-check the M5 DoD
// demands: chok init must produce a project that builds and serves
// with zero edits. It exercises the real templates, the real sync
// engine (init shares its code path) and a real `go build` against
// this checkout via a replace directive, then boots the binary, waits
// for /healthz, creates a Note through the example route and shuts
// down on SIGINT.
func TestInit_GeneratedProjectBoots(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the go tool over a scaffolded project")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	proj := filepath.Join(t.TempDir(), "initapp")
	cmd := initCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{proj})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}

	for _, f := range []string{"chok.yaml", "main.go", "chok_modules_gen.go", "go.mod", "Makefile", ".gitignore", "migrations/README.md"} {
		if _, err := os.Stat(filepath.Join(proj, f)); err != nil {
			t.Fatalf("scaffold must create %s: %v", f, err)
		}
	}
	gen, _ := os.ReadFile(filepath.Join(proj, "chok_modules_gen.go"))
	for _, want := range []string{"web.Module()", "db.Module()", "account.Module()", "swagger.Module()"} {
		if !strings.Contains(string(gen), want) {
			t.Fatalf("generated assembly must include %s:\n%s", want, gen)
		}
	}

	// Pin the replace to THIS checkout (detectChokLocalPath depends on
	// where the test binary lives — make it deterministic).
	gomodPath := filepath.Join(proj, "go.mod")
	gomod, err := os.ReadFile(gomodPath)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(gomod), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "replace github.com/zynthara/chok/v2") {
			continue
		}
		kept = append(kept, line)
	}
	kept = append(kept, fmt.Sprintf("replace github.com/zynthara/chok/v2 => %s", repoRoot), "")
	if err := os.WriteFile(gomodPath, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	goTool := func(args ...string) {
		t.Helper()
		c := exec.Command("go", args...)
		c.Dir = proj
		c.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
		if b, err := c.CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, b)
		}
	}
	goTool("mod", "tidy")
	goTool("build", "-o", "initapp-bin", ".")

	// Boot on a free port, against a throwaway sqlite file.
	port := freePort(t)
	app := exec.Command("./initapp-bin")
	app.Dir = proj
	app.Env = append(os.Environ(),
		"INITAPP_HTTP_ADDR=127.0.0.1:"+port,
		"INITAPP_DB_SQLITE_PATH="+filepath.Join(t.TempDir(), "initapp.db"),
	)
	var appOut strings.Builder
	app.Stdout = &appOut
	app.Stderr = &appOut
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = app.Process.Kill()
			_ = app.Wait()
		}
	}()

	base := "http://127.0.0.1:" + port
	waitForHealthz(t, app, base, &appOut)

	// The example route works end to end.
	resp, err := http.Post(base+"/api/v1/notes", "application/json",
		strings.NewReader(`{"title":"hello","body":"from the scaffold"}`))
	if err != nil {
		t.Fatalf("POST /api/v1/notes: %v\n%s", err, appOut.String())
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/notes = %d, want 201: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"id":"note_`) {
		t.Fatalf("created note must expose the prefixed RID: %s", body)
	}

	// Ctrl-C: the generated app must stop cleanly.
	if err := app.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- app.Wait() }()
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatalf("app exited non-zero after SIGINT: %v\n%s", err, appOut.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("app did not stop on SIGINT\n%s", appOut.String())
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	return port
}

func waitForHealthz(t *testing.T, app *exec.Cmd, base string, logs *strings.Builder) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if app.ProcessState != nil {
			t.Fatalf("app exited during startup:\n%s", logs.String())
		}
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(b), `"status":"up"`) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("/healthz never came up\n%s", logs.String())
}
