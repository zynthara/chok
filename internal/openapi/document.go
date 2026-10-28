// Package openapi builds the OpenAPI 3.1 document of a chok App from its
// route table by reflecting over each route's request and response types.
package openapi

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Operation is one documented route.
type Operation struct {
	Method, Path string
	Summary      string
	Tags         []string
	Status       int
	Deprecated   bool
	// Security is the schemes the route's authentication middleware
	// accepts, any one of them; none for a route without one.
	Security []Scheme
	// Request is the bound request type; nil or chok.None documents no
	// parameters and no body.
	Request reflect.Type
	// Response is the success body type; nil documents no body.
	Response reflect.Type
	// ListItem and Filters describe a List route: the item type and the
	// query fields it filters on.
	ListItem reflect.Type
	Filters  []string
}

// Info is the document-level metadata.
type Info struct {
	Title   string
	Version string
}

// Scheme is a security scheme: Name is its key in
// components.securitySchemes, the rest its definition.
type Scheme struct {
	Name         string
	Type         string // http or apiKey
	Scheme       string // http: bearer
	BearerFormat string // http bearer: the token's format
	In           string // apiKey: cookie, header or query
	Param        string // apiKey: the cookie's, header's or parameter's name
}

// Securer is implemented by the handler an authentication middleware
// returns: the schemes the middleware accepts, in order. chok collects
// them from each route's middleware chain into the route's security, so
// the document says a route is protected exactly when a middleware that
// authenticates runs on it. identity's Authn implements it; nothing
// exported does, so an application's own middleware is not recognized.
type Securer interface {
	OpenAPISecurity() []Scheme
}

// BearerAuth is the scheme of a JWT in the Authorization header.
var BearerAuth = Scheme{Name: "bearerAuth", Type: "http", Scheme: "bearer", BearerFormat: "JWT"}

// CookieAuth is the scheme of a session token in the cookie named name.
func CookieAuth(name string) Scheme {
	return Scheme{Name: "cookieAuth", Type: "apiKey", In: "cookie", Param: name}
}

// Collect returns the schemes of a middleware chain's layers, outermost
// first, without repeats: a scheme reported twice keeps its first place.
func Collect(layers ...[]Scheme) []Scheme {
	var out []Scheme
	for _, layer := range layers {
		for _, s := range layer {
			if !slices.ContainsFunc(out, func(o Scheme) bool { return o.Name == s.Name }) {
				out = append(out, s)
			}
		}
	}
	return out
}

type document struct {
	OpenAPI    string               `json:"openapi"`
	Info       info                 `json:"info"`
	Paths      map[string]*pathItem `json:"paths"`
	Components components           `json:"components"`
}

type info struct {
	Title   string `json:"title"`
	Version string `json:"version"`
}

type components struct {
	Schemas         map[string]*Schema         `json:"schemas,omitzero"`
	SecuritySchemes map[string]*securityScheme `json:"securitySchemes,omitzero"`
}

type securityScheme struct {
	Type         string `json:"type"`
	Scheme       string `json:"scheme,omitzero"`
	BearerFormat string `json:"bearerFormat,omitzero"`
	In           string `json:"in,omitzero"`
	Name         string `json:"name,omitzero"`
}

type pathItem struct {
	Get    *operation `json:"get,omitzero"`
	Put    *operation `json:"put,omitzero"`
	Post   *operation `json:"post,omitzero"`
	Delete *operation `json:"delete,omitzero"`
	Patch  *operation `json:"patch,omitzero"`
}

type operation struct {
	Summary     string               `json:"summary,omitzero"`
	OperationID string               `json:"operationId,omitzero"`
	Tags        []string             `json:"tags,omitzero"`
	Deprecated  bool                 `json:"deprecated,omitzero"`
	Parameters  []*parameter         `json:"parameters,omitzero"`
	RequestBody *requestBody         `json:"requestBody,omitzero"`
	Responses   map[string]*response `json:"responses"`
	// Security is nil when no route has a scheme, and points to an
	// empty list for a route without one, so the document says
	// "security": [].
	Security *[]map[string][]string `json:"security,omitzero"`
}

