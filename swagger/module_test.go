package swagger

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/store/where"
	"github.com/zynthara/chok/v2/web"
)

// metaOf extracts the Meta a constructed handler carries, as the web
// router would at registration.
func metaOf(t *testing.T, h http.Handler) *handler.Meta {
	t.Helper()
	hm, ok := h.(interface{ Meta() handler.Meta })
	if !ok {
		t.Fatal("handler carries no Meta")
	}
	m := hm.Meta()
	return &m
}

func TestBuildSpec_FromRouteTable(t *testing.T) {
	type createReq struct {
		Title string `json:"title" binding:"required,max=200"`
	}
	type getReq struct {
		RID string `uri:"rid" binding:"required"`
	}
	type model struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}

	create := handler.HandleRequest(
		func(_ context.Context, r *createReq) (*model, error) { return nil, nil },
		handler.WithSuccessCode(201), handler.WithSummary("Create post"), handler.WithTags("posts"),
	)
	get := handler.HandleRequest(
		func(_ context.Context, r *getReq) (*model, error) { return nil, nil },
		handler.WithPublic(),
	)

	routes := []web.Route{
		{Method: "POST", Pattern: "/api/v1/posts", Meta: metaOf(t, create)},
		{Method: "GET", Pattern: "/api/v1/posts/{rid}", Meta: metaOf(t, get)},
		{Method: "GET", Pattern: "/healthz", Meta: nil}, // plain handler — excluded
	}

	doc := BuildSpec("Test API", "9.9.9", true, routes)
	raw, err := doc.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}

	paths := spec["paths"].(map[string]any)
	if len(paths) != 2 {
		t.Fatalf("expected 2 documented paths (healthz excluded), got %d: %v", len(paths), paths)
	}

	post := paths["/api/v1/posts"].(map[string]any)["post"].(map[string]any)
	if post["summary"] != "Create post" {
		t.Fatalf("summary = %v", post["summary"])
	}
	if _, hasSecurity := post["security"]; !hasSecurity {
		t.Fatal("bearer-auth operation must declare security")
	}
	if _, has201 := post["responses"].(map[string]any)["201"]; !has201 {
		t.Fatal("success code 201 not rendered")
	}

	get2 := paths["/api/v1/posts/{rid}"].(map[string]any)["get"].(map[string]any)
	// Auto-derived summary for the un-annotated handler.
	if get2["summary"] != "Get posts" {
		t.Fatalf("auto summary = %v", get2["summary"])
	}
	// Public handler: no security requirement.
	if _, hasSecurity := get2["security"]; hasSecurity {
		t.Fatal("public operation must not declare security")
	}
	// Path parameter extracted from the uri tag.
	params := get2["parameters"].([]any)
	p0 := params[0].(map[string]any)
	if p0["name"] != "rid" || p0["in"] != "path" {
		t.Fatalf("path parameter = %v", p0)
	}
}

func TestBuildSpec_ListOperationEnvelope(t *testing.T) {
	type item struct {
		Name string `json:"name"`
	}
	lister := listerFunc[item](func(context.Context) ([]item, int64, error) { return nil, 0, nil })
	h := handler.HandleList[item](lister, handler.WithSummary("List items"), handler.WithTags("items"))

	doc := BuildSpec("t", "", false, []web.Route{
		{Method: "GET", Pattern: "/items", Meta: metaOf(t, h)},
	})
	raw, _ := doc.MarshalJSON()
	var spec map[string]any
	json.Unmarshal(raw, &spec)

	get := spec["paths"].(map[string]any)["/items"].(map[string]any)["get"].(map[string]any)
	if get["summary"] != "List items" {
		t.Fatalf("summary = %v", get["summary"])
	}
	params := get["parameters"].([]any)
	if len(params) != 3 {
		t.Fatalf("expected page/size/order params, got %v", params)
	}
	schema := get["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	if _, ok := props["items"]; !ok {
		t.Fatal("list envelope missing items")
	}
	if _, ok := props["total"]; !ok {
		t.Fatal("list envelope missing total")
	}
}

// listerFunc adapts a func to handler.QueryLister for tests.
type listerFunc[T any] func(context.Context) ([]T, int64, error)

func (f listerFunc[T]) ListFromQuery(ctx context.Context, _ url.Values) (*where.Page[T], error) {
	items, total, err := f(ctx)
	return &where.Page[T]{Items: items, Total: total}, err
}
