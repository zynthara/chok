package chok

// autoregister.go discovers known config.Options types in the user's
// config struct and auto-registers the corresponding Components when
// the user hasn't registered them explicitly. It also wires the
// internal EventAfterStart hook that orchestrates Router mounting.
//
// Discovery uses the same discover[T]() mechanism that initLogger and
// initCache already rely on, extended to HTTP, DB, Redis, Account,
// Swagger, Health, and Metrics.

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/cache"
	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/middleware"
	"github.com/zynthara/chok/parts"
	"github.com/zynthara/chok/server"
)

// ---------------------------------------------------------------------------
// Auto-registration
// ---------------------------------------------------------------------------

// autoRegisterComponents is called after setupFn. For each known Options
// type found in configPtr, it registers the matching Component — unless
// the user already registered one with the same Name() in setupFn.
//
// Returns an error on config ambiguity or conflict (fail-fast). A config
// error here means the user's config struct is misconfigured and the app
// should not start silently with missing subsystems.
func (a *App) autoRegisterComponents() error {
	if err := a.autoRegisterCache(); err != nil {
		return err
	}
	if a.configPtr == nil {
		return nil
	}
	for _, fn := range []func() error{
		a.autoRegisterHTTP,
		a.autoRegisterDB,
		a.autoRegisterRedis,
		a.autoRegisterAccount,
		a.autoRegisterSwagger,
		a.autoRegisterHealth,
		a.autoRegisterMetrics,
		a.autoRegisterDebug,
	} {
		if err := fn(); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) hasComponent(name string) bool {
	a.registryMu.RLock()
	defer a.registryMu.RUnlock()
	for _, c := range a.pendingComponents {
		if c.Name() == name {
			return true
		}
	}
	return false
}

// autoRegisterHTTP creates an HTTPComponent when config.HTTPOptions is
// found and no "http" Component (or manual Server) has been added yet.
func (a *App) autoRegisterHTTP() error {
	if a.hasComponent("http") || len(a.servers) > 0 {
		return nil
	}
	opts, err := discoverOne[config.HTTPOptions](a.configPtr)
	if err != nil {
		return fmt.Errorf("auto-register http: %w", err)
	}
	comp := parts.NewDefaultHTTPComponent(opts)
	if comp != nil {
		// Honour `log.access_enabled: false` in the auto-register path.
		// Without this, a fronting proxy that already records access
		// logs would still see chok's middleware double-log every
		// request — silently, because the user did set the config field
		// and reasonably expected it to take effect.
		if !a.AccessLogEnabled() {
			comp.WithoutAccessLog()
		}
		a.Register(comp)
		// Inherit drain delay from HTTP config when the user hasn't set
		// WithDrainDelay explicitly (drainDelay is still zero).
		if a.drainDelay == 0 && opts.DrainDelay > 0 {
			a.drainDelay = opts.DrainDelay
		}
	}
	return nil
}

func (a *App) autoRegisterDB() error {
	if a.hasComponent("db") {
		return nil
	}

	// Prefer the discriminator-based DatabaseOptions (driver: sqlite|mysql)
	// when present. Fall back to the legacy separate MySQLOptions /
	// SQLiteOptions fields for backward compatibility.
	dbOpts, err := discoverOne[config.DatabaseOptions](a.configPtr)
	if err != nil {
		return fmt.Errorf("auto-register db: %w", err)
	}
	if dbOpts != nil && dbOpts.Driver != "" {
		switch dbOpts.Driver {
		case "sqlite":
			a.Register(parts.NewDBComponent(parts.SQLiteBuilder(&dbOpts.SQLite), a.tables...))
		case "mysql":
			a.Register(parts.NewDBComponent(parts.MySQLBuilder(&dbOpts.MySQL), a.tables...))
		default:
			return fmt.Errorf("auto-register db: unsupported driver %q", dbOpts.Driver)
		}
		return nil
	}

	// Legacy path: separate MySQLOptions / SQLiteOptions fields.
	sqlite, err := discoverOne[config.SQLiteOptions](a.configPtr)
	if err != nil {
		return fmt.Errorf("auto-register db: %w", err)
	}
	mysql, err := discoverOne[config.MySQLOptions](a.configPtr)
	if err != nil {
		return fmt.Errorf("auto-register db: %w", err)
	}
	if sqlite != nil && sqlite.Enabled && mysql != nil && mysql.Enabled {
		return fmt.Errorf("auto-register db: both sqlite and mysql are enabled; disable one with enabled: false, or use database.driver discriminator")
	}
	comp := parts.NewDefaultDBComponent(sqlite, mysql, a.tables)
	if comp != nil {
		a.Register(comp)
	}
	return nil
}

func (a *App) autoRegisterRedis() error {
	if a.hasComponent("redis") {
		return nil
	}
	opts, err := discoverOne[config.RedisOptions](a.configPtr)
	if err != nil {
		return fmt.Errorf("auto-register redis: %w", err)
	}
	comp := parts.NewDefaultRedisComponent(opts)
	if comp != nil {
		a.Register(comp)
	}
	return nil
}

func (a *App) autoRegisterAccount() error {
	if a.hasComponent("account") {
		return nil
	}
	opts, err := discoverOne[config.AccountOptions](a.configPtr)
	if err != nil {
		return fmt.Errorf("auto-register account: %w", err)
	}
	comp := parts.NewDefaultAccountComponent(opts)
	if comp != nil {
		a.Register(comp)
	}
	return nil
}

func (a *App) autoRegisterSwagger() error {
	if a.hasComponent("swagger") {
		return nil
	}
	opts, err := discoverOne[config.SwaggerOptions](a.configPtr)
	if err != nil {
		return fmt.Errorf("auto-register swagger: %w", err)
	}
	comp := parts.NewDefaultSwaggerComponent(opts)
	if comp != nil {
		a.Register(comp)
	}
	return nil
}

func (a *App) autoRegisterCache() error {
	if a.hasComponent("cache") {
		return nil
	}
	// User explicitly set a cache via SetCacher — wrap it in a
	// CacheComponent without the auto-redis optional dependency
	// (the pre-built cache already has whatever backends it needs).
	if a.cacher != nil {
		builderUnused := func(component.Kernel) (cache.Cache, error) { return nil, nil }
		a.Register(parts.NewCacheComponent(builderUnused).
			WithoutOptionalDependencies().
			WithPreBuilt(a.cacher, true))
		return nil
	}
	if a.configPtr == nil {
		return nil
	}
	// Auto-discover cache config. DefaultCacheBuilder integrates Redis
	// when a RedisComponent is available (via CacheComponent's optional
	// dependency on "redis"), producing the full memory → file → Redis
	// chain. This is the recommended path for all auto-registered caches.
	memOpts := a.cacheMemOpts
	fileOpts := a.cacheFileOpts
	if memOpts == nil {
		var err error
		memOpts, err = discoverOne[config.CacheMemoryOptions](a.configPtr)
		if err != nil {
			return fmt.Errorf("auto-register cache: %w", err)
		}
	}
	if fileOpts == nil {
		var err error
		fileOpts, err = discoverOne[config.CacheFileOptions](a.configPtr)
		if err != nil {
			return fmt.Errorf("auto-register cache: %w", err)
		}
	}
	if (memOpts != nil && memOpts.Enabled) || (fileOpts != nil && fileOpts.Enabled) {
		a.Register(parts.NewCacheComponent(parts.DefaultCacheBuilder(memOpts, fileOpts)))
	}
	return nil
}

func (a *App) autoRegisterHealth() error {
	if a.hasComponent("health") || !a.hasHTTP() {
		return nil
	}
	// Default: auto-register when HTTP exists.
	// HealthOptions overrides path and can disable.
	path := "/healthz"
	if a.configPtr != nil {
		opts, err := discoverOne[config.HealthOptions](a.configPtr)
		if err != nil {
			return fmt.Errorf("auto-register health: %w", err)
		}
		if opts != nil {
			if !opts.Enabled {
				return nil
			}
			if opts.Path != "" {
				path = opts.Path
			}
		}
	}
	a.Register(parts.NewHealthComponent(path))
	return nil
}

func (a *App) autoRegisterMetrics() error {
	if a.hasComponent("metrics") || !a.hasHTTP() {
		return nil
	}
	path := "/metrics"
	if a.configPtr != nil {
		opts, err := discoverOne[config.MetricsOptions](a.configPtr)
		if err != nil {
			return fmt.Errorf("auto-register metrics: %w", err)
		}
		if opts != nil {
			if !opts.Enabled {
				return nil
			}
			if opts.Path != "" {
				path = opts.Path
			}
		}
	}
	a.Register(parts.NewMetricsComponent(path))
	return nil
}

func (a *App) autoRegisterDebug() error {
	if a.hasComponent("debug") || !a.hasHTTP() {
		return nil
	}
	if a.configPtr != nil {
		opts, err := discoverOne[config.DebugOptions](a.configPtr)
		if err != nil {
			return fmt.Errorf("auto-register debug: %w", err)
		}
		if opts != nil && !opts.Enabled {
			return nil
		}
		if opts == nil {
			return nil // no DebugOptions in config, disabled by default
		}
	}
	a.Register(parts.NewDebugComponent(""))
	return nil
}

// hasHTTP returns true when an HTTP capability exists — either via
// HTTPComponent or a manually added Server.
func (a *App) hasHTTP() bool {
	return a.hasComponent("http") || len(a.servers) > 0
}

// ---------------------------------------------------------------------------
// HTTP server extraction
// ---------------------------------------------------------------------------

// extractHTTPServer takes the *server.HTTPServer from the HTTPComponent
// (after Init) and adds it to App.servers so runServers manages its
// listen/shutdown lifecycle. No-op when HTTPComponent is absent or
// disabled (nil Server).
func (a *App) extractHTTPServer() {
	if a.registry == nil {
		return
	}
	hc, ok := a.registry.Get("http").(*parts.HTTPComponent)
	if !ok || hc == nil {
		return
	}
	srv := hc.Server()
	if srv == nil {
		return // disabled
	}
	a.AddServer(srv)
}

// ---------------------------------------------------------------------------
// Internal route mounting
// ---------------------------------------------------------------------------

// internalMountHook returns an EventAfterStart hook that orchestrates
// Router mounting in three phases:
//  1. Mount all Router Components except swagger (auto-discovered)
//  2. Call routesFn (user business routes)
//  3. Mount swagger last (so Populate sees all routes)
func (a *App) internalMountHook() component.Hook {
	return func(ctx context.Context) error {
		engine := a.engine()
		if engine == nil {
			// Surface a clear signal when the app registered Router
			// components (swagger / account / health / metrics / debug
			// / user-defined) but forgot to configure an HTTP server —
			// otherwise those routes would silently be unreachable and
			// the only hint would be 404s from the client side.
			for _, c := range a.registry.StartedComponents() {
				if _, ok := c.(interface{ Mount(any) error }); ok {
					a.logger.Warn(
						"HTTP server not configured; Router components will not be mounted",
						"component", c.Name(),
					)
				}
			}
			return nil
		}

		// Inject per-App error mapper registry into every request context.
		// Handlers use apierr.ResolveWithContext to check these before the
		// global mappers, enabling test isolation and multi-App scenarios.
		if a.errorMappers != nil {
			engine.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(
					apierr.WithMapperRegistry(c.Request.Context(), a.errorMappers))
				c.Next()
			})
		}

		// Phase 1: mount every Router component except swagger.
		// Uses StartedComponents (topo start order) so that:
		//   - optional components that failed Init are skipped
		//   - mount order respects the dependency graph
		for _, c := range a.registry.StartedComponents() {
			if c.Name() == "swagger" || c.Name() == "http" {
				continue // swagger is mounted last; http is the server itself
			}
			r, ok := c.(interface{ Mount(any) error })
			if !ok {
				continue
			}
			if err := r.Mount(engine); err != nil {
				return fmt.Errorf("mount %s: %w", c.Name(), err)
			}
		}

		// Phase 2: user business routes.
		if a.routesFn != nil {
			if err := a.routesFn(ctx, a); err != nil {
				return fmt.Errorf("routes callback: %w", err)
			}
		}

		// Phase 3: swagger last.
		if sc := a.registry.Get("swagger"); sc != nil {
			if r, ok := sc.(interface{ Mount(any) error }); ok {
				if err := r.Mount(engine); err != nil {
					return fmt.Errorf("mount swagger: %w", err)
				}
			}
		}
		return nil
	}
}

