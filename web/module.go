// Package web is the chok v2 HTTP layer: a stdlib http.Server plus a
// ServeMux-backed implementation of the kernel Router/RouterProvider
// contracts (SPEC §4). The module owns the default middleware stack —
// RED metrics and the dedicated access log included — wiring soft
// dependencies (metrics, tracing, authz) by role, never by import.
package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/internal/clientip"
	"github.com/zynthara/chok/v2/internal/ctxval"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/middleware"
)

// ModOpt configures the module at assembly time.
type ModOpt func(*Component)

// WithMiddleware appends user middleware after the default stack
// (Recovery → RequestID → ClientIP → [Tracing] → Logger → [Timeout] →
// [Metrics] → [AccessLog] → [AttachAuthz] → [error mappers]), just
// before the router.
func WithMiddleware(mw ...kernel.Middleware) ModOpt {
	return func(c *Component) { c.extra = append(c.extra, mw...) }
}

// Module returns the http component for chok.Use.
func Module(opts ...ModOpt) kernel.Component {
	c := &Component{router: newRouter()}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Component is the http module. Exported so peers can reach the route
// table: swagger does
//
//	webc, ok := chok.Get[*web.Component](k, "http")
//	for _, rt := range webc.Routes() { ... }
type Component struct {
	opts    Options
	router  *router
	handler http.Handler
	extra   []kernel.Middleware

	mappers      *apierr.MapperRegistry
	accessCloser io.Closer

	logger    log.Logger
	boundAddr atomic.Value // string; set once listening
}

// Describe implements kernel.Component. Kind is "http" — the module
// name is an implementation detail, the capability is not (SPEC §6).
func (c *Component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "http",
		ConfigKey: "http",
		Options:   Options{},
		Needs: []kernel.Dep{
			{Kind: "log", Optional: true},
			{Kind: "metrics", Optional: true},
			{Kind: "tracing", Optional: true},
			{Kind: "authz", Optional: true},
		},
	}
}

// AttachErrorMappers receives the per-App mapper registry during App
// assembly (structural handshake, mini-SPEC §5) — the WithErrorMapper
// injection point moved here from the v1 gin mount hook (SPEC §9).
func (c *Component) AttachErrorMappers(reg *apierr.MapperRegistry) { c.mappers = reg }

// ProvideRouter implements kernel.RouterProvider: this module fills
// the single router-provider role for the mount phase.
func (c *Component) ProvideRouter() kernel.Router { return c.router }

// Routes exposes the route table (registration order) — swagger's data
// source.
func (c *Component) Routes() []Route { return c.router.Routes() }

// BoundAddr returns the actual listen address once Serve is up (""
// before) — fixtures bind :0 and discover the port here.
func (c *Component) BoundAddr() string {
	if v, ok := c.boundAddr.Load().(string); ok {
		return v
	}
	return ""
}

// Init decodes the section and assembles the request pipeline. Soft
// dependencies initialized before us (declared in Needs) are looked up
// by role interface; each absent one simply drops its middleware —
// no error, no stub (M2 acceptance: "absent ⇒ not mounted, no error").
func (c *Component) Init(ctx context.Context, k kernel.Kernel) error {
	if err := k.Config().Section("http", &c.opts); err != nil {
		return fmt.Errorf("web: decode section: %w", err)
	}
	// The kernel logging contract is a subset of log.Logger; the root
	// logger the App builds satisfies the full interface. A bare
	// kernel.Logger double (tests) falls back to the inert logger.
	if l, ok := k.Logger().(log.Logger); ok {
		c.logger = l
	} else {
		c.logger = log.Empty()
	}

	resolver, err := clientip.NewResolver(c.opts.TrustedProxies)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	mws := []kernel.Middleware{
		middleware.Recovery(c.logger),
		middleware.RequestID(),
		middleware.ClientIP(resolver),
	}

	// Tracing: span per request when the tracing module is assembled,
	// enabled and initialized. Placed before Logger so trace ids are
	// available for log correlation.
	if tc, ok := kernel.Get[interface {
		Enabled() bool
		ServiceName() string
	}](k, "tracing"); ok && tc.Enabled() {
		mws = append(mws, middleware.Tracing(tc.ServiceName()))
	}

	mws = append(mws, middleware.Logger(c.logger))

	if c.opts.RequestTimeout > 0 {
		mws = append(mws, middleware.Timeout(c.opts.RequestTimeout))
	}

	// RED metrics: restored M1 integration — when the metrics module is
	// assembled, instrument every request against its registry.
	if mc, ok := kernel.Get[interface {
		Registry() *prometheus.Registry
	}](k, "metrics"); ok {
		mws = append(mws, middleware.Metrics(mc.Registry()))
	}

	// Access log: restored M1 integration. The log section's
	// access_enabled / access_files keep their v1 semantics: dedicated
	// rotating files when configured, the root logger otherwise
	// (mini-SPEC §9 records the cross-module section read).
	if al := c.buildAccessLogger(k); al != nil {
		mws = append(mws, middleware.AccessLog(al))
	}

	// Authz: soft-dependency assembly of the request-context attach.
	// The authz module arrives in M4; the mechanism (and its absence
	// path) is delivered and tested now.
	if ac, ok := kernel.Get[interface {
		Authorizer() authz.Authorizer
	}](k, "authz"); ok {
		if az := ac.Authorizer(); az != nil {
			mws = append(mws, middleware.AttachAuthz(az))
		}
	}

	// Per-App error mappers into every request context (v1 mounted this
	// inside the gin engine; the injection point is now module assembly).
	if c.mappers != nil {
		reg := c.mappers
		mws = append(mws, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(apierr.WithMapperRegistry(r.Context(), reg)))
			})
		})
	}

	mws = append(mws, c.extra...)

	// Root handler: written-tracking writer + route-pattern slot first,
	// then the middleware onion, the router innermost. Unmatched
	// requests run the full stack — middleware sit outside the mux
	// (SPEC §4.2 matrix: preserved v1 behaviour).
	var h http.Handler = c.router
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	inner := h
	c.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := ctxval.WithRoutePattern(r.Context())
		inner.ServeHTTP(Wrap(w), r.WithContext(ctx))
	})
	return nil
}

