package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// --- test helpers ---

func doJSON(r *gin.Engine, method, path string, body any, token ...string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if len(token) > 0 && token[0] != "" {
		req.Header.Set("Authorization", "Bearer "+token[0])
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func register(t *testing.T, r *gin.Engine, email, password string) string {
	t.Helper()
	w := doJSON(r, "POST", "/auth/register", map[string]string{
		"email": email, "password": password,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("register %s: expected 201, got %d: %s", email, w.Code, w.Body.String())
	}
	var resp struct{ Token string }
	json.NewDecoder(w.Body).Decode(&resp)
	return resp.Token
}

func createPost(t *testing.T, r *gin.Engine, token, title, content string) map[string]any {
	t.Helper()
	w := doJSON(r, "POST", "/api/v1/posts", map[string]string{
		"title": title, "content": content,
	}, token)
	if w.Code != http.StatusCreated {
		t.Fatalf("create post: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var post map[string]any
	json.NewDecoder(w.Body).Decode(&post)
	return post
}

// --- tests ---

func TestBlog_FullCRUD(t *testing.T) {
	r := NewTestRouter()
	token := register(t, r, "alice@test.com", "password123")

	// Create.
	post := createPost(t, r, token, "Hello World", "My first post")
	rid := post["id"].(string)
	if post["status"] != "draft" {
		t.Fatalf("expected draft, got %v", post["status"])
	}

	// Get.
	w := doJSON(r, "GET", "/api/v1/posts/"+rid, nil, token)
	if w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", w.Code)
	}

	// List.
	w = doJSON(r, "GET", "/api/v1/posts", nil, token)
	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", w.Code)
	}
	var listResp struct{ Total int64 }
	json.NewDecoder(w.Body).Decode(&listResp)
	if listResp.Total != 1 {
		t.Fatalf("expected total=1, got %d", listResp.Total)
	}

	// Update — publish.
	published := "published"
	w = doJSON(r, "PUT", "/api/v1/posts/"+rid, map[string]*string{"status": &published}, token)
	if w.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var updated map[string]any
	json.NewDecoder(w.Body).Decode(&updated)
	if updated["status"] != "published" {
		t.Fatalf("expected published, got %v", updated["status"])
	}

	// Delete.
	w = doJSON(r, "DELETE", "/api/v1/posts/"+rid,
		map[string]int{"version": int(updated["version"].(float64))}, token)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify gone.
	w = doJSON(r, "GET", "/api/v1/posts/"+rid, nil, token)
	if w.Code != http.StatusNotFound {
		t.Fatalf("after delete: expected 404, got %d", w.Code)
	}
}

func TestBlog_OwnerIsolation(t *testing.T) {
	r := NewTestRouter()

	alice := register(t, r, "alice@test.com", "password123")
	bob := register(t, r, "bob@test.com", "password123")

	post := createPost(t, r, alice, "Alice's Post", "Content")
	rid := post["id"].(string)

	// Bob cannot see Alice's post.
	w := doJSON(r, "GET", "/api/v1/posts/"+rid, nil, bob)
	if w.Code != http.StatusNotFound {
		t.Fatalf("bob should not see alice's post, got %d", w.Code)
	}

	// Bob's list is empty.
	w = doJSON(r, "GET", "/api/v1/posts", nil, bob)
	var resp struct{ Total int64 }
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Total != 0 {
		t.Fatalf("bob should see 0 posts, got %d", resp.Total)
	}

	// Bob cannot update Alice's post.
	title := "Hacked"
	w = doJSON(r, "PUT", "/api/v1/posts/"+rid, map[string]*string{"title": &title}, bob)
	if w.Code != http.StatusNotFound {
		t.Fatalf("bob should not update alice's post, got %d", w.Code)
	}
}

func TestBlog_UnauthenticatedBlocked(t *testing.T) {
	r := NewTestRouter()

	endpoints := []struct{ method, path string }{
		{"POST", "/api/v1/posts"},
		{"GET", "/api/v1/posts"},
		{"GET", "/api/v1/posts/pst_xxx"},
		{"PUT", "/api/v1/posts/pst_xxx"},
		{"DELETE", "/api/v1/posts/pst_xxx"},
	}
	for _, e := range endpoints {
		w := doJSON(r, e.method, e.path, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401, got %d", e.method, e.path, w.Code)
		}
	}
}

func TestBlog_Pagination(t *testing.T) {
	r := NewTestRouter()
	token := register(t, r, "alice@test.com", "password123")

	for i := range 5 {
		createPost(t, r, token, fmt.Sprintf("Post %d", i+1), "Content")
	}

	w := doJSON(r, "GET", "/api/v1/posts?page=1&size=2", nil, token)
	var resp struct {
		Items []any `json:"items"`
		Total int64 `json:"total"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Items))
	}
	if resp.Total != 5 {
		t.Fatalf("expected total=5, got %d", resp.Total)
	}
}
