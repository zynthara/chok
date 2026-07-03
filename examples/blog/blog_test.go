package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v2"
	"github.com/zynthara/chok/v2/web"
)

// TestBlog_QuickstartPath is the acceptance run behind the README's
// five-minute walkthrough: boot from chok.yaml, register + login over
// real HTTP, drive the post CRUD with the Bearer token, prove the
// owner fence and the optimistic lock, stop cleanly.
func TestBlog_QuickstartPath(t *testing.T) {
	t.Setenv("BLOG_CONFIG", "chok.yaml") // test CWD is examples/blog
	t.Setenv("BLOG_DB_SQLITE_PATH", filepath.Join(t.TempDir(), "blog.db"))
	t.Setenv("BLOG_HTTP_ADDR", "127.0.0.1:0")

	app := buildApp()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx) }()
	base := waitForHTTP(t, app, runErr)

	alice := registerAndLogin(t, base, "alice@blog.test")
	bob := registerAndLogin(t, base, "bob@blog.test")

	// Anonymous requests fail closed.
	if code, _ := request(t, base, http.MethodGet, "/api/v1/posts", "", ""); code != 401 {
		t.Fatalf("anonymous list = %d, want 401", code)
	}

	// Create → read back.
	code, body := request(t, base, http.MethodPost, "/api/v1/posts", alice,
		`{"title":"Hello chok","content":"first post"}`)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var created Post
	if err := json.Unmarshal([]byte(body), &created); err != nil || !strings.HasPrefix(created.RID, "pst_") {
		t.Fatalf("create must return a pst_ RID: %s", body)
	}
	rid := created.RID

	if code, body = request(t, base, http.MethodGet, "/api/v1/posts/"+rid, alice, ""); code != 200 || !strings.Contains(body, "Hello chok") {
		t.Fatalf("get: %d %s", code, body)
	}

	// Owner fence: bob sees an empty world and cannot read alice's post.
	if code, body = request(t, base, http.MethodGet, "/api/v1/posts", bob, ""); code != 200 || strings.Contains(body, rid) {
		t.Fatalf("bob's list must not leak alice's post: %d %s", code, body)
	}
	if code, _ = request(t, base, http.MethodGet, "/api/v1/posts/"+rid, bob, ""); code != 404 {
		t.Fatalf("bob reading alice's post = %d, want 404", code)
	}

	// Update publishes the post (optimistic lock riding Post.Version).
	if code, body = request(t, base, http.MethodPut, "/api/v1/posts/"+rid, alice, `{"status":"published"}`); code != 200 {
		t.Fatalf("update: %d %s", code, body)
	}
	// A stale delete (version from before the update) conflicts.
	if code, body = request(t, base, http.MethodDelete, "/api/v1/posts/"+rid, alice,
		fmt.Sprintf(`{"version":%d}`, created.Version)); code != 409 {
		t.Fatalf("stale-version delete = %d, want 409: %s", code, body)
	}
	// The current version deletes; the row is gone.
	var updated Post
	_, body = request(t, base, http.MethodGet, "/api/v1/posts/"+rid, alice, "")
	if err := json.Unmarshal([]byte(body), &updated); err != nil {
		t.Fatal(err)
	}
	if code, body = request(t, base, http.MethodDelete, "/api/v1/posts/"+rid, alice,
		fmt.Sprintf(`{"version":%d}`, updated.Version)); code != 204 {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, _ = request(t, base, http.MethodGet, "/api/v1/posts/"+rid, alice, ""); code != 404 {
		t.Fatalf("deleted post must 404, got %d", code)
	}

	// Swagger serves the spec the route table produced.
	if code, body = request(t, base, http.MethodGet, "/swagger/doc.json", "", ""); code != 200 || !strings.Contains(body, "/api/v1/posts/{rid}") {
		t.Fatalf("swagger spec must document the post routes: %d", code)
	}

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

func registerAndLogin(t *testing.T, base, email string) (token string) {
	t.Helper()
	code, body := request(t, base, http.MethodPost, "/auth/register", "",
		fmt.Sprintf(`{"email":%q,"password":"password123","name":"u"}`, email))
	if code != 201 {
		t.Fatalf("register %s: %d %s", email, code, body)
	}
	code, body = request(t, base, http.MethodPost, "/auth/login", "",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, email))
	if code != 200 {
		t.Fatalf("login %s: %d %s", email, code, body)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &login); err != nil || login.Token == "" {
		t.Fatalf("login response missing token: %s", body)
	}
	return login.Token
}

func request(t *testing.T, base, method, path, token, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

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
