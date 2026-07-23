package main

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v2"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/web"
)

// TestM3Fixture_EndToEnd is the M3 acceptance run (SPEC §10 M3 row):
// the fixture boots with db.migrate: versioned — schema arrives via
// the embedded ledger-driven migrations — and serves a store-backed
// API whose writes surface as EntityChanged events on the kernel bus.
func TestM3Fixture_EndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "m3.db")
	t.Setenv("M3FIXTURE_CONFIG", "chok.yaml") // test CWD is the package dir
	t.Setenv("M3FIXTURE_DB_SQLITE_PATH", dbPath)
	t.Setenv("M3FIXTURE_DEBUG_ENABLED", "true")
	t.Setenv("M3FIXTURE_HTTP_ADDR", "127.0.0.1:0")

	app := buildApp()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx) }()

	base := waitForHTTP(t, app, runErr)

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// The db component is up and visible.
	if code, body := get("/componentz"); code != 200 || !strings.Contains(body, `"component":"db"`) {
		t.Fatalf("/componentz must show the db component: %d %s", code, body)
	}
	if code, body := get("/healthz"); code != 200 || !strings.Contains(body, `"status":"up"`) {
		t.Fatalf("/healthz with db assembled: %d %s", code, body)
	}

	// Store-backed API over the versioned-migrated table.
	resp, err := http.Post(base+"/api/v1/notes", "application/json",
		strings.NewReader(`{"title":"first note"}`))
	if err != nil {
		t.Fatal(err)
	}
	created, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("POST /api/v1/notes: %d %s", resp.StatusCode, created)
	}
	var note struct {
		RID     string `json:"rid"`
		Title   string `json:"title"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(created, &note); err != nil {
		t.Fatalf("create response not JSON: %v (%s)", err, created)
	}
	if !strings.HasPrefix(note.RID, "note_") || note.Version != 1 {
		t.Fatalf("created note must carry a note_ RID and version 1: %s", created)
	}

	if code, body := get("/api/v1/notes/" + note.RID); code != 200 || !strings.Contains(body, "first note") {
		t.Fatalf("GET note: %d %s", code, body)
	}
	if code, body := get("/api/v1/notes"); code != 200 || !strings.Contains(body, note.RID) {
		t.Fatalf("list notes: %d %s", code, body)
	}

	// WithBus end to end: the bus subscriber (async) sees the create.
	waitFor(t, 5*time.Second, func() bool {
		_, body := get("/api/v1/notes/events")
		return strings.Contains(body, `"created":1`)
	}, "EntityChanged[Note] never reached the bus subscriber")

	// The ledger recorded the embedded migration set — same status the
	// CLI renders, built-in catalog included (M3 DoD: versioned boot presents
	// the framework-owned schema boundary honestly).
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	h := db.From(app.Kernel())
	st, err := db.MigrationsStatus(context.Background(), h, sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Applied) != 1 || st.Applied[0].Name != "notes" || len(st.Pending) != 0 {
		t.Fatalf("versioned boot must have applied 0001_notes: %+v", st)
	}
	want := []string{
		"audit_logs",
		"casbin_rule",
		"identities",
		"outbox_messages",
		"outbox_relay_state",
		"schema_migrations",
		"schema_migrations_chok_account",
		"schema_migrations_chok_audit",
		"schema_migrations_chok_authz",
		"schema_migrations_chok_manifest",
		"schema_migrations_chok_outbox",
		"schema_migrations_chok_repairs",
		"users",
	}
	if strings.Join(st.FrameworkTables, ",") != strings.Join(want, ",") {
		t.Fatalf("framework table catalog drifted: %v", st.FrameworkTables)
	}

	// Clean stop.
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("app did not stop")
	}
}

func waitFor(t *testing.T, budget time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal(msg)
}

// waitForHTTP blocks until the web component reports its bound
// address (Serve running), failing fast if Run exits first.
func waitForHTTP(t *testing.T, app *chok.App, runErr <-chan error) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-runErr:
			t.Fatalf("app exited during startup: %v", err)
		case <-deadline:
			t.Fatal("web component never became reachable")
		case <-time.After(25 * time.Millisecond):
		}
		if k := app.Kernel(); k != nil {
			if webc, ok := chok.Get[*web.Component](k, "http"); ok {
				if addr := webc.BoundAddr(); addr != "" {
					return "http://" + addr
				}
			}
		}
	}
}
