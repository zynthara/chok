package swagger

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/handler"
)

// Generate walks the gin engine's registered routes, looks up handler
// metadata (registered automatically by HandleRequest/HandleAction/HandleList),
// and builds a complete OpenAPI 3.0 spec. Returns nil if not enabled.
//
// This approach requires ZERO changes to route registration code:
//
//	// Routes registered normally:
//	rg.POST("/posts", handler.HandleRequest(h.create, handler.WithSuccessCode(201)))
//	rg.GET("/posts", handler.HandleList[model.Post](posts))
//
//	// One line generates the spec:
//	swagger.Generate(&cfg.Swagger, srv.Engine())
func Generate(opts *config.SwaggerOptions, engine *gin.Engine) *Spec {
	if opts == nil || !opts.Enabled {
		return nil
	}

	doc := New(defStr(opts.Title, "API"), defStr(opts.Version, "1.0.0"))
	if opts.BearerAuth {
		doc.BearerAuth()
	}

	Populate(doc, engine)

	prefix := opts.Prefix
	if prefix == "" {
		prefix = "/swagger"
	}
	doc.Mount(engine, prefix)
	return doc
}

// Populate walks the gin engine's registered routes, looks up handler
// metadata (registered by HandleRequest/HandleAction/HandleList), and
// adds the corresponding OpenAPI operations to spec. Nil-safe on both
// arguments; safe to call more than once (later calls add new routes).
func Populate(spec *Spec, engine *gin.Engine) {
	if spec == nil || engine == nil {
		return
	}
	handler.IndexRoutes(engine)
	for _, route := range engine.Routes() {
		meta := handler.LookupRoute(route.Method, route.Path)
		if meta == nil {
			continue
		}

		summary := meta.Summary
		if summary == "" {
			summary = handler.AutoSummary(route.Method, route.Path)
		}
		tags := meta.Tags
		if len(tags) == 0 {
			tags = handler.AutoTags(route.Path)
		}

		if meta.IsList {
			addListOperation(spec, route.Method, route.Path, summary, tags, meta)
		} else {
			spec.addOperation(route.Method, route.Path, Op{
				Summary: summary,
				Tags:    tags,
				Code:    meta.Code,
				Public:  meta.Public,
			}, meta.ReqType, meta.RespType)
		}
	}
}

func addListOperation(doc *Spec, method, ginPath, summary string, tags []string, meta *handler.HandlerMeta) {
	oaPath := ginPathToOpenAPI(ginPath)

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

	pi := doc.paths[oaPath]
	if pi == nil {
		pi = &pathItem{}
		doc.paths[oaPath] = pi
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
	if doc.bearerAuth && !meta.Public {
		oper.Security = []map[string][]string{{"BearerAuth": {}}}
	}
	pi.setMethod("get", oper)
}
