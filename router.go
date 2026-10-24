package chok

import (
	"net/http"
	"slices"
	"strings"

	"github.com/zynthara/chok/v3/internal/reqinfo"
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
	r.register(method, pattern, h, mw)
}

func (r *router) Group(prefix string, mw ...Middleware) Router {
	return &router{app: r.app, prefix: joinPath(r.prefix, prefix), mw: append(slices.Clone(r.mw), mw...)}
}

// register adds a route to the mux.
func (r *router) register(method, pattern string, h http.Handler, mw []Middleware) {
	full := joinPath(r.prefix, pattern)
	chain := append(slices.Clone(r.mw), mw...)
	for _, c := range slices.Backward(chain) {
		h = c(h)
	}
	key := full
	if method != "" {
		key = method + " " + full
	}
	h = routeMarker(full, h)
	r.app.mux.Handle(key, h)
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

// Handler returns the App's complete handler — the default middleware,
// the application middleware and every route — for tests and for
// embedding in another server. Register components and routes first:
// the middleware stack is fixed by the first call.
func (a *App) Handler() http.Handler {
	a.handlerOnce.Do(func() { a.handler = a.buildHandler() })
	return a.handler
}