type parameter struct {
	Name        string  `json:"name"`
	In          string  `json:"in"`
	Description string  `json:"description,omitzero"`
	Required    bool    `json:"required,omitzero"`
	Schema      *Schema `json:"schema"`
}

type requestBody struct {
	Required bool                  `json:"required,omitzero"`
	Content  map[string]*mediaType `json:"content"`
}

type response struct {
	Description string                `json:"description"`
	Content     map[string]*mediaType `json:"content,omitzero"`
}

type mediaType struct {
	Schema *Schema `json:"schema"`
}

// Build renders the document for ops.
func Build(in Info, ops []Operation) ([]byte, error) {
	g := newRegistry()
	g.schemas["Error"] = errorSchema()
	doc := document{
		OpenAPI: "3.1.0",
		Info:    info{Title: or(in.Title, "API"), Version: or(in.Version, "0.0.0")},
		Paths:   map[string]*pathItem{},
	}
	// The document's schemes are the union of the routes'; with none,
	// it says nothing about security at all.
	schemes := map[string]*securityScheme{}
	for _, op := range ops {
		for _, s := range op.Security {
			if _, seen := schemes[s.Name]; !seen {
				schemes[s.Name] = &securityScheme{Type: s.Type, Scheme: s.Scheme, BearerFormat: s.BearerFormat, In: s.In, Name: s.Param}
			}
		}
	}
	ids := map[string]int{}
	for _, op := range ops {
		path := oaPath(op.Path)
		o := buildOperation(g, op, path)
		o.OperationID = uniqueID(ids, operationID(op.Method, o.Summary))
		if len(schemes) > 0 {
			sec := []map[string][]string{}
			for _, s := range op.Security {
				sec = append(sec, map[string][]string{s.Name: {}})
			}
			o.Security = &sec
		}
		item := doc.Paths[path]
		if item == nil {
			item = &pathItem{}
			doc.Paths[path] = item
		}
		switch op.Method {
		case http.MethodGet:
			item.Get = o
		case http.MethodPut:
			item.Put = o
		case http.MethodPost:
			item.Post = o
		case http.MethodDelete:
			item.Delete = o
		case http.MethodPatch:
			item.Patch = o
		}
	}
	doc.Components.Schemas = g.schemas
	if len(schemes) > 0 {
		doc.Components.SecuritySchemes = schemes
	}
	return json.Marshal(doc, json.Deterministic(true))
}

func buildOperation(g *registry, op Operation, path string) *operation {
	o := &operation{
		Summary:    or(op.Summary, Summary(op.Method, op.Path, op.ListItem != nil)),
		Tags:       op.Tags,
		Deprecated: op.Deprecated,
		Responses:  map[string]*response{},
	}
	if len(o.Tags) == 0 {
		if r := resource(op.Path); r != "" {
			o.Tags = []string{r}
		}
	}
	params := map[string]bool{}
	if t := op.Request; t != nil && t.Name() != "None" {
		if t.Kind() == reflect.Struct {
			for _, p := range parameters(g, t, nil) {
				params[p.In+" "+p.Name] = true
				o.Parameters = append(o.Parameters, p)
			}
			if body := g.object(t, true); body.Properties != nil {
				o.RequestBody = &requestBody{
					Required: len(body.Required) > 0,
					Content:  map[string]*mediaType{"application/json": {Schema: body}},
				}
			}
		} else {
			o.RequestBody = &requestBody{Required: true, Content: map[string]*mediaType{"application/json": {Schema: g.schemaOf(t)}}}
		}
	}
	for _, name := range pathParams(path) {
		if !params["path "+name] {
			o.Parameters = append(o.Parameters, &parameter{Name: name, In: "path", Required: true, Schema: &Schema{Type: "string"}})
		}
	}
	if op.ListItem != nil {
		o.Parameters = append(o.Parameters, listParameters(g, op.ListItem, op.Filters)...)
	}
	sortParams(o.Parameters)

	status := op.Status
	if status == 0 {
		status = http.StatusOK
	}
	ok := &response{Description: http.StatusText(status)}
	if op.Response != nil && op.Response.Name() != "None" {
		ok.Content = map[string]*mediaType{"application/json": {Schema: g.schemaOf(op.Response)}}
	}
	o.Responses[strconv.Itoa(status)] = ok
	o.Responses["default"] = &response{
		Description: "Error",
		Content:     map[string]*mediaType{"application/json": {Schema: &Schema{Ref: "#/components/schemas/Error"}}},
	}
	return o
}

