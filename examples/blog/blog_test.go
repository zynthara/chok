package main

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/choktest"
)

// envelope is the error body every failing request returns.
type envelope struct {
	Code      int            `json:"code"`
	Reason    string         `json:"reason"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Metadata  map[string]any `json:"metadata"`
}

// TestBlog_QuickstartPath walks README.md's five-minute path in process,
// from chok.yaml: health, register and log in, the post CRUD as alice
// with the optimistic lock and the error envelopes, the owner fence
// against bob, the anonymous caller turned away, and finally the OpenAPI
// document.
func TestBlog_QuickstartPath(t *testing.T) {
	t.Setenv("BLOG_DB_SQLITE_PATH", filepath.Join(t.TempDir(), "blog.db"))
	t.Setenv("BLOG_AUTH_SIGNING_KEY", strings.Repeat("k", 32))
	cfg, err := chok.Load[Config]("blog", chok.ConfigFile("chok.yaml"), chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	c := choktest.App(t, cfg.Base, func(app *chok.App) { setup(app, cfg) })

	// 1. Health: every component reports in.
	var health struct {
		Status     string `json:"status"`
		Components []struct {
			Component string `json:"component"`
			Status    string `json:"status"`
		} `json:"components"`
	}
	if code := c.GetJSON("/healthz", &health); code != http.StatusOK || health.Status != "up" || len(health.Components) != 2 ||
		health.Components[0].Component != "db" || health.Components[1].Component != "identity" {
		t.Fatalf("healthz = %d %+v", code, health)
	}

	// 2. Register and log in: the token goes with every request after.
	alice := c.WithBearer(registerAndLogin(t, c, "alice@example.com", "Alice"))
	bob := c.WithBearer(registerAndLogin(t, c, "bob@example.com", "Bob"))
	var e envelope
	if code := c.PostJSON("/auth/register", map[string]string{"email": "alice@example.com", "password": "password123"}, &e); code != http.StatusConflict ||
		e.Metadata["field"] != "email" {
		t.Fatalf("registering alice again = %d %+v", code, e)
	}

	// Without a token every route is a 401 envelope: write, list and read.
	for _, anon := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/posts"}, {http.MethodGet, "/api/v1/posts"}, {http.MethodGet, "/api/v1/posts/pst_missing"},
	} {
		e = envelope{}
		var body any
		if anon.method == http.MethodPost {
			body = map[string]string{"title": "x", "content": "y"}
		}
		if code := c.JSON(anon.method, anon.path, body, &e); code != http.StatusUnauthorized || e.Code != 401 || e.Reason != "Unauthorized" || e.RequestID == "" {
			t.Fatalf("anonymous %s %s = %d %+v", anon.method, anon.path, code, e)
		}
	}
	e = envelope{}
	if code := c.GetJSON("/api/v1/posts", &e, "Authorization", "Bearer not-a-token"); code != http.StatusUnauthorized || e.Reason != "Unauthorized" {
		t.Fatalf("a forged token = %d %+v", code, e)
	}

	// 3. Create; the same title again conflicts on the title field, and a
	// missing title is a validation error naming it.
	var p Post
	if code := alice.PostJSON("/api/v1/posts", map[string]string{"title": "Hello chok", "content": "my first post"}, &p); code != http.StatusCreated ||
		!strings.HasPrefix(p.RID, "pst_") || p.Status != "draft" || p.Version != 1 {
		t.Fatalf("create = %d %+v", code, p)
	}
	e = envelope{}
	if code := alice.PostJSON("/api/v1/posts", map[string]string{"title": "Hello chok", "content": "again"}, &e); code != http.StatusConflict ||
		e.Reason != "Conflict" || e.Metadata["field"] != "title" {
		t.Fatalf("duplicate title = %d %+v", code, e)
	}
	e = envelope{}
	if code := alice.PostJSON("/api/v1/posts", map[string]string{"content": "no title"}, &e); code != http.StatusBadRequest ||
		e.Reason != "Validation" || !hasKey(e.Metadata["fields"], "title") {
		t.Fatalf("missing title = %d %+v", code, e)
	}

	// 4. List: owner-scoped, filtered by a query field, ordered.
	var page chok.Page[Post]
	if code := alice.GetJSON("/api/v1/posts?status=draft&order=created_at:desc", &page); code != http.StatusOK ||
		page.Total != 1 || page.Page != 1 || len(page.Items) != 1 || page.Items[0].RID != p.RID {
		t.Fatalf("list = %d %+v", code, page)
	}
	e = envelope{}
	if code := alice.GetJSON("/api/v1/posts?author=bob", &e); code != http.StatusBadRequest || !strings.Contains(e.Message, "status") {
		t.Fatalf("unknown filter = %d %+v", code, e)
	}

	// 5. Read one.
	var got Post
	if code := alice.GetJSON("/api/v1/posts/"+p.RID, &got); code != http.StatusOK || got.Title != "Hello chok" {
		t.Fatalf("get = %d %+v", code, got)
	}

	// The owner fence: bob sees nothing of alice's, reads her post as a
	// 404, may reuse her title, and cannot change her post.
	page = chok.Page[Post]{}
	if code := bob.GetJSON("/api/v1/posts", &page); code != http.StatusOK || page.Total != 0 {
		t.Fatalf("bob's list = %d %+v", code, page)
	}
	e = envelope{}
	if code := bob.GetJSON("/api/v1/posts/"+p.RID, &e); code != http.StatusNotFound || e.Reason != "NotFound" {
		t.Fatalf("bob reading alice's post = %d %+v", code, e)
	}
	if code := bob.PostJSON("/api/v1/posts", map[string]string{"title": "Hello chok", "content": "bob's"}, nil); code != http.StatusCreated {
		t.Fatalf("bob reusing alice's title = %d", code)
	}
	if code := bob.JSON(http.MethodPatch, "/api/v1/posts/"+p.RID, map[string]any{"status": "published"}, nil); code != http.StatusNotFound {
		t.Fatalf("bob patching alice's post = %d", code)
	}

	// 6. Publish it with the version alice saw; the same, now stale,
	// version is a 409 naming it.
	patch := map[string]any{"version": p.Version, "status": "published"}
	got = Post{}
	if code := alice.JSON(http.MethodPatch, "/api/v1/posts/"+p.RID, patch, &got); code != http.StatusOK ||
		got.Status != "published" || got.Version != p.Version+1 || !got.UpdatedAt.After(p.UpdatedAt) {
		t.Fatalf("patch = %d %+v", code, got)
	}
	e = envelope{}
	if code := alice.JSON(http.MethodPatch, "/api/v1/posts/"+p.RID, patch, &e); code != http.StatusConflict ||
		e.Metadata["version"] != float64(p.Version) {
		t.Fatalf("stale patch = %d %+v", code, e)
	}

	// 7. Delete (204), then reading it is a 404 and its title is free.
	if code := alice.JSON(http.MethodDelete, "/api/v1/posts/"+p.RID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if code := alice.GetJSON("/api/v1/posts/"+p.RID, nil); code != http.StatusNotFound {
		t.Fatalf("get after delete = %d", code)
	}
	if code := alice.PostJSON("/api/v1/posts", map[string]string{"title": "Hello chok", "content": "again"}, nil); code != http.StatusCreated {
		t.Fatalf("recreate after delete = %d", code)
	}

	// The signed-in user, as the token names it.
	var me struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if code := alice.GetJSON("/auth/me", &me); code != http.StatusOK || me.Email != "alice@example.com" || me.Name != "Alice" || !strings.HasPrefix(me.ID, "usr_") {
		t.Fatalf("me = %d %+v", code, me)
	}

	// 8. The OpenAPI document lists the routes: login and register are
	// public, the posts and me need the bearer token, and the list carries
	// its pagination and filter fields as query parameters.
	var spec struct {
		Info struct {
			Title string `json:"title"`
		} `json:"info"`
		Paths map[string]map[string]struct {
			Summary    string                `json:"summary"`
			Security   []map[string][]string `json:"security"`
			Parameters []struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if code := c.GetJSON("/openapi.json", &spec); code != http.StatusOK || spec.Info.Title != "Blog API" {
		t.Fatalf("openapi = %d %+v", code, spec.Info)
	}
	for _, public := range []string{"/auth/login", "/auth/register"} {
		if op := spec.Paths[public]["post"]; op.Security == nil || len(op.Security) != 0 {
			t.Errorf("%s must be public (security: []), got %+v", public, op.Security)
		}
	}
	for route, method := range map[string]string{"/api/v1/posts": "post", "/api/v1/posts/{id}": "patch", "/auth/me": "get"} {
		if op := spec.Paths[route][method]; len(op.Security) != 1 || op.Security[0]["bearerAuth"] == nil {
			t.Errorf("%s %s must require the bearer token, got %+v", method, route, op.Security)
		}
	}
	list := spec.Paths["/api/v1/posts"]["get"]
	var params []string
	for _, prm := range list.Parameters {
		if prm.In == "query" {
			params = append(params, prm.Name)
		}
	}
	for _, want := range []string{"page", "size", "order", "title", "status"} {
		if !slices.Contains(params, want) {
			t.Errorf("list parameters %v lack %q", params, want)
		}
	}
	if list.Summary != "List my posts" || spec.Paths["/api/v1/posts/{id}"]["patch"].Summary == "" {
		t.Errorf("summaries: %+v", spec.Paths)
	}
}

// registerAndLogin signs a user up, then logs in, as README.md does, and
// returns the token.
func registerAndLogin(t *testing.T, c *choktest.Client, email, name string) string {
	t.Helper()
	if code := c.PostJSON("/auth/register", map[string]string{"email": email, "password": "password123", "name": name}, nil); code != http.StatusCreated {
		t.Fatalf("register %s = %d", email, code)
	}
	var login struct {
		Token string `json:"token"`
	}
	if code := c.PostJSON("/auth/login", map[string]string{"email": email, "password": "password123"}, &login); code != http.StatusOK || login.Token == "" {
		t.Fatalf("login %s = %d", email, code)
	}
	return login.Token
}

func hasKey(v any, key string) bool {
	m, ok := v.(map[string]any)
	_, has := m[key]
	return ok && has
}
