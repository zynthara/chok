package swagger

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

type testCreateReq struct {
	Title   string `json:"title"   binding:"required,max=200"`
	Content string `json:"content" binding:"required"`
}

type testGetReq struct {
	RID string `uri:"rid" binding:"required"`
}

type testUpdateReq struct {
	RID    string  `uri:"rid"    binding:"required"`
	Title  *string `json:"title"  binding:"omitempty,max=200"`
	Status *string `json:"status" binding:"omitempty,oneof=draft published"`
}

type testModel struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func createHandler(_ context.Context, req *testCreateReq) (*testModel, error) {
	return &testModel{Title: req.Title}, nil
}

func getHandler(_ context.Context, req *testGetReq) (*testModel, error) {
	return &testModel{ID: req.RID}, nil
}

func updateHandler(_ context.Context, req *testUpdateReq) (*testModel, error) {
	return &testModel{ID: req.RID}, nil
}

func TestSpec_MarshalJSON(t *testing.T) {
	doc := New("Test API", "1.0.0")
	doc.BearerAuth()

	// Add operations manually via addOperation.
	doc.addOperation("post", "/api/v1/posts", Op{
		Summary: "Create post", Tags: []string{"posts"}, Code: 201,
	}, typeOf[testCreateReq](), typeOf[testModel]())

	doc.addOperation("get", "/api/v1/posts/{rid}", Op{
		Summary: "Get post", Tags: []string{"posts"}, Code: 200,
	}, typeOf[testGetReq](), typeOf[testModel]())

	doc.addOperation("put", "/api/v1/posts/{rid}", Op{
		Summary: "Update post", Tags: []string{"posts"}, Code: 200,
	}, typeOf[testUpdateReq](), typeOf[testModel]())

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	var spec map[string]any
	json.Unmarshal(data, &spec)

	// Verify structure.
	if spec["openapi"] != "3.0.3" {
		t.Fatalf("expected openapi 3.0.3, got %v", spec["openapi"])
	}
	paths := spec["paths"].(map[string]any)
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(paths))
	}
	if _, ok := paths["/api/v1/posts"]; !ok {
		t.Fatal("missing /api/v1/posts path")
	}
	if _, ok := paths["/api/v1/posts/{rid}"]; !ok {
		t.Fatal("missing /api/v1/posts/{rid} path")
	}

	// Verify bearer auth component.
	comps := spec["components"].(map[string]any)
	schemes := comps["securitySchemes"].(map[string]any)
	bearer := schemes["BearerAuth"].(map[string]any)
	if bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		t.Fatalf("unexpected bearer auth: %v", bearer)
	}

	t.Logf("OpenAPI spec:\n%s", data)
}

func TestSchemaFromType_BasicTypes(t *testing.T) {
	s := schemaFromType(typeOf[testCreateReq](), "json")
	if s.Type != "object" {
		t.Fatalf("expected object, got %s", s.Type)
	}
	if s.Properties["title"] == nil {
		t.Fatal("missing title property")
	}
	if s.Properties["title"].MaxLength == nil || *s.Properties["title"].MaxLength != 200 {
		t.Fatal("expected maxLength=200 on title")
	}
	// "required" should include title and content.
	if len(s.Required) != 2 {
		t.Fatalf("expected 2 required fields, got %d: %v", len(s.Required), s.Required)
	}
}

func TestSchemaFromType_Enum(t *testing.T) {
	s := schemaFromType(typeOf[testUpdateReq](), "json")
	statusProp := s.Properties["status"]
	if statusProp == nil {
		t.Fatal("missing status property")
	}
	if len(statusProp.Enum) != 2 || statusProp.Enum[0] != "draft" {
		t.Fatalf("expected enum [draft published], got %v", statusProp.Enum)
	}
	if !statusProp.Nullable {
		t.Fatal("*string should be nullable")
	}
}

func TestExtractParams_URI(t *testing.T) {
	params := extractParams(typeOf[testGetReq]())
	if len(params) != 1 {
		t.Fatalf("expected 1 param, got %d", len(params))
	}
	if params[0].Name != "rid" || params[0].In != "path" || !params[0].Required {
		t.Fatalf("unexpected param: %+v", params[0])
	}
}

func TestGinPathToOpenAPI(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/posts/:rid", "/posts/{rid}"},
		{"/api/v1/posts", "/api/v1/posts"},
		{"/users/:uid/posts/:pid", "/users/{uid}/posts/{pid}"},
	}
	for _, tt := range tests {
		got := ginPathToOpenAPI(tt.in)
		if got != tt.want {
			t.Fatalf("ginPathToOpenAPI(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func typeOf[T any]() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}