// parameters collects the path, query and header fields of struct t.
func parameters(g *registry, t reflect.Type, out []*parameter) []*parameter {
	for f := range t.Fields() {
		in, name := "", ""
		for _, tag := range sourceTags {
			if v, ok := f.Tag.Lookup(tag); ok && v != "-" {
				in, name = tag, strings.Split(v, ",")[0]
			}
		}
		if in == "" {
			if f.Anonymous && f.Type.Kind() == reflect.Struct && f.Tag.Get("json") == "" {
				out = parameters(g, f.Type, out)
			}
			continue
		}
		if name == "" {
			name = f.Name
		}
		s := g.schemaOf(f.Type)
		if s.Type != nil {
			s.Type = baseType(s.Type)
		}
		required := applyRules(f.Tag.Get("validate"), itemsOr(s))
		out = append(out, &parameter{Name: name, In: in, Required: in == "path" || required, Schema: s})
	}
	return out
}

func itemsOr(s *Schema) *Schema {
	if s.Items != nil {
		return s.Items
	}
	return s
}

func listParameters(g *registry, item reflect.Type, filters []string) []*parameter {
	one := 1.0
	ps := []*parameter{
		{Name: "page", In: "query", Description: "page number, from 1", Schema: &Schema{Type: "integer", Minimum: &one}},
		{Name: "size", In: "query", Description: "items per page", Schema: &Schema{Type: "integer", Minimum: &one}},
	}
	if len(filters) > 0 {
		var orders []any
		for _, f := range filters {
			orders = append(orders, f+":asc", f+":desc")
		}
		ps = append(ps, &parameter{Name: "order", In: "query", Description: "field:asc or field:desc", Schema: &Schema{Type: "string", Enum: orders}})
	} else {
		ps = append(ps, &parameter{Name: "order", In: "query", Description: "field:asc or field:desc", Schema: &Schema{Type: "string"}})
	}
	props := g.object(derefStruct(item), false).Properties
	for _, f := range filters {
		s := &Schema{Type: "string"}
		if p, ok := props[f]; ok && p.Ref == "" {
			s = &Schema{Type: baseType(p.Type), Format: p.Format, Enum: p.Enum}
		}
		ps = append(ps, &parameter{Name: f, In: "query", Description: "equals", Schema: s})
	}
	return ps
}

func derefStruct(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func sortParams(ps []*parameter) {
	order := map[string]int{"path": 0, "query": 1, "header": 2}
	slices.SortStableFunc(ps, func(a, b *parameter) int { return order[a.In] - order[b.In] })
}

func errorSchema() *Schema {
	return &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"code":       {Type: "integer", Description: "the HTTP status code"},
			"reason":     {Type: "string", Description: "machine-readable, such as NotFound or Validation"},
			"message":    {Type: "string"},
			"request_id": {Type: "string"},
			"metadata":   {Type: "object", Description: "details, such as fields for a validation failure"},
		},
		Required: []string{"code", "message", "reason"},
	}
}

