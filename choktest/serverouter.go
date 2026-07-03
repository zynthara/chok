package choktest

import (
	"net/http"

	"github.com/zynthara/chok/v2/kernel"
)

// ServeRouter is a kernel.Router backed by a real http.ServeMux — for
// tests that must actually dispatch requests (path parameters via
// r.PathValue, per-method routing, cookies) without assembling the web
// module. Group prefixing and middleware wrapping mirror the web
// router's semantics: group middleware wraps outside per-route
// middleware, outermost first.
//
// TestRouter (the recording double) stays the right tool for mount
// assertions; ServeRouter is for end-to-end handler behaviour.
type ServeRouter struct {
	mux    *http.ServeMux
	prefix string
	mw     []kernel.Middleware
}

// NewServeRouter constructs an empty serving router.
func NewServeRouter() *ServeRouter {
	return &ServeRouter{mux: http.NewServeMux()}
}

// Handle implements kernel.Router.
func (s *ServeRouter) Handle(method, pattern string, h http.Handler, mw ...kernel.Middleware) {
	chain := append(append([]kernel.Middleware{}, s.mw...), mw...)
	for i := len(chain) - 1; i >= 0; i-- {
		h = chain[i](h)
	}
	s.mux.Handle(method+" "+s.prefix+pattern, h)
}

// Group implements kernel.Router.
func (s *ServeRouter) Group(prefix string, mw ...kernel.Middleware) kernel.Router {
	return &ServeRouter{
		mux:    s.mux,
		prefix: s.prefix + prefix,
		mw:     append(append([]kernel.Middleware{}, s.mw...), mw...),
	}
}

// ServeHTTP dispatches through the underlying mux.
func (s *ServeRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}
