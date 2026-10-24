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
// from chok.yaml: health, the post CRUD as alice with the optimistic lock
// and the error envelopes, the owner fence against bob and the anonymous
// caller, and finally the OpenAPI document. Callers name themselves with
// X-User-Id, the interim devAuthn of devauthn.go, until WP6 brings login.
func TestBlog_QuickstartPath(t *testing.T) {
	t.Setenv("BLOG_DB_SQLITE_PATH", filepath.Join(t.TempDir(), "blog.db"))
	cfg, err := chok.Load[Config]("blog", chok.ConfigFile("chok.yaml"), chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	c := choktest.App(t, cfg.Base, func(app *chok.App) { setup(app, cfg) })
	alice, bob := []string{"X-User-Id", "alice"}, []string{"X-User-Id", "bob"}

	// 1. Health: every component reports in.
	var health struct {
		Status     string `json:"status"`
		Components []struct {
			Component string `json:"component"`
			Status    string `json:"status"`
		} `json:"components"`
	}
	if code := c.GetJSON("/healthz", &health); code != http.StatusOK || health.Status != "up" ||
		len(health.Components) != 1 || health.Components[0].Component != "db" {
		t.Fatalf("healthz = %d %+v", code, health)
	}

	// 2. Anonymous: a write is a 401 envelope, reads find nothing.
	var e envelope
	if code := c.PostJSON("/api/v1/posts", map[string]string{"title": "x", "content": "y"}, &e); code != http.StatusUnauthorized ||
		e.Code != 401 || e.Reason != "Unauthorized" || e.RequestID == "" {
		t.Fatalf("anonymous create = %d %+v", code, e)
	}
	var page chok.Page[Post]
	if code := c.GetJSON("/api/v1/posts", &page); code != http.StatusOK || page.Total != 0 || page.Items == nil {
		t.Fatalf("anonymous list = %d %+v", code, page)
	}

	// 3. Create; the same title again conflicts on the title field, and a
	// missing title is a validation error naming it.
	var p Post
	if code := c.PostJSON("/api/v1/posts", map[string]string{"title": "Hello chok", "content": "my first post"}, &p, alice...); code != http.StatusCreated ||
		!strings.HasPrefix(p.RID, "pst_") || p.Status != "draft" || p.Version != 1 {
		t.Fatalf("create = %d %+v", code, p)
	}
	e = envelope{}
	if code := c.PostJSON("/api/v1/posts", map[string]string{"title": "Hello chok", "content": "again"}, &e, alice...); code != http.StatusConflict ||
		e.Reason != "Conflict" || e.Metadata["field"] != "title" {
		t.Fatalf("duplicate title = %d %+v", code, e)
	}
	e = envelope{}
	if code := c.PostJSON("/api/v1/posts", map[string]string{"content": "no title"}, &e, alice...); code != http.StatusBadRequest ||
		e.Reason != "Validation" || !hasKey(e.Metadata["fields"], "title") {
		t.Fatalf("missing title = %d %+v", code, e)
	}

	// 4. List: owner-scoped, filtered by a query field, ordered.
	page = chok.Page[Post]{}
	if code := c.GetJSON("/api/v1/posts?status=draft&order=created_at:desc", &page, alice...); code != http.StatusOK ||
		page.Total != 1 || page.Page != 1 || len(page.Items) != 1 || page.Items[0].RID != p.RID {
		t.Fatalf("list = %d %+v", code, page)
	}
	e = envelope{}
	if code := c.GetJSON("/api/v1/posts?author=bob", &e, alice...); code != http.StatusBadRequest || !strings.Contains(e.Message, "status") {
		t.Fatalf("unknown filter = %d %+v", code, e)
	}

	// 5. Read one.
	var got Post
	if code := c.GetJSON("/api/v1/posts/"+p.RID, &got, alice...); code != http.StatusOK || got.Title != "Hello chok" {
		t.Fatalf("get = %d %+v", code, got)
	}

	// The owner fence: bob sees nothing of alice's, may reuse her title,
	// and cannot change her post; anonymous reads are 404s.
	page = chok.Page[Post]{}
	if code := c.GetJSON("/api/v1/posts", &page, bob...); code != http.StatusOK || page.Total != 0 {
		t.Fatalf("bob's list = %d %+v", code, page)
	}
	for _, who := range [][]string{bob, nil} {
		e = envelope{}
		if code := c.GetJSON("/api/v1/posts/"+p.RID, &e, who...); code != http.StatusNotFound || e.Reason != "NotFound" {
			t.Fatalf("%v reading alice's post = %d %+v", who, code, e)
		}
	}
	if code := c.PostJSON("/api/v1/posts", map[string]string{"title": "Hello chok", "content": "bob's"}, nil, bob...); code != http.StatusCreated {
		t.Fatalf("bob reusing alice's title = %d", code)
	}
	if code := c.JSON(http.MethodPatch, "/api/v1/posts/"+p.RID, map[string]any{"status": "published"}, nil, bob...); code != http.StatusNotFound {
		t.Fatalf("bob patching alice's post = %d", code)
	}

	// 6. Publish it with the version alice saw; the same, now stale,
	// version is a 409 naming it.
	patch := map[string]any{"version": p.Version, "status": "published"}
	got = Post{}
	if code := c.JSON(http.MethodPatch, "/api/v1/posts/"+p.RID, patch, &got, alice...); code != http.StatusOK ||
		got.Status != "published" || got.Version != p.Version+1 || !got.UpdatedAt.After(p.UpdatedAt) {
		t.Fatalf("patch = %d %+v", code, got)
	}
	e = envelope{}
	if code := c.JSON(http.MethodPatch, "/api/v1/posts/"+p.RID, patch, &e, alice...); code != http.StatusConflict ||
		e.Metadata["version"] != float64(p.Version) {
		t.Fatalf("stale patch = %d %+v", code, e)
	}

	// 7. Delete (204), then reading it is a 404 and its title is free.
	if code := c.JSON(http.MethodDelete, "/api/v1/posts/"+p.RID, nil, nil, alice...); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if code := c.GetJSON("/api/v1/posts/"+p.RID, nil, alice...); code != http.StatusNotFound {
		t.Fatalf("get after delete = %d", code)
	}
	if code := c.PostJSON("/api/v1/posts", map[string]string{"title": "Hello chok", "content": "again"}, nil, alice...); code != http.StatusCreated {
		t.Fatalf("recreate after delete = %d", code)
	}

	// 8. The OpenAPI document lists the routes; the list carries its
	// pagination and filter fields as query parameters.
	var spec struct {
		Info struct {
			Title string `json:"title"`
		} `json:"info"`
		Paths map[string]map[string]struct {
			Summary    string `json:"summary"`
			Parameters []struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if code := c.GetJSON("/openapi.json", &spec); code != http.StatusOK || spec.Info.Title != "Blog API" {
		t.Fatalf("openapi = %d %+v", code, spec.Info)
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

func hasKey(v any, key string) bool {
	m, ok := v.(map[string]any)
	_, has := m[key]
	return ok && has
}