// buildAccessLogger mirrors v1 LoggerComponent.buildAccessLogger: nil
// when access logging is disabled, a dedicated rotating logger when
// access_files is set (owned by this module — closed in Close), the
// root logger otherwise.
func (c *Component) buildAccessLogger(k kernel.Kernel) log.Logger {
	var lo log.Options
	if err := k.Config().Section("log", &lo); err != nil {
		c.logger.Warn("web: log section unreadable; access log falls back to the root logger", "error", err)
		return c.logger
	}
	if !lo.AccessEnabled {
		return nil
	}
	if len(lo.AccessFiles) == 0 {
		return c.logger
	}
	dedicated := log.New(log.Options{
		Level:  lo.Level,
		Format: lo.Format,
		Output: nil,
		Files:  lo.AccessFiles,
	})
	if closer, ok := dedicated.(io.Closer); ok {
		c.accessCloser = closer
	}
	return dedicated
}

// Close releases the dedicated access logger (when one was built).
// The server itself has already stopped: the draining phase waits for
// Serve to return before any Close runs.
func (c *Component) Close(ctx context.Context) error {
	if c.accessCloser != nil {
		err := c.accessCloser.Close()
		c.accessCloser = nil
		return err
	}
	return nil
}

// Serve implements kernel.Server: bind, signal readiness, serve until
// ctx cancels, then drain gracefully within shutdown_timeout and
// force-Close whatever outlives it — hung handlers must not outlive
// registry teardown (v1 Stop contract). Serve returns only after the
// wind-down completes, so every dependency is still alive while
// in-flight requests finish (SPEC §3.3 draining).
func (c *Component) Serve(ctx context.Context, ready func()) error {
	ln, err := net.Listen("tcp", c.opts.Addr)
	if err != nil {
		return fmt.Errorf("web: listen %s: %w", c.opts.Addr, err)
	}
	if ctx.Err() != nil { // stopped between start and listen
		_ = ln.Close()
		return ctx.Err()
	}
	c.boundAddr.Store(ln.Addr().String())

	h := c.handler
	if c.opts.H2C {
		h = h2c.NewHandler(h, &http2.Server{})
	}
	srv := &http.Server{
		Handler:           h,
		ReadTimeout:       c.opts.ReadTimeout,
		WriteTimeout:      c.opts.WriteTimeout,
		ReadHeaderTimeout: c.opts.ReadHeaderTimeout,
		IdleTimeout:       c.opts.IdleTimeout,
	}

	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.opts.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shCtx); err != nil {
			// Shutdown gave up (deadline) but does NOT force-close
			// hijacked or long-running connections — they would keep
			// using DB/cache that teardown is about to rip out. Close
			// drops them for real.
			closeErr := srv.Close()
			c.logger.Warn("web: graceful shutdown incomplete, connections force-closed",
				"error", err)
			done <- errors.Join(err, closeErr)
			return
		}
		done <- nil
	}()

	c.logger.Info("web: listening", "addr", ln.Addr().String(), "h2c", c.opts.H2C)
	ready()

	serveErr := srv.Serve(ln)
	if !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("web: serve: %w", serveErr)
	}
	// Serve returns the moment Shutdown begins; wait for the wind-down
	// so "Serve returned" means "in-flight work finished".
	return <-done
}
