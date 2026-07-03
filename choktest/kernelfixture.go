package choktest

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/kernel"
)

// TestRouter is a kernel.Router double: it records registrations and
// serves them for httptest-driven assertions — mount behaviour is
// testable without any real HTTP server (SPEC §10 M1 acceptance).
type TestRouter struct {
	root   *routerState
	prefix string
	mw     []kernel.Middleware
}

type routerState struct {
	mu     sync.Mutex
	routes []TestRoute
}

// TestRoute is one recorded registration, in mount order.
type TestRoute struct {
	Method  string
	Pattern string
	Handler http.Handler
}

// NewTestRouter constructs an empty double.
func NewTestRouter() *TestRouter {
	return &TestRouter{root: &routerState{}}
}

// Handle implements kernel.Router.
func (r *TestRouter) Handle(method, pattern string, h http.Handler, mw ...kernel.Middleware) {
	full := r.prefix + pattern
	chain := append(append([]kernel.Middleware{}, r.mw...), mw...)
	for i := len(chain) - 1; i >= 0; i-- {
		h = chain[i](h)
	}
	r.root.mu.Lock()
	defer r.root.mu.Unlock()
	r.root.routes = append(r.root.routes, TestRoute{Method: method, Pattern: full, Handler: h})
}

// Group implements kernel.Router.
func (r *TestRouter) Group(prefix string, mw ...kernel.Middleware) kernel.Router {
	return &TestRouter{
		root:   r.root,
		prefix: r.prefix + prefix,
		mw:     append(append([]kernel.Middleware{}, r.mw...), mw...),
	}
}

// Routes returns every recorded registration in mount order.
func (r *TestRouter) Routes() []TestRoute {
	r.root.mu.Lock()
	defer r.root.mu.Unlock()
	return append([]TestRoute(nil), r.root.routes...)
}

// Patterns returns "METHOD pattern" strings in mount order.
func (r *TestRouter) Patterns() []string {
	routes := r.Routes()
	out := make([]string, len(routes))
	for i, rt := range routes {
		out[i] = rt.Method + " " + rt.Pattern
	}
	return out
}

// Handler returns the mounted handler for an exact method+pattern.
func (r *TestRouter) Handler(method, pattern string) (http.Handler, bool) {
	for _, rt := range r.Routes() {
		if rt.Method == method && rt.Pattern == pattern {
			return rt.Handler, true
		}
	}
	return nil, false
}

// routerProvider satisfies kernel.RouterProvider for fixtures.
type routerProvider struct {
	router *TestRouter
}

func (p *routerProvider) Describe() kernel.Descriptor {
	return kernel.Descriptor{Kind: "testrouter"}
}
func (p *routerProvider) Init(context.Context, kernel.Kernel) error { return nil }
func (p *routerProvider) Close(context.Context) error               { return nil }
func (p *routerProvider) ProvideRouter() kernel.Router              { return p.router }

// NewRouterProviderComponent wraps a TestRouter in a component that
// fills the kernel's RouterProvider role — for fixture apps that
// assemble Mounters before the web module exists (M1-M4).
func NewRouterProviderComponent(r *TestRouter) kernel.Component {
	return &routerProvider{router: r}
}

// TestKernel bundles a started registry with its TestRouter.
type TestKernel struct {
	*kernel.Registry
	Router *TestRouter
}

// NewTestKernel builds a conf store from the yaml literal (may be
// empty), assembles the components plus a TestRouter provider, starts
// the registry and returns it. Stop is registered as test cleanup.
// Any assembly or startup error fails the test — use StartKernel for
// fail-fast-path assertions.
func NewTestKernel(t testing.TB, yaml string, comps ...kernel.Component) *TestKernel {
	t.Helper()
	tk, err := StartKernel(t, yaml, comps...)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// StartKernel is the non-fatal variant of NewTestKernel: it hands the
// assembly/startup error back so fail-fast paths (misconfiguration,
// unsatisfied explicit dependencies) are assertable. On success the
// registry Stop is registered as test cleanup, same as NewTestKernel.
func StartKernel(t testing.TB, yaml string, comps ...kernel.Component) (*TestKernel, error) {
	t.Helper()

	loader := conf.NewLoader("choktest", "CHOKTEST")
	if yaml != "" {
		dir := t.TempDir()
		path := filepath.Join(dir, "choktest.yaml")
		if err := os.WriteFile(path, []byte(strings.TrimSpace(yaml)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		loader.SetPath(path)
	}
	for _, c := range comps {
		d := c.Describe()
		if key := kernel.SectionKeyOf(d); key != "" && d.Options != nil {
			if err := loader.Register(key, d.Options); err != nil {
				t.Fatal(err)
			}
		}
	}
	store, err := conf.NewStore(loader)
	if err != nil {
		return nil, err
	}

	router := NewTestRouter()
	all := append([]kernel.Component{NewRouterProviderComponent(router)}, comps...)
	reg, err := kernel.New(kernel.Config{Store: store, Components: all})
	if err != nil {
		return nil, err
	}
	if err := reg.Start(context.Background()); err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = reg.Stop(context.Background()) })
	return &TestKernel{Registry: reg, Router: router}, nil
}
