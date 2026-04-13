// Package swagger provides automatic OpenAPI 3.0 spec generation from
// chok's typed handler system. Route registration and spec collection
// happen in a single call — zero annotations required.
package swagger

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/config"
)

// Spec collects OpenAPI metadata during route registration.
type Spec struct {
	info       specInfo
	paths      map[string]*pathItem
	bearerAuth bool
}

// New creates an OpenAPI spec builder.
func New(title, version string) *Spec {
	return &Spec{
		info:  specInfo{Title: title, Version: version},
		paths: make(map[string]*pathItem),
	}
}

// FromConfig creates a Spec from config. Returns nil if not enabled.
// Deprecated: Use Setup for automatic BearerAuth + Mount.
func FromConfig(opts *config.SwaggerOptions) *Spec {
	if opts == nil || !opts.Enabled {
		return nil
	}
	return New(defStr(opts.Title, "API"), defStr(opts.Version, "1.0.0"))
}

// Setup creates a Spec from config, applies BearerAuth if configured,
// and mounts the swagger UI — all in one call. Returns nil if not enabled.
//
//	doc := swagger.Setup(&cfg.Swagger, srv.Engine())
//	swagger.Post(doc, api, "/posts", h.create, swagger.Op{...})
func Setup(opts *config.SwaggerOptions, r interface{ GET(string, ...gin.HandlerFunc) gin.IRoutes }) *Spec {
	if opts == nil || !opts.Enabled {
		return nil
	}
	doc := New(defStr(opts.Title, "API"), defStr(opts.Version, "1.0.0"))
	if opts.BearerAuth {
		doc.BearerAuth()
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = "/swagger"
	}
	doc.Mount(r, prefix)
	return doc
}

func defStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// BearerAuth declares a JWT Bearer security scheme.
// All operations added after this call require authentication by default.
// Set Op.Public = true for public endpoints.
// Nil-safe: no-op when Spec is nil (swagger disabled).
func (s *Spec) BearerAuth() {
	if s == nil {
		return
	}
	s.bearerAuth = true
}

// Op configures a single OpenAPI operation.
type Op struct {
	Summary     string
	Description string
	Tags        []string
	Code        int  // success status code override
	Deprecated  bool
	Public      bool // true = no auth required
}

// addOperation adds an operation to the spec. Nil-safe.
func (s *Spec) addOperation(method, ginPath string, op Op, reqType, respType reflect.Type) {
	if s == nil {
		return
	}
	oaPath := ginPathToOpenAPI(ginPath)
	method = strings.ToLower(method)

	pi := s.paths[oaPath]
	if pi == nil {
		pi = &pathItem{}
		s.paths[oaPath] = pi
	}

	oper := &operation{
		Summary:     op.Summary,
		Description: op.Description,
		Tags:        op.Tags,
		Deprecated:  op.Deprecated,
		Responses:   make(map[string]*response),
	}

	// Security.
	if s.bearerAuth && !op.Public {
		oper.Security = []map[string][]string{{"BearerAuth": {}}}
	}

	// Parameters (uri + form fields).
	if reqType != nil {
		oper.Parameters = extractParams(reqType)
	}

	// Request body (json fields only).
	if reqType != nil {
		bodySchema := schemaFromType(reqType, "json")
		if len(bodySchema.Properties) > 0 {
			oper.RequestBody = &requestBody{
				Required: true,
				Content: map[string]*mediaType{
					"application/json": {Schema: bodySchema},
				},
			}
		}
	}

	// Success response.
	code := op.Code
	if code == 0 {
		code = http.StatusOK
	}
	codeStr := http.StatusText(code)
	if respType != nil && respType.Kind() != reflect.Invalid {
		respSchema := schemaFromType(respType, "")
		oper.Responses[statusCode(code)] = &response{
			Description: codeStr,
			Content: map[string]*mediaType{
				"application/json": {Schema: respSchema},
			},
		}
	} else {
		oper.Responses[statusCode(code)] = &response{Description: codeStr}
	}

	// Error responses.
	if s.bearerAuth && !op.Public {
		oper.Responses["401"] = &response{Description: "Unauthenticated"}
	}

	pi.setMethod(method, oper)
}

