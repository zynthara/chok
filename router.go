package chok

import (
	"bytes"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/zynthara/chok/internal/openapi"
	"github.com/zynthara/chok/internal/reqinfo"
)

// Middleware wraps an http.Handler.
type Middleware = func(http.Handler) http.Handler

// Router registers routes. Patterns follow net/http.ServeMux:
// "/posts/{id}", "/files/{path...}", "/{$}".
type Router interface {
	// Handle registers h for method ("" for any) and pattern below the
	// router's prefix, wrapped by the router's and the given middleware.
	Handle(method, pattern string, h http.Handler, mw ...Middleware)
	// Group returns a router for prefix whose routes also run mw.
	Group(prefix string, mw ...Middleware) Router
}

type router struct {
	app    *App
	prefix string
	mw     []Middleware
}

// Router returns the root router.
func (a *App) Router() Router { return &router{app: a} }

// Group returns a router for routes below prefix that also run mw.
func (a *App) Group(prefix string, mw ...Middleware) Router { return a.Router().Group(prefix, mw...) }

func (r *router) Handle(method, pattern string, h http.Handler, mw ...Middleware) {
	r.register(method, pattern, h, mw, nil)
}

func (r *router) Group(prefix string, mw ...Middleware) Router {
	return &router{app: r.app, prefix: joinPath(r.prefix, prefix), mw: append(slices.Clone(r.mw), mw...)}
}

// register adds a route to the mux and, for the typed routes, to the
// table the OpenAPI document is built from, with the schemes the
// authentication middleware in its chain accepts.
func (r *router) register(method, pattern string, h http.Handler, mw []Middleware, doc *routeDoc) {
	full := joinPath(r.prefix, pattern)
	chain := append(slices.Clone(r.mw), mw...)
	h, security := wrap(h, chain)
	r.app.noteCookieAuth(security)
	key := full
	if method != "" {
		key = method + " " + full
	}
	r.app.mux.Handle(key, routeMarker(full, h))
	if doc != nil {
		doc.method, doc.path, doc.security = method, full, security
		r.app.routes.add(doc)
	}
}

// wrap applies chain to h, outermost first, and collects the OpenAPI
// schemes of the layers that authenticate.
func wrap(h http.Handler, chain []Middleware) (http.Handler, []openapi.Scheme) {
	layers := make([][]openapi.Scheme, len(chain))
	for i, m := range slices.Backward(chain) {
		h = m(h)
		if s, ok := h.(openapi.Securer); ok {
			layers[i] = s.OpenAPISecurity()
		}
	}
	return h, openapi.Collect(layers...)
}

func joinPath(prefix, pattern string) string {
	switch {
	case prefix == "":
		if pattern == "" {
			return "/"
		}
		return pattern
	case pattern == "":
		return prefix
	}
	return strings.TrimRight(prefix, "/") + "/" + strings.TrimLeft(pattern, "/")
}

func routeMarker(pattern string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if info := reqinfo.From(r.Context()); info != nil {
			info.Route = pattern
		}
		next.ServeHTTP(w, r)
	})
}

// routeTable is the documented routes in registration order.
type routeTable struct {
	mu   sync.Mutex
	docs []*routeDoc
}

func (t *routeTable) add(d *routeDoc) {
	t.mu.Lock()
	t.docs = append(t.docs, d)
	t.mu.Unlock()
}

func (t *routeTable) all() []*routeDoc {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.docs)
}

var errMethodNotAllowed = NewError(http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed")

// serveMux answers unmatched requests with the JSON envelope: 404, or
// 405 with the Allow header; ServeMux's redirects pass through.
func (a *App) serveMux(w http.ResponseWriter, r *http.Request) {
	if _, pattern := a.mux.Handler(r); pattern != "" {
		a.mux.ServeHTTP(w, r)
		return
	}
	rec := &muxRecorder{header: http.Header{}}
	a.mux.ServeHTTP(rec, r)
	switch rec.code {
	case http.StatusNotFound:
		a.writeError(w, r, ErrNotFound)
	case http.StatusMethodNotAllowed:
		w.Header().Set("Allow", rec.header.Get("Allow"))
		a.writeError(w, r, errMethodNotAllowed)
	default:
		maps.Copy(w.Header(), rec.header)
		w.WriteHeader(rec.code)
		_, _ = w.Write(rec.body.Bytes())
	}
}

// muxRecorder captures what ServeMux answers an unmatched request with.
type muxRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *muxRecorder) Header() http.Header { return r.header }

func (r *muxRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *muxRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(b)
}

// Handler returns the App's complete handler — the default middleware,
// the application middleware and every route — for tests and for
// embedding in another server. Register components and routes first:
// the middleware stack is fixed by the first call.
func (a *App) Handler() http.Handler {
	a.handlerOnce.Do(func() { a.handler = a.buildHandler() })
	return a.handler
}
