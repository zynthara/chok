package chok

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zynthara/chok/v3/internal/openapi"
)

var update = flag.Bool("update", false, "rewrite golden files")

// The golden route set: a path parameter, query and header fields, a
// validated JSON body, a named response type, a list and a public route.
type author struct {
	Name  string `json:"name"`
	Email string `json:"email" validate:"omitempty,email"`
}

type article struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Author    *author   `json:"author"`
	Tags      []string  `json:"tags"`
	Words     int       `json:"words"`
	Published *bool     `json:"published"`
	CreatedAt time.Time `json:"created_at"`
}

type createArticle struct {
	Draft   bool     `query:"draft"`
	TraceID string   `header:"X-Trace-Id"`
	Title   string   `json:"title" validate:"required,min=3,max=200"`
	Status  string   `json:"status" validate:"omitempty,oneof=draft published"`
	Words   int      `json:"words" validate:"gte=0,lt=100000"`
	Tags    []string `json:"tags" validate:"max=5,dive,max=20"`
	Author  author   `json:"author"`
	Meta    struct {
		Source string `json:"source"`
	} `json:"meta"`
}

type articleID struct {
	ID string `path:"id" validate:"required"`
}

type updateArticle struct {
	ID     string  `path:"id"`
	Title  *string `json:"title" validate:"omitempty,max=200"`
	Status *string `json:"status" validate:"omitempty,oneof=draft published"`
}

type articles struct{}

func (articles) ListFromQuery(context.Context, url.Values) (*Page[article], error) {
	return &Page[article]{}, nil
}
func (articles) QueryFields() []string { return []string{"status", "words"} }

func goldenApp(t *testing.T) *App {
	base := testBase(t)
	base.HTTP.OpenAPI.Title = "Golden API"
	app := New(base, Logger(quiet()), Version("1.2.3"))
	api := app.Group("/api/v1")
	POST(api, "/articles", func(context.Context, *createArticle) (*article, error) { return nil, nil }, Created())
	List(api, "/articles", articles{})
	GET(api, "/articles/{id}", func(context.Context, *articleID) (*article, error) { return nil, nil })
	PATCH(api, "/articles/{id}", func(context.Context, *updateArticle) (*article, error) { return nil, nil }, Deprecated())
	DELETE(api, "/articles/{id}", func(context.Context, *articleID) (None, error) { return None{}, nil }, Tags("admin"))
	POST(app.Group("/auth"), "/refresh-token", func(context.Context, *None) (map[string]string, error) { return nil, nil },
		Public(), Summary("Refresh the access token"))
	return app
}

func TestOpenAPI_Golden(t *testing.T) {
	app := goldenApp(t)
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/openapi.json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got := rec.Body.Bytes()
	var pretty bytes.Buffer
	if err := json.Unmarshal(got, new(any)); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	v := jsontext.Value(bytes.Clone(got))
	if err := v.Indent(); err != nil {
		t.Fatal(err)
	}
	pretty.Write(v)
	pretty.WriteByte('\n')
	path := filepath.Join("testdata", "openapi.golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run TestOpenAPI_Golden -update)", err)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Fatalf("the document changed; review and rerun with -update:\n%s", pretty.String())
	}
}

type oaDoc struct {
	Paths map[string]map[string]struct {
		Summary    string                 `json:"summary"`
		Security   *[]map[string][]string `json:"security"`
		Parameters []struct {
			Name     string `json:"name"`
			In       string `json:"in"`
			Required bool   `json:"required"`
		} `json:"parameters"`
		RequestBody *struct {
			Content map[string]struct {
				Schema map[string]any `json:"schema"`
			} `json:"content"`
		} `json:"requestBody"`
		Responses map[string]struct {
			Content map[string]struct {
				Schema map[string]any `json:"schema"`
			} `json:"content"`
		} `json:"responses"`
	} `json:"paths"`
	Components struct {
		Schemas map[string]map[string]any `json:"schemas"`
	} `json:"components"`
}

func spec(t *testing.T, app *App) oaDoc {
	t.Helper()
	return decode[oaDoc](t, do(t, app.Handler(), call{method: http.MethodGet, path: "/openapi.json"}))
}

func TestOpenAPI_PublicRouteHasEmptySecurity(t *testing.T) {
	d := spec(t, goldenApp(t))
	refresh := d.Paths["/auth/refresh-token"]["post"]
	if refresh.Security == nil || len(*refresh.Security) != 0 {
		t.Fatalf("public: %+v", refresh.Security)
	}
	create := d.Paths["/api/v1/articles"]["post"]
	if create.Security == nil || len(*create.Security) != 1 {
		t.Fatalf("secured: %+v", create.Security)
	}
}

func TestOpenAPI_WithoutBearerAuthHasNoSecurity(t *testing.T) {
	base := testBase(t)
	base.HTTP.OpenAPI.BearerAuth = false
	app := New(base, Logger(quiet()))
	GET(app.Router(), "/x", func(context.Context, *None) (None, error) { return None{}, nil })
	if op := spec(t, app).Paths["/x"]["get"]; op.Security != nil {
		t.Fatalf("got %+v", op.Security)
	}
}