// ---------------------------------------------------------------------------
// Convenience accessors (available inside WithRoutes callback)
// ---------------------------------------------------------------------------

// engine returns the gin.Engine from the HTTPComponent in the registry,
// or by scanning App.servers for a manually added HTTPServer (backward
// compat). Returns nil if no HTTP capability exists.
func (a *App) engine() *gin.Engine {
	// Prefer HTTPComponent in registry.
	if a.registry != nil {
		if hc, ok := a.registry.Get("http").(*parts.HTTPComponent); ok && hc != nil {
			return hc.Engine()
		}
	}
	// Fallback: scan servers for a manually added *server.HTTPServer.
	for _, s := range a.servers {
		if hs, ok := s.(*server.HTTPServer); ok {
			return hs.Engine()
		}
	}
	return nil
}

// API returns a gin.RouterGroup for the given path prefix, ready for
// registering business routes. Panics if no HTTP server is available.
//
// Typical usage inside WithRoutes:
//
//	api := a.API("/api/v1")
//	handler.RegisterPostRoutes(api, store)
func (a *App) API(relativePath string, middlewares ...gin.HandlerFunc) *gin.RouterGroup {
	e := a.engine()
	if e == nil {
		panic("chok: API() called but no HTTP server registered")
	}
	return e.Group(relativePath, middlewares...)
}

// AuthMiddleware returns a gin middleware that authenticates requests
// via the AccountComponent's JWT token parser. Returns a no-op
// middleware if no AccountComponent is registered or if it's disabled.
//
// Typical usage:
//
//	api := a.API("/api/v1", a.AuthMiddleware())
func (a *App) AuthMiddleware() gin.HandlerFunc {
	if a.registry == nil {
		return func(c *gin.Context) { c.Next() }
	}
	acct, ok := a.registry.Get("account").(*parts.AccountComponent)
	if !ok || acct == nil || acct.Module() == nil {
		return func(c *gin.Context) { c.Next() }
	}
	mod := acct.Module()
	return middleware.Authn(mod.TokenParser(), mod.PrincipalResolver())
}