// oaPath turns a ServeMux pattern into an OpenAPI path.
func oaPath(pattern string) string {
	if i := strings.IndexByte(pattern, '/'); i > 0 {
		pattern = pattern[i:] // drop a host
	}
	var segs []string
	for seg := range strings.SplitSeq(pattern, "/") {
		if seg == "{$}" {
			continue
		}
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "...}") {
			seg = strings.TrimSuffix(seg, "...}") + "}"
		}
		segs = append(segs, seg)
	}
	if p := strings.Join(segs, "/"); p != "" {
		return p
	}
	return "/"
}

func pathParams(path string) []string {
	var out []string
	for seg := range strings.SplitSeq(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, seg[1:len(seg)-1])
		}
	}
	return out
}

// Summary derives a route summary: "Create post", "Get post", "List
// posts", "Replace post", "Update post", "Delete post". A POST to a
// segment that is not a plural collection is named after the segment:
// POST /auth/refresh-token is "Refresh token".
func Summary(method, path string, list bool) string {
	segs := staticSegments(path)
	if len(segs) == 0 {
		return method + " /"
	}
	last := segs[len(segs)-1]
	words := strings.ReplaceAll(strings.ReplaceAll(last, "-", " "), "_", " ")
	param := endsWithParam(path)
	switch method {
	case http.MethodGet:
		if param {
			return "Get " + singular(words)
		}
		if list || plural(words) {
			return "List " + words
		}
		return "Get " + words
	case http.MethodPost:
		if param || plural(words) {
			return "Create " + singular(words)
		}
		return capitalize(words)
	case http.MethodPut:
		return "Replace " + singular(words)
	case http.MethodPatch:
		return "Update " + singular(words)
	case http.MethodDelete:
		return "Delete " + singular(words)
	}
	return capitalize(strings.ToLower(method)) + " " + words
}

func staticSegments(path string) []string {
	var out []string
	for seg := range strings.SplitSeq(path, "/") {
		if seg != "" && !strings.HasPrefix(seg, "{") {
			out = append(out, seg)
		}
	}
	return out
}

func endsWithParam(path string) bool {
	path = strings.TrimSuffix(path, "/")
	return strings.HasSuffix(path, "}") && !strings.HasSuffix(path, "{$}")
}

// resource is the last static segment, the default tag.
func resource(path string) string {
	segs := staticSegments(path)
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1]
}

func plural(w string) bool {
	return strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is")
}

func singular(w string) string {
	switch {
	case !plural(w):
		return w
	case strings.HasSuffix(w, "ies"):
		return strings.TrimSuffix(w, "ies") + "y"
	case strings.HasSuffix(w, "sses"), strings.HasSuffix(w, "xes"), strings.HasSuffix(w, "ches"), strings.HasSuffix(w, "shes"):
		return strings.TrimSuffix(w, "es")
	}
	return strings.TrimSuffix(w, "s")
}

func capitalize(s string) string {
	for i, r := range s {
		return string(unicode.ToUpper(r)) + s[i+len(string(r)):]
	}
	return s
}

// operationID is the lowerCamel form of the summary.
func operationID(method, summary string) string {
	var b strings.Builder
	for i, w := range strings.Fields(summary) {
		w = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return r
			}
			return -1
		}, w)
		if w == "" {
			continue
		}
		if i == 0 {
			b.WriteString(strings.ToLower(w))
		} else {
			b.WriteString(capitalize(strings.ToLower(w)))
		}
	}
	if b.Len() == 0 {
		return strings.ToLower(method)
	}
	return b.String()
}

func uniqueID(seen map[string]int, id string) string {
	seen[id]++
	if n := seen[id]; n > 1 {
		return fmt.Sprintf("%s%d", id, n)
	}
	return id
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// UI is the /docs page: Swagger UI from its CDN, pointed at specURL.
func UI(title, specURL string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>%s</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    SwaggerUIBundle({ url: %q, dom_id: '#swagger-ui', presets: [SwaggerUIBundle.presets.apis] });
  </script>
</body>
</html>
`, htmlEscape(title), specURL)
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
