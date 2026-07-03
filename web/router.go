package web

import (
	"net/http"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/internal/ctxval"
	"github.com/zynthara/chok/v2/kernel"
)

// Router / Middleware are the kernel contracts; user code sees the web
// spellings (SPEC §3.2 — the definition lives in kernel so the mount
// phase never depends on this package).
type Router = kernel.Router

// Middleware is a plain http.Handler decorator.
type Middleware = kernel.Middleware

// HandlerMeta is the optional interface a registered http.Handler may
// implement to contribute route metadata (request/response types,
// summary, tags) to the route table. handler.HandleRequest /
// HandleAction / HandleList construct such handlers; the router
// type-asserts at registration — metadata stays a web-layer concern,
// the kernel.Router contract does not grow (SPEC §4.2 item 1).
type HandlerMeta interface {
	Meta() handler.Meta
}

// Route is one route-table entry, in registration order. Meta is nil
// for plain handlers (health endpoints, user http.HandlerFunc values).
type Route struct {
	Method  string
	Pattern string // full pattern including group prefixes
	Meta    *handler.Meta
}

// errMethodNotAllowed mirrors the v1 NoMethod envelope.
var errMethodNotAllowed = apierr.New(http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed")

// router implements kernel.Router on http.ServeMux, owning the route
// table swagger generates from. Groups share the root state; only the
// prefix and middleware chain differ.
type router struct {
	root   *routerRoot
	prefix string
	mw     []Middleware
}

type routerRoot struct {
	mux    *http.ServeMux
	routes []Route
}

func newRouter() *router {
	return &router{root: &routerRoot{mux: http.NewServeMux()}}
}

// Handle implements kernel.Router. The handler is wrapped by the group
// chain plus the per-route middleware (outermost first), and a
// dispatch shim records the matched pattern into the request's
// route-pattern slot for access log / RED metrics / tracing labels.
//
// Invalid or conflicting patterns panic at registration time (ServeMux
// semantics) — same fail-fast class as gin's route-conflict panics,
// surfacing during the mount phase, not at request time.
func (r *router) Handle(method, pattern string, h http.Handler, mw ...Middleware) {
	full := r.prefix + pattern
	if meta, ok := h.(HandlerMeta); ok {
		m := meta.Meta()
		r.root.routes = append(r.root.routes, Route{Method: method, Pattern: full, Meta: &m})
	} else {
		r.root.routes = append(r.root.routes, Route{Method: method, Pattern: full})
	}

	chain := append(append([]Middleware{}, r.mw...), mw...)
	for i := len(chain) - 1; i >= 0; i-- {
		h = chain[i](h)
	}

	final := h
	r.root.mux.Handle(method+" "+full, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctxval.RoutePatternHolder(req.Context()).Set(full)
		final.ServeHTTP(w, req)
	}))
}

// Group implements kernel.Router: prefix concatenation plus middleware
// inheritance. An empty prefix makes a middleware-only group.
func (r *router) Group(prefix string, mw ...Middleware) Router {
	return &router{
		root:   r.root,
		prefix: r.prefix + prefix,
		mw:     append(append([]Middleware{}, r.mw...), mw...),
	}
}

// Routes returns the route table in registration order.
func (r *router) Routes() []Route {
	return append([]Route(nil), r.root.routes...)
}

// ServeHTTP dispatches through the mux. Matched requests — including
// ServeMux's canonicalization redirects (trailing-slash, path
// cleaning; declared v2 behaviour, SPEC §4.2 item 3) — go straight
// through. For unmatched requests the mux's internal handler is
// probed against a recorder and translated into the apierr envelope:
// 404 keeps the v1 NoRoute body, 405 keeps the v1 NoMethod body plus
// the mux-computed Allow header. Anything else (future mux behaviours)
// is replayed verbatim rather than guessed at.
func (r *router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if _, pattern := r.root.mux.Handler(req); pattern != "" {
		r.root.mux.ServeHTTP(w, req)
		return
	}
	r.serveUnmatched(w, req)
}

func (r *router) serveUnmatched(w http.ResponseWriter, req *http.Request) {
	h, _ := r.root.mux.Handler(req) // the internal 404/405 writer
	probe := &probeRecorder{header: make(http.Header)}
	h.ServeHTTP(probe, req)

	switch probe.status {
	case http.StatusNotFound:
		handler.WriteError(w, req, apierr.ErrNotFound.WithMessage("route not found"))
	case http.StatusMethodNotAllowed:
		if allow := probe.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		handler.WriteError(w, req, errMethodNotAllowed)
	default:
		for k, vs := range probe.header {
			w.Header()[k] = vs
		}
		w.WriteHeader(probe.status)
		_, _ = w.Write(probe.body)
	}
}

// probeRecorder captures the mux's internal unmatched response
// (status + headers + tiny plain-text body) without touching the real
// connection. Real handlers never run against it.
type probeRecorder struct {
	header http.Header
	status int
	body   []byte
}

func (p *probeRecorder) Header() http.Header { return p.header }

func (p *probeRecorder) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}

func (p *probeRecorder) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	p.body = append(p.body, b...)
	return len(b), nil
}
