//go:build ignore

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

// TestBlog_QuickstartPath walks README.md's five-minute path in process:
// health, register and login, then the post CRUD with the owner fence,
// the optimistic lock and the error envelopes, and finally the spec.
func TestBlog_QuickstartPath(t *testing.T) {
	t.Setenv("BLOG_DB_SQLITE_PATH", filepath.Join(t.TempDir(), "blog.db"))
	t.Setenv("BLOG_AUTH_SIGNING_KEY", strings.Repeat("k", 32))
	cfg, err := chok.Load[Config]("blog", chok.ConfigFile("chok.yaml"), chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	c := choktest.App(t, cfg.Base, func(app *chok.App) { setup(app, cfg) })

	if code := c.GetJSON("/healthz", nil); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}
	alice := c.WithBearer(registerAndLogin(t, c, "alice@blog.test"))
	bob := c.WithBearer(registerAndLogin(t, c, "bob@blog.test"))

	// No token: 401 with the full envelope.
	var e envelope
	if code := c.GetJSON("/api/v1/posts", &e); code != http.StatusUnauthorized ||
		e.Reason != "Unauthorized" || e.RequestID == "" {
		t.Fatalf("anonymous list = %d %+v", code, e)
	}

	// Create, then a duplicate title conflicts on the title field.
	var p Post
	if code := alice.PostJSON("/api/v1/posts", map[string]string{"title": "Hello chok", "content": "first"}, &p); code != http.StatusCreated ||
		!strings.HasPrefix(p.RID, "pst_") || p.Status != "draft" {
		t.Fatalf("create = %d %+v", code, p)
	}
	e = envelope{}
	if code := alice.PostJSON("/api/v1/posts", map[string]string{"title": "Hello chok", "content": "again"}, &e); code != http.StatusConflict ||
		e.Metadata["field"] != "title" {
		t.Fatalf("duplicate title = %d %+v", code, e)
	}
	// Validation names the JSON field.
	e = envelope{}
	if code := alice.PostJSON("/api/v1/posts", map[string]string{"content": "no title"}, &e); code != http.StatusBadRequest ||
		e.Reason != "Validation" || !strings.Contains(e.Message+toString(e.Metadata["fields"]), "title") {
		t.Fatalf("missing title = %d %+v", code, e)
	}

	// Read back; the list filters by a declared query field.
	var got Post
	if code := alice.GetJSON("/api/v1/posts/"+p.RID, &got); code != http.StatusOK || got.Title != "Hello chok" {
		t.Fatalf("get = %d %+v", code, got)
	}
	var page chok.Page[Post]
	if code := alice.GetJSON("/api/v1/posts?status=draft&order=created_at:desc", &page); code != http.StatusOK ||
		page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("list = %d %+v", code, page)
	}

	// The owner fence: bob sees nothing and cannot read alice's post.
	page = chok.Page[Post]{}
	if code := bob.GetJSON("/api/v1/posts", &page); code != http.StatusOK || page.Total != 0 {
		t.Fatalf("bob's list = %d %+v", code, page)
	}
	e = envelope{}
	if code := bob.GetJSON("/api/v1/posts/"+p.RID, &e); code != http.StatusNotFound || e.Reason != "NotFound" {
		t.Fatalf("bob reading alice's post = %d %+v", code, e)
	}

	// PATCH with the version alice saw publishes the post; replaying the
	// same, now stale, version conflicts.
	patch := map[string]any{"version": p.Version, "status": "published"}
	if code := alice.JSON(http.MethodPatch, "/api/v1/posts/"+p.RID, patch, &got); code != http.StatusOK ||
		got.Status != "published" || got.Version != p.Version+1 {
		t.Fatalf("patch = %d %+v", code, got)
	}
	e = envelope{}
	if code := alice.JSON(http.MethodPatch, "/api/v1/posts/"+p.RID, patch, &e); code != http.StatusConflict {
		t.Fatalf("stale patch = %d %+v", code, e)
	}

	// Delete, then the post is gone.
	if code := alice.JSON(http.MethodDelete, "/api/v1/posts/"+p.RID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if code := alice.GetJSON("/api/v1/posts/"+p.RID, nil); code != http.StatusNotFound {
		t.Fatalf("get after delete = %d", code)
	}

	// The spec documents the routes: login is public, the list carries
	// its filter fields as query parameters.
	var spec struct {
		Paths map[string]map[string]struct {
			Security   []map[string][]string `json:"security"`
			Parameters []struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if code := c.GetJSON("/openapi.json", &spec); code != http.StatusOK {
		t.Fatalf("openapi = %d", code)
	}
	if login := spec.Paths["/auth/login"]["post"]; login.Security == nil || len(login.Security) != 0 {
		t.Fatalf("/auth/login must be public (security: []), got %+v", login.Security)
	}
	if posts := spec.Paths["/api/v1/posts"]["post"]; len(posts.Security) == 0 {
		t.Fatal("POST /api/v1/posts must require the bearer token")
	}
	var params []string
	for _, prm := range spec.Paths["/api/v1/posts"]["get"].Parameters {
		if prm.In == "query" {
			params = append(params, prm.Name)
		}
	}
	for _, want := range []string{"page", "size", "order", "title", "status"} {
		if !slices.Contains(params, want) {
			t.Errorf("list parameters %v lack %q", params, want)
		}
	}
}

func registerAndLogin(t *testing.T, c *choktest.Client, email string) string {
	t.Helper()
	creds := map[string]string{"email": email, "password": "password123", "name": "u"}
	if code := c.PostJSON("/auth/register", creds, nil); code != http.StatusCreated {
		t.Fatalf("register %s = %d", email, code)
	}
	var login struct {
		Token string `json:"token"`
	}
	delete(creds, "name")
	if code := c.PostJSON("/auth/login", creds, &login); code != http.StatusOK || login.Token == "" {
		t.Fatalf("login %s = %d", email, code)
	}
	return login.Token
}

func toString(v any) string {
	if m, ok := v.(map[string]any); ok {
		var b strings.Builder
		for k := range m {
			b.WriteString(k)
		}
		return b.String()
	}
	return ""
}
