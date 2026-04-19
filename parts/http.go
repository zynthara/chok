package parts

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/middleware"
	"github.com/zynthara/chok/server"
)

// HTTPResolver extracts HTTPOptions from the app config. Returning nil
// disables the component — Server() returns nil and Init is a no-op.
type HTTPResolver func(appConfig any) *config.HTTPOptions

// HTTPComponent owns the application's HTTP server (gin-backed). It
// builds the gin.Engine and applies default middleware during Init, but
// does NOT bind the port — the App extracts Server() after
// registry.Start and runs it through the normal Server lifecycle.
//
// This design keeps HTTPComponent as a standard Component (short Init,
// no blocking) while letting Router components declare
// Dependencies: ["http"] to reach the Engine.
type HTTPComponent struct {
	resolve     HTTPResolver
	extraMw     []gin.HandlerFunc
	srv         *server.HTTPServer
	accessLog   bool
	httpMetrics bool // enable RED metrics middleware
}

// NewHTTPComponent constructs the component. The resolver is called
// during Init to obtain HTTPOptions from the app config.
func NewHTTPComponent(resolve HTTPResolver) *HTTPComponent {
	return &HTTPComponent{resolve: resolve, accessLog: true, httpMetrics: true}
}

// Use appends middleware applied after the default stack (Recovery,
// RequestID, Logger). Call before Init (i.e. at registration time).
func (h *HTTPComponent) Use(mw ...gin.HandlerFunc) *HTTPComponent {
	h.extraMw = append(h.extraMw, mw...)
	return h
}

// WithoutAccessLog disables the automatic access-log middleware.
func (h *HTTPComponent) WithoutAccessLog() *HTTPComponent {
	h.accessLog = false
	return h
}

// WithoutMetrics disables the automatic HTTP RED metrics middleware.
func (h *HTTPComponent) WithoutMetrics() *HTTPComponent {
	h.httpMetrics = false
	return h
}

// Name implements component.Component.
func (h *HTTPComponent) Name() string { return "http" }

// ConfigKey implements component.Component.
func (h *HTTPComponent) ConfigKey() string { return "http" }

// OptionalDependencies declares soft ordering constraints so metrics and
// log components Init before HTTP when they exist. HTTP works without them.
func (h *HTTPComponent) OptionalDependencies() []string {
	return []string{"metrics", "log", "tracing"}
}

// Init builds the gin.Engine and applies the default middleware stack:
// Recovery → RequestID → Logger (→ AccessLog if enabled) → user extras.
// The server is ready for route mounting but not yet listening.
//
// Optional-dependency wiring uses k.Get with nil checks because each
// component listed in OptionalDependencies may have failed Init (and
// been scrubbed from the registry by removeFromStartOrder) or simply
// be absent from the registry. The middleware stack is built so a
// missing tracing / metrics / log component reduces gracefully to the
// next-best behaviour rather than panicking at request time.
//
// When an optional dependency was registered but is unreachable here
// (post Init-failure), we emit a single WARN so operators see the
// degraded wiring instead of debugging silent feature loss.
func (h *HTTPComponent) Init(ctx context.Context, k component.Kernel) error {
	opts := h.resolve(k.ConfigSnapshot())
	if opts == nil {
		return nil // disabled
	}

	h.srv = server.NewHTTPServer(opts)

	mws := []gin.HandlerFunc{
		middleware.Recovery(),
		middleware.RequestID(),
	}
	// Tracing: create a server span for each request when a
	// TracingComponent is registered and active. Placed before Logger
	// so trace_id / span_id are available for log correlation.
	// otel.Tracer returns a delegate that re-resolves the global
	// TracerProvider on each Start call, so the captured tracer
	// transparently follows TracingComponent.Close's swap to noop —
	// no per-request re-fetch needed here.
	if tc := optionalComponent[*TracingComponent](k, "tracing"); tc != nil && tc.Enabled() {
		mws = append(mws, middleware.Tracing(tc.ServiceName()))
	}
	mws = append(mws, middleware.Logger(k.Logger()))
	// Request timeout: inject after logger so the per-request logger is
	// available when the timeout fires and a 504 is written.
	if opts.RequestTimeout > 0 {
		mws = append(mws, middleware.Timeout(opts.RequestTimeout))
	}
	// HTTP RED metrics: inject early so all requests (including
	// errored ones) are counted. Uses MetricsComponent's registry
	// when available, otherwise creates a standalone registry.
	if h.httpMetrics {
		if mc := optionalComponent[*MetricsComponent](k, "metrics"); mc != nil {
			mws = append(mws, middleware.Metrics(mc.PrometheusRegistry()))
		}
	}
	if h.accessLog {
		// Access logger: use the LoggerComponent's access logger when
		// available, otherwise fall back to the main logger. Captured
		// by reference; LoggerComponent.Reload only mutates level
		// (documented restart-required for routing changes), so the
		// reference stays valid for the App's lifetime.
		accessLogger := k.Logger()
		if lc := optionalComponent[*LoggerComponent](k, "log"); lc != nil {
			if al := lc.AccessLogger(); al != nil {
				accessLogger = al
			}
		}
		mws = append(mws, middleware.AccessLog(accessLogger))
	}
	mws = append(mws, h.extraMw...)
	h.srv.Use(mws...)

	return nil
}

// optionalComponent fetches a component by name and asserts it to T.
// Returns the typed pointer or nil for any failure mode (component not
// registered, optional Init failed, or registered as a different
// concrete type). Centralising this lets HTTPComponent stay readable
// while still tolerating missing soft dependencies.
func optionalComponent[T any](k component.Kernel, name string) T {
	var zero T
	c := k.Get(name)
	if c == nil {
		return zero
	}
	t, ok := c.(T)
	if !ok {
		return zero
	}
	return t
}

// Close is a no-op — the server's Stop is managed by App.runServers.
func (h *HTTPComponent) Close(ctx context.Context) error { return nil }

// Health reports whether the server was successfully built. A nil
// server means HTTP was disabled via config (not a failure).
func (h *HTTPComponent) Health(ctx context.Context) component.HealthStatus {
	if h.srv == nil {
		return component.HealthStatus{Status: component.HealthOK}
	}
	return component.HealthStatus{Status: component.HealthOK}
}

// Server returns the underlying *server.HTTPServer. nil when disabled
// or before Init. The App should call this after registry.Start to
// extract the server and add it to the server lifecycle.
func (h *HTTPComponent) Server() *server.HTTPServer { return h.srv }

// Engine returns the gin.Engine. nil when disabled or before Init.
func (h *HTTPComponent) Engine() *gin.Engine {
	if h.srv == nil {
		return nil
	}
	return h.srv.Engine()
}