// MarshalJSON outputs the full OpenAPI 3.0 spec. Nil-safe: returns "null".
func (s *Spec) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("null"), nil
	}
	spec := &openAPISpec{
		OpenAPI: "3.0.3",
		Info:    s.info,
		Paths:   s.paths,
	}
	if s.bearerAuth {
		spec.Components = &components{
			SecuritySchemes: map[string]*securityScheme{
				"BearerAuth": {
					Type:         "http",
					Scheme:       "bearer",
					BearerFormat: "JWT",
				},
			},
		}
	}
	return json.Marshal(spec)
}

// Mount registers swagger UI and spec JSON on the server.
//
//	doc.Mount(srv, "/swagger")
//
// Serves:
//
//	GET /swagger/doc.json — OpenAPI JSON
//	GET /swagger/          — Swagger UI
// Mount registers swagger UI and spec JSON on the server.
// Nil-safe: no-op when Spec is nil (swagger disabled).
func (s *Spec) Mount(r interface{ GET(string, ...gin.HandlerFunc) gin.IRoutes }, prefix string) {
	if s == nil {
		return
	}
	prefix = strings.TrimRight(prefix, "/")
	r.GET(prefix+"/*any", func(c *gin.Context) {
		if strings.HasSuffix(c.Request.URL.Path, "/doc.json") {
			c.Header("Access-Control-Allow-Origin", "*")
			c.JSON(http.StatusOK, s)
			return
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, swaggerHTML(prefix+"/doc.json"))
	})
}

// --- OpenAPI 3.0 types ---

type openAPISpec struct {
	OpenAPI    string               `json:"openapi"`
	Info       specInfo             `json:"info"`
	Paths      map[string]*pathItem `json:"paths"`
	Components *components          `json:"components,omitempty"`
}

type specInfo struct {
	Title   string `json:"title"`
	Version string `json:"version"`
}

type pathItem struct {
	Get    *operation `json:"get,omitempty"`
	Post   *operation `json:"post,omitempty"`
	Put    *operation `json:"put,omitempty"`
	Patch  *operation `json:"patch,omitempty"`
	Delete *operation `json:"delete,omitempty"`
}

func (pi *pathItem) setMethod(method string, op *operation) {
	switch method {
	case "get":
		pi.Get = op
	case "post":
		pi.Post = op
	case "put":
		pi.Put = op
	case "patch":
		pi.Patch = op
	case "delete":
		pi.Delete = op
	}
}

type operation struct {
	Summary     string                `json:"summary,omitempty"`
	Description string                `json:"description,omitempty"`
	Tags        []string              `json:"tags,omitempty"`
	Parameters  []Parameter           `json:"parameters,omitempty"`
	RequestBody *requestBody          `json:"requestBody,omitempty"`
	Responses   map[string]*response  `json:"responses"`
	Security    []map[string][]string `json:"security,omitempty"`
	Deprecated  bool                  `json:"deprecated,omitempty"`
}

// Parameter is an OpenAPI Parameter Object.
type Parameter struct {
	Name     string  `json:"name"`
	In       string  `json:"in"` // path, query
	Required bool    `json:"required,omitempty"`
	Schema   *Schema `json:"schema,omitempty"`
}

type requestBody struct {
	Required bool                  `json:"required,omitempty"`
	Content  map[string]*mediaType `json:"content"`
}

type response struct {
	Description string                `json:"description"`
	Content     map[string]*mediaType `json:"content,omitempty"`
}

type mediaType struct {
	Schema *Schema `json:"schema"`
}

type components struct {
	SecuritySchemes map[string]*securityScheme `json:"securitySchemes,omitempty"`
}

type securityScheme struct {
	Type         string `json:"type"`
	Scheme       string `json:"scheme"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

// --- helpers ---

// ginPathToOpenAPI converts Gin's :param to OpenAPI {param}.
func ginPathToOpenAPI(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = "{" + p[1:] + "}"
		}
	}
	return strings.Join(parts, "/")
}

func statusCode(code int) string {
	return strconv.Itoa(code)
}
