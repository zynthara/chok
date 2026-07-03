package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v2"
	"github.com/zynthara/chok/v2/web"
)

// TestM2Fixture_EndToEnd is the M2 acceptance run (SPEC §10 M2 row):
// the fixture assembly serves real HTTP — the M1-deferred items
// (/healthz /metrics /componentz reachability) are verified here, and
// the swagger spec generated for the fixture's routes must equal the
// hand-written baseline file.
func TestM2Fixture_EndToEnd(t *testing.T) {
	t.Setenv("M2FIXTURE_DEBUG_ENABLED", "true")
	t.Setenv("M2FIXTURE_HTTP_ADDR", "127.0.0.1:0")

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

	// --- M1 deferred acceptance: the three endpoints over real HTTP ---
	if code, body := get("/healthz"); code != 200 || !strings.Contains(body, `"status":"up"`) {
		t.Fatalf("/healthz: %d %s", code, body)
	}
	if code, body := get("/metrics"); code != 200 || !strings.Contains(body, "chok_component_up") {
		t.Fatalf("/metrics: %d (chok_component_up missing)", code)
	}
	if code, body := get("/componentz"); code != 200 || !strings.Contains(body, `"component":"http"`) {
		t.Fatalf("/componentz: %d %s", code, body)
	}
	// The assembled-but-disabled tracing module stays visible (SPEC
	// §3.1 definition 3).
	if _, body := get("/componentz"); !strings.Contains(body, `"component":"tracing","state":"disabled"`) {
		t.Fatalf("/componentz must show tracing as disabled: %s", body)
	}

	// Liveness/readiness alongside.
	if code, _ := get("/livez"); code != 200 {
		t.Fatalf("/livez: %d", code)
	}
	if code, _ := get("/readyz"); code != 200 {
		t.Fatalf("/readyz: %d", code)
	}

	// --- user routes: plain + typed handlers ---
	if code, body := get("/hello"); code != 200 || !strings.HasPrefix(body, "hello from m2") {
		t.Fatalf("/hello: %d %q", code, body)
	}
	resp, err := http.Post(base+"/api/v1/posts", "application/json",
		strings.NewReader(`{"title":"t1","content":"c1"}`))
	if err != nil {
		t.Fatal(err)
	}
	created, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 || !strings.Contains(string(created), `"rid":"post_1"`) {
		t.Fatalf("POST /api/v1/posts: %d %s", resp.StatusCode, created)
	}
	if code, body := get("/api/v1/posts/post_9"); code != 200 || !strings.Contains(body, `"rid":"post_9"`) {
		t.Fatalf("GET /api/v1/posts/{rid}: %d %s", code, body)
	}

	// 404 envelope end to end (matrix decision over a real socket).
	if code, body := get("/missing"); code != 404 || !strings.Contains(body, `"reason":"NotFound"`) {
		t.Fatalf("404 envelope: %d %s", code, body)
	}

	// --- swagger spec vs the hand-written baseline ---
	code, gotSpec := get("/swagger/doc.json")
	if code != 200 {
		t.Fatalf("/swagger/doc.json: %d", code)
	}
	wantSpec, err := os.ReadFile("testdata/swagger-baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal([]byte(gotSpec), &got); err != nil {
		t.Fatalf("generated spec is not JSON: %v", err)
	}
	if err := json.Unmarshal(wantSpec, &want); err != nil {
		t.Fatalf("baseline is not JSON: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		gotPretty, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("generated swagger spec diverges from the baseline.\n--- generated ---\n%s", gotPretty)
	}

	// Swagger UI serves under the prefix subtree.
	if code, body := get("/swagger/"); code != 200 || !strings.Contains(body, "swagger-ui") {
		t.Fatalf("/swagger/ UI: %d", code)
	}

	// --- clean stop ---
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
