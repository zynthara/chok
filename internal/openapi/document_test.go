package openapi

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOAPath(t *testing.T) {
	cases := map[string]string{
		"/posts/{id}":         "/posts/{id}",
		"/files/{path...}":    "/files/{path}",
		"/{$}":                "/",
		"/a/{$}":              "/a",
		"example.com/x/{id}":  "/x/{id}",
		"/":                   "/",
		"/api/v1/posts/{id}/": "/api/v1/posts/{id}/",
	}
	for in, want := range cases {
		if got := oaPath(in); got != want {
			t.Errorf("oaPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSummary(t *testing.T) {
	cases := []struct {
		method, path string
		list         bool
		want         string
	}{
		{"POST", "/api/v1/posts", false, "Create post"},
		{"GET", "/api/v1/posts/{id}", false, "Get post"},
		{"GET", "/api/v1/posts", true, "List posts"},
		{"GET", "/api/v1/posts", false, "List posts"},
		{"PUT", "/posts/{id}", false, "Replace post"},
		{"PATCH", "/posts/{id}", false, "Update post"},
		{"DELETE", "/posts/{id}", false, "Delete post"},
		{"POST", "/auth/login", false, "Login"},
		{"POST", "/auth/refresh-token", false, "Refresh token"},
		{"GET", "/auth/me", false, "Get me"},
		{"GET", "/categories/{id}", false, "Get category"},
		{"POST", "/addresses", false, "Create address"},
		{"GET", "/status", false, "Get status"},
		{"POST", "/boxes", false, "Create box"},
	}
	for _, c := range cases {
		if got := Summary(c.method, c.path, c.list); got != c.want {
			t.Errorf("Summary(%s %s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

func TestOperationID(t *testing.T) {
	ids := map[string]int{}
	if got := uniqueID(ids, operationID("GET", "List my posts")); got != "listMyPosts" {
		t.Fatal(got)
	}
	if got := uniqueID(ids, operationID("GET", "List my posts")); got != "listMyPosts2" {
		t.Fatalf("duplicates are numbered: %s", got)
	}
	if got := operationID("POST", "!!"); got != "post" {
		t.Fatal(got)
	}
}

type basic struct {
	S   string            `json:"s"`
	I   int64             `json:"i"`
	U   uint8             `json:"u"`
	F   float32           `json:"f"`
	B   bool              `json:"b"`
	T   time.Time         `json:"t"`
	D   time.Duration     `json:"d"`
	P   *string           `json:"p"`
	L   []int             `json:"l"`
	M   map[string]bool   `json:"m"`
	A   any               `json:"a"`
	Raw []byte            `json:"raw"`
	N   int               `json:"n,string"`
	X   string            `json:"-"`
	Q   string            `json:"q,omitzero"`
	E   map[string]*basic `json:"e"`
	low string            //nolint:unused // proves unexported fields are skipped
}

func TestSchema_BasicTypes(t *testing.T) {
	g := newRegistry()
	s := g.object(reflect.TypeFor[basic](), false)
	want := map[string]string{
		"s": `{"type":"string"}`, "i": `{"type":"integer"}`, "u": `{"type":"integer"}`, "f": `{"type":"number"}`,
		"b": `{"type":"boolean"}`, "t": `{"type":"string","format":"date-time"}`, "p": `{"type":["string","null"]}`,
		"l": `{"type":"array","items":{"type":"integer"}}`, "m": `{"type":"object","additionalProperties":{"type":"boolean"}}`,
		"a": `{}`, "raw": `{"type":"string","format":"byte"}`, "n": `{"type":"string"}`,
		"e": `{"type":"object","additionalProperties":{"$ref":"#/components/schemas/basic"}}`,
	}
	for k, w := range want {
		b, _ := json.Marshal(s.Properties[k], json.Deterministic(true))
		if string(b) != w {
			t.Errorf("%s: %s, want %s", k, b, w)
		}
	}
	for _, k := range []string{"-", "X", "low"} {
		if _, ok := s.Properties[k]; ok {
			t.Errorf("%s must be left out", k)
		}
	}
	if !strings.Contains(s.Properties["d"].Description, "duration") {
		t.Error("durations are strings with a hint")
	}
}

type rules struct {
	Mode  string   `json:"mode" validate:"required,oneof=fast slow"`
	Level int      `json:"level" validate:"oneof=1 2 3"`
	Email string   `json:"email" validate:"omitempty,email"`
	Name  string   `json:"name" validate:"min=2,max=8"`
	Score float64  `json:"score" validate:"gt=0,lte=10"`
	List  []string `json:"list" validate:"min=1,dive,max=3"`
	Exact string   `json:"exact" validate:"len=4"`
}

func TestSchema_ValidateRules(t *testing.T) {
	s := newRegistry().object(reflect.TypeFor[rules](), false)
	b, _ := json.Marshal(s, json.Deterministic(true))
	for _, want := range []string{
		`"mode":{"type":"string","enum":["fast","slow"]}`,
		`"level":{"type":"integer","enum":[1,2,3]}`,
		`"email":{"type":"string","format":"email"}`,
		`"name":{"type":"string","minLength":2,"maxLength":8}`,
		`"score":{"type":"number","maximum":10,"exclusiveMinimum":0}`,
		`"list":{"type":"array","items":{"type":"string","maxLength":3},"minItems":1}`,
		`"exact":{"type":"string","minLength":4,"maxLength":4}`,
		`"required":["mode"]`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("schema lacks %s:\n%s", want, b)
		}
	}
}

type pageOf[T any] struct {
	Items []T `json:"items"`
}

type node struct {
	Name     string  `json:"name"`
	Children []*node `json:"children"`
}

func TestSchema_ComponentsAndNames(t *testing.T) {
	g := newRegistry()
	s := g.schemaOf(reflect.TypeFor[pageOf[node]]())
	if s.Ref != "#/components/schemas/pageOf_node" {
		t.Fatalf("generic names are cleaned: %s", s.Ref)
	}
	n := g.schemas["node"]
	if n == nil || n.Properties["children"].Items.Ref != "#/components/schemas/node" {
		t.Fatal("a recursive type refers to itself")
	}
	type local struct{ A int }
	if r := g.schemaOf(reflect.TypeFor[struct{ L local }]()); r.Ref != "" || r.Properties["L"].Ref == "" {
		t.Fatal("anonymous structs inline, named ones are refs")
	}
}

type params struct {
	ID     string   `path:"id"`
	Q      string   `query:"q" validate:"required"`
	Filter []string `query:"filter" validate:"dive,oneof=a b"`
	Key    string   `header:"X-Key"`
	Body   string   `json:"body"`
}

func TestParameters(t *testing.T) {
	g := newRegistry()
	ps := parameters(g, reflect.TypeFor[params](), nil)
	got := map[string]*parameter{}
	for _, p := range ps {
		got[p.In+":"+p.Name] = p
	}
	if len(ps) != 4 || !got["path:id"].Required || !got["query:q"].Required || got["header:X-Key"].Required {
		t.Fatalf("got %+v", got)
	}
	if f := got["query:filter"].Schema; f.Type != "array" || len(f.Items.Enum) != 2 {
		t.Fatalf("list parameters carry item rules: %+v", f)
	}
}

func TestBuild_DocumentShape(t *testing.T) {
	doc, err := Build(Info{Title: "T", BearerAuth: true}, []Operation{
		{Method: "GET", Path: "/things/{id}", Request: reflect.TypeFor[params](), Response: reflect.TypeFor[node](), Status: 200},
		{Method: "POST", Path: "/login", Public: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	for _, want := range []string{
		`"openapi":"3.1.0"`, `"title":"T"`, `"version":"0.0.0"`,
		`"securitySchemes":{"bearerAuth":{"type":"http","scheme":"bearer","bearerFormat":"JWT"}}`,
		`"security":[]`, `"security":[{"bearerAuth":[]}]`, `"$ref":"#/components/schemas/Error"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document lacks %s:\n%s", want, s)
		}
	}
}

func TestUI_EscapesTitle(t *testing.T) {
	page := UI(`<b>"x"</b>`, "/openapi.json")
	if strings.Contains(page, "<b>") || !strings.Contains(page, `url: "/openapi.json"`) {
		t.Fatalf("%s", page)
	}
}
