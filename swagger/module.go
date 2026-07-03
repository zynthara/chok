package swagger

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/web"
)

// Options is the "swagger" yaml section. `enabled` defaults to true —
// v2 assembly is intent (SPEC §3.1; the v1 yaml default was false, a
// declared change). BearerAuth adds the JWT security scheme and marks
// non-Public operations as requiring it, matching v1.
type Options struct {
	Enabled    bool   `mapstructure:"enabled"     default:"true"`
	Title      string `mapstructure:"title"                          reload:"restart"`
	Version    string `mapstructure:"version"     default:"1.0.0"    reload:"restart"`
	Prefix     string `mapstructure:"prefix"      default:"/swagger" reload:"restart"`
	BearerAuth bool   `mapstructure:"bearer_auth" default:"true"     reload:"restart"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Prefix == "" || !strings.HasPrefix(o.Prefix, "/") {
		return fmt.Errorf("swagger: prefix must start with /, got %q", o.Prefix)
	}
	return nil
}

// Module returns the swagger component for chok.Use.
func Module() kernel.Component { return &component{} }

type component struct {
	k    kernel.Kernel
	opts Options
	spec *Spec
}

// Describe declares MountOrder 100: swagger mounts after the user
// Routes callback so the route table it reads is complete — the
// kernel orders by the field, it knows no battery names (SPEC §3.3).
// The hard dependency on http turns "swagger without a web module"
// into a directly diagnosable startup error.
func (c *component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:       "swagger",
		ConfigKey:  "swagger",
		Options:    Options{},
		Needs:      []kernel.Dep{{Kind: "http"}},
		MountOrder: 100,
	}
}

func (c *component) Init(ctx context.Context, k kernel.Kernel) error {
	c.k = k
	if err := k.Config().Section("swagger", &c.opts); err != nil {
		return fmt.Errorf("swagger: decode section: %w", err)
	}
	return nil
}

func (c *component) Close(ctx context.Context) error { return nil }

// Mount implements kernel.Mounter. Running last (MountOrder 100), it
// snapshots the web route table into an OpenAPI spec — only routes
// carrying handler metadata contribute, exactly the v1 Populate filter
// — and registers the spec + UI endpoints.
func (c *component) Mount(r kernel.Router) error {
	webc, ok := kernel.Get[*web.Component](c.k, "http")
	if !ok {
		return fmt.Errorf("swagger: http component unavailable (route table unreachable)")
	}

	c.spec = BuildSpec(defStr(c.opts.Title, "API"), c.opts.Version, c.opts.BearerAuth, webc.Routes())

	prefix := strings.TrimRight(c.opts.Prefix, "/")
	r.Handle(http.MethodGet, prefix+"/doc.json", http.HandlerFunc(c.serveSpec))
	r.Handle(http.MethodGet, prefix+"/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/doc.json") {
			c.serveSpec(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(swaggerHTML(prefix + "/doc.json")))
	}))
	return nil
}

func (c *component) serveSpec(w http.ResponseWriter, _ *http.Request) {
	data, err := c.spec.MarshalJSON()
	if err != nil {
		http.Error(w, `{"code":500,"reason":"InternalError","message":"spec encoding failed"}`, http.StatusInternalServerError)
		return
	}
	// v1 served the spec with a permissive CORS header so external
	// Swagger UIs can fetch it; preserved.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}

// Spec exposes the generated spec (nil before mount) — tests and the
// future `chok openapi export` read it here.
func (c *component) Spec() *Spec { return c.spec }

// BuildSpec assembles an OpenAPI spec from a web route table. Pure —
// unit-testable without a kernel; Mount is a thin wrapper over it.
func BuildSpec(title, version string, bearerAuth bool, routes []web.Route) *Spec {
	doc := New(title, defStr(version, "1.0.0"))
	if bearerAuth {
		doc.BearerAuth()
	}
	for _, rt := range routes {
		if rt.Meta == nil {
			continue // plain handlers don't document themselves (v1 filter)
		}
		meta := *rt.Meta

		summary := meta.Summary
		if summary == "" {
			summary = handler.AutoSummary(rt.Method, rt.Pattern)
		}
		tags := meta.Tags
		if len(tags) == 0 {
			tags = handler.AutoTags(rt.Pattern)
		}

		if meta.IsList {
			doc.addListOperation(rt.Method, rt.Pattern, summary, tags, meta)
			continue
		}
		doc.addOperation(rt.Method, rt.Pattern, Op{
			Summary: summary,
			Tags:    tags,
			Code:    meta.Code,
			Public:  meta.Public,
		}, meta.ReqType, meta.RespType)
	}
	return doc
}

// addListOperation renders the HandleList envelope (items/total) with
// the standard pagination parameters — carried over from the v1
// generate.go.
func (s *Spec) addListOperation(method, pattern, summary string, tags []string, meta handler.Meta) {
	oaPath := patternToOpenAPI(pattern)

	itemSchema := schemaFromType(meta.RespType, "")
	listSchema := &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"items": {Type: "array", Items: itemSchema},
			"total": {Type: "integer"},
		},
	}

	params := []Parameter{
		{Name: "page", In: "query", Schema: &Schema{Type: "integer"}},
		{Name: "size", In: "query", Schema: &Schema{Type: "integer"}},
		{Name: "order", In: "query", Schema: &Schema{Type: "string"}},
	}

	pi := s.paths[oaPath]
	if pi == nil {
		pi = &pathItem{}
		s.paths[oaPath] = pi
	}

	oper := &operation{
		Summary:    summary,
		Tags:       tags,
		Parameters: params,
		Responses: map[string]*response{
			statusCode(http.StatusOK): {
				Description: http.StatusText(http.StatusOK),
				Content: map[string]*mediaType{
					"application/json": {Schema: listSchema},
				},
			},
		},
	}
	if s.bearerAuth && !meta.Public {
		oper.Security = []map[string][]string{{"BearerAuth": {}}}
	}
	pi.setMethod(strings.ToLower(method), oper)
}