// TestOpenAPI_CookieAuthHook: the internal hook identity calls in cookie
// mode adds the cookie scheme to the App behind a router, a group's
// included; a foreign router is ignored.
func TestOpenAPI_CookieAuthHook(t *testing.T) {
	app := New(testBase(t), Logger(quiet()))
	GET(app.Router(), "/x", func(context.Context, *None) (None, error) { return None{}, nil })
	openapi.UseCookieAuth(app.Group("/auth"), "sid")
	openapi.UseCookieAuth(http.NewServeMux(), "ignored")
	op := spec(t, app).Paths["/x"]["get"]
	if op.Security == nil || len(*op.Security) != 2 || (*op.Security)[1]["cookieAuth"] == nil {
		t.Fatalf("security %+v", op.Security)
	}
}

func TestOpenAPI_ListDocumentsFilters(t *testing.T) {
	list := spec(t, goldenApp(t)).Paths["/api/v1/articles"]["get"]
	names := map[string]bool{}
	for _, p := range list.Parameters {
		if p.In == "query" {
			names[p.Name] = true
		}
	}
	for _, want := range []string{"page", "size", "order", "status", "words"} {
		if !names[want] {
			t.Fatalf("list parameters %v lack %s", names, want)
		}
	}
	if list.Summary != "List articles" {
		t.Fatalf("summary %q", list.Summary)
	}
}

func TestOpenAPI_NamedTypesAreRefs(t *testing.T) {
	d := spec(t, goldenApp(t))
	resp := d.Paths["/api/v1/articles/{id}"]["get"].Responses["200"].Content["application/json"].Schema
	if resp["$ref"] != "#/components/schemas/article" {
		t.Fatalf("response: %v", resp)
	}
	if _, ok := d.Components.Schemas["article"]; !ok {
		t.Fatal("named types land in components")
	}
	if _, ok := d.Components.Schemas["Page_article"]; !ok {
		t.Fatalf("a generic page gets a clean name: %v", keys(d.Components.Schemas))
	}
	body := d.Paths["/api/v1/articles"]["post"].RequestBody.Content["application/json"].Schema
	props := body["properties"].(map[string]any)
	if _, ok := props["draft"]; ok {
		t.Fatal("query fields are parameters, not body members")
	}
	if meta := props["meta"].(map[string]any); meta["$ref"] != nil {
		t.Fatal("an anonymous struct is inlined")
	}
	if props["author"].(map[string]any)["$ref"] != "#/components/schemas/author" {
		t.Fatal("a named nested type is a $ref")
	}
}

func TestOpenAPI_SummariesAndBuiltinsLeftOut(t *testing.T) {
	d := spec(t, goldenApp(t))
	want := map[string]string{
		"POST /api/v1/articles": "Create article", "GET /api/v1/articles/{id}": "Get article",
		"PATCH /api/v1/articles/{id}": "Update article", "DELETE /api/v1/articles/{id}": "Delete article",
		"POST /auth/refresh-token": "Refresh the access token",
	}
	for k, w := range want {
		var method, path string
		for i := range k {
			if k[i] == ' ' {
				method, path = k[:i], k[i+1:]
				break
			}
		}
		if got := d.Paths[path][lower(method)].Summary; got != w {
			t.Fatalf("%s: %q, want %q", k, got, w)
		}
	}
	for _, p := range []string{"/healthz", "/openapi.json", "/docs"} {
		if _, ok := d.Paths[p]; ok {
			t.Fatalf("%s must not be documented", p)
		}
	}
}

func lower(s string) string { return string(bytes.ToLower([]byte(s))) }

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestOpenAPI_DocsPageAndDisable(t *testing.T) {
	app := goldenApp(t)
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/docs"})
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"/openapi.json"`)) || !bytes.Contains(rec.Body.Bytes(), []byte("Golden API")) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	base := testBase(t)
	base.HTTP.OpenAPI.Enabled = false
	off := New(base, Logger(quiet()))
	if rec := do(t, off.Handler(), call{method: http.MethodGet, path: "/openapi.json"}); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: %d", rec.Code)
	}
}

func TestDebug_PprofOnlyWhenEnabled(t *testing.T) {
	app := newTestApp(t)
	if rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/debug/pprof/"}); rec.Code != http.StatusNotFound {
		t.Fatalf("off by default: %d", rec.Code)
	}
	base := testBase(t)
	base.App.Debug = true
	dbg := New(base, Logger(quiet()))
	for _, p := range []string{"/debug/pprof/", "/debug/pprof/goroutine?debug=1", "/debug/pprof/goroutineleak?debug=1"} {
		if rec := do(t, dbg.Handler(), call{method: http.MethodGet, path: p}); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Body)
		}
	}
}
