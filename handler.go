package chok

import (
	"context"
	"net/http"
	"net/url"
	"reflect"

	"github.com/zynthara/chok/v3/internal/openapi"
)

// HandlerFunc is a typed handler: chok binds and validates *T from the
// request and renders R, or the error, as the response.
type HandlerFunc[T any, R any] func(ctx context.Context, req *T) (R, error)

// None is a request type that binds nothing, or a response type with no
// body: a handler returning None answers 204 unless Status says otherwise.
type None struct{}

// GET registers fn for GET pattern on r.
//
// Request fields bind by tag: path:"id", query:"q", header:"X-Api-Key",
// and json:"title" from an application/json body (unknown members and
// duplicate keys are rejected, names match case-sensitively). Query,
// header and path values overwrite whatever the body put in their fields.
// A request type with a Default() method gets it called after binding;
// then the validate tags run and a failure is a 400 whose
// metadata.fields names each field by its source name.
func GET[T, R any](r Router, pattern string, fn HandlerFunc[T, R], opts ...RouteOption) {
	handle(r, http.MethodGet, pattern, fn, opts)
}

// POST registers fn for POST pattern on r; see GET for binding.
func POST[T, R any](r Router, pattern string, fn HandlerFunc[T, R], opts ...RouteOption) {
	handle(r, http.MethodPost, pattern, fn, opts)
}

// PUT registers fn for PUT pattern on r; see GET for binding.
func PUT[T, R any](r Router, pattern string, fn HandlerFunc[T, R], opts ...RouteOption) {
	handle(r, http.MethodPut, pattern, fn, opts)
}

// PATCH registers fn for PATCH pattern on r; see GET for binding.
func PATCH[T, R any](r Router, pattern string, fn HandlerFunc[T, R], opts ...RouteOption) {
	handle(r, http.MethodPatch, pattern, fn, opts)
}

// DELETE registers fn for DELETE pattern on r; see GET for binding.
func DELETE[T, R any](r Router, pattern string, fn HandlerFunc[T, R], opts ...RouteOption) {
	handle(r, http.MethodDelete, pattern, fn, opts)
}

func handle[T, R any](r Router, method, pattern string, fn HandlerFunc[T, R], opts []RouteOption) {
	cfg := routeConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	reqType, respType := reflect.TypeFor[T](), reflect.TypeFor[R]()
	b := newBinder(reqType)
	app := appOf(r)
	status := cfg.status
	if status == 0 {
		status = http.StatusOK
		if respType == noneType {
			status = http.StatusNoContent
		}
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		in := new(T)
		if err := b.bind(w, req, in, app.maxBody(cfg)); err != nil {
			app.writeError(w, req, err)
			return
		}
		out, err := fn(req.Context(), in)
		if err != nil {
			app.writeError(w, req, err)
			return
		}
		if respType == noneType {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, req, status, out)
	})
	register(r, method, pattern, h, &routeDoc{cfg: cfg, status: status, req: reqType, resp: respType})
}

// Lister serves a List route: a page from the query string, and the
// fields the query may filter on, which the OpenAPI document lists.
type Lister[T any] interface {
	ListFromQuery(ctx context.Context, q url.Values) (*Page[T], error)
	QueryFields() []string
}

// Page is one page of a list.
type Page[T any] struct {
	Items   []T    `json:"items"`
	Total   int64  `json:"total"`
	Page    int    `json:"page"`
	Size    int    `json:"size"`
	HasMore bool   `json:"has_more"`
	Cursor  string `json:"cursor,omitzero"`
}

// List registers GET pattern on r to serve pages from l: page, size,
// order=field:asc|desc and every field of l.QueryFields() as an equality
// filter come from the query string.
func List[T any](r Router, pattern string, l Lister[T], opts ...RouteOption) {
	cfg := routeConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	app := appOf(r)
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		page, err := l.ListFromQuery(req.Context(), req.URL.Query())
		if err != nil {
			app.writeError(w, req, err)
			return
		}
		if page == nil {
			page = &Page[T]{}
		}
		if page.Items == nil {
			page.Items = []T{}
		}
		writeJSON(w, req, http.StatusOK, page)
	})
	register(r, http.MethodGet, pattern, h, &routeDoc{
		cfg: cfg, status: http.StatusOK,
		req: noneType, resp: reflect.TypeFor[Page[T]](), item: reflect.TypeFor[T](),
		filters: l.QueryFields(), list: true,
	})
}

// RouteOption configures one route.
type RouteOption func(*routeConfig)

// Summary is the route's one-line description in the OpenAPI document;
// without it chok derives one such as "Create post" or "List posts".
func Summary(s string) RouteOption { return func(c *routeConfig) { c.summary = s } }

// Tags groups the route in the OpenAPI document; the default is its
// resource segment.
func Tags(tags ...string) RouteOption { return func(c *routeConfig) { c.tags = tags } }

// Status sets the success status code.
func Status(code int) RouteOption { return func(c *routeConfig) { c.status = code } }

// Created is Status(201).
func Created() RouteOption { return Status(http.StatusCreated) }

// Use adds middleware to this route only.
func Use(mw ...Middleware) RouteOption {
	return func(c *routeConfig) { c.mw = append(c.mw, mw...) }
}

// MaxBody caps this route's request body in bytes, instead of
// http.max_body_bytes.
func MaxBody(n int64) RouteOption { return func(c *routeConfig) { c.maxBody = n } }

// Deprecated marks the route deprecated in the OpenAPI document.
func Deprecated() RouteOption { return func(c *routeConfig) { c.deprecated = true } }

// routeConfig is what the RouteOptions set.
type routeConfig struct {
	summary    string
	tags       []string
	status     int
	deprecated bool
	maxBody    int64
	mw         []Middleware
}

// routeDoc is what the OpenAPI document needs to know about a route.
type routeDoc struct {
	method, path string
	cfg          routeConfig
	status       int
	req, resp    reflect.Type
	// security is what the route's authentication middleware accepts.
	security []openapi.Scheme
	// List routes: the item type and its filter fields.
	list    bool
	item    reflect.Type
	filters []string
}

// appOf returns the App behind a chok Router, or nil for a foreign one:
// routes then render errors without the App's ErrorHook and appear in no
// OpenAPI document.
func appOf(r Router) *App {
	if cr, ok := r.(*router); ok {
		return cr.app
	}
	return nil
}

func register(r Router, method, pattern string, h http.Handler, doc *routeDoc) {
	if cr, ok := r.(*router); ok {
		cr.register(method, pattern, h, doc.cfg.mw, doc)
		return
	}
	r.Handle(method, pattern, h, doc.cfg.mw...)
}

func (a *App) maxBody(c routeConfig) int64 {
	if c.maxBody > 0 {
		return c.maxBody
	}
	if a == nil {
		return 4 << 20
	}
	return a.base.HTTP.MaxBodyBytes
}
