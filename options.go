package chok

import (
	"context"
	"time"

	"github.com/spf13/pflag"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/version"
)

// Option configures an App.
type Option func(*App)

func WithVersion(v version.Info) Option {
	return func(a *App) { a.version = v }
}

// WithConfig registers a typed config pointer and optional explicit path.
// The pointer is populated during Run() after config loading.
func WithConfig(cfg any, path ...string) Option {
	return func(a *App) {
		a.configPtr = cfg
		if len(path) > 0 && path[0] != "" {
			a.configPath = path[0]
			a.configExplicit = true
		}
	}
}

func WithEnvPrefix(prefix string) Option {
	return func(a *App) { a.envPrefix = prefix }
}

// WithLogConfig points to the SlogOptions inside the typed config.
// The pointer is dereferenced after config loading, before Setup.
func WithLogConfig(opts *config.SlogOptions) Option {
	return func(a *App) { a.logOpts = opts }
}

// WithCacheConfig points to cache config options inside the typed config.
// After config loading, the framework auto-builds the cache from enabled layers.
// SetCacher() in Setup overrides the auto-built cache.
// Pointers are dereferenced after config loading (same timing as WithLogConfig).
func WithCacheConfig(memory *config.CacheMemoryOptions, file *config.CacheFileOptions) Option {
	return func(a *App) {
		a.cacheMemOpts = memory
		a.cacheFileOpts = file
	}
}

// WithLogger injects a Logger directly (highest priority).
func WithLogger(l log.Logger) Option {
	return func(a *App) { a.logger = l }
}

func WithSetup(f func(context.Context, *App) error) Option {
	return func(a *App) { a.setupFn = f }
}

// WithCleanup registers a cleanup callback, called LIFO when Run ends.
func WithCleanup(f func(context.Context) error) Option {
	return func(a *App) { a.cleanupFns = append(a.cleanupFns, f) }
}

func WithShutdownTimeout(d time.Duration) Option {
	return func(a *App) { a.shutdownTimeout = d }
}

// WithDrainDelay sets a pause between marking readyz as 503 and
// actually stopping HTTP servers. In Kubernetes deployments this gives
// the load balancer time to deregister the pod from endpoints before
// in-flight requests are drained. Zero (the default) means no delay.
//
// The delay is bounded by the overall shutdown timeout — if the remaining
// budget is less than the configured drain delay, the shorter value wins.
func WithDrainDelay(d time.Duration) Option {
	return func(a *App) { a.drainDelay = d }
}

// WithInitTimeout sets the default per-component startup-phase timeout,
// covering both Init and Migrate. If a component implements
// InitTimeouter, its value takes precedence. Default is 30s. Zero
// disables the timeout. For long-running schema migrations, increase
// this value or implement InitTimeouter on the DBComponent.
func WithInitTimeout(d time.Duration) Option {
	return func(a *App) { a.initTimeout = d }
}

// WithCloseTimeout sets the default per-component Close timeout. If a
// component implements CloseTimeouter, its value takes precedence.
// Default is 15s. Zero disables the timeout.
func WithCloseTimeout(d time.Duration) Option {
	return func(a *App) { a.closeTimeout = d }
}

// WithHealthTimeout sets the per-probe Health timeout. Each Healther
// probe runs concurrently and is given at most this duration. A hard
// fan-in deadline (timeout + 1s) ensures rogue probes that ignore their
// context cannot block the entire /healthz endpoint.
// Default is 3s. Zero disables the timeout.
func WithHealthTimeout(d time.Duration) Option {
	return func(a *App) { a.healthTimeout = d }
}

// WithHookTimeout sets the aggregate timeout for lifecycle hook
// execution. When set, EventBeforeStop and EventBeforeReload hooks
// are bounded by this duration — a slow hook cannot block the shutdown
// sequence past the Kubernetes terminationGracePeriod. Default is 10s.
// Zero disables the timeout.
func WithHookTimeout(d time.Duration) Option {
	return func(a *App) { a.hookTimeout = d }
}

// WithComponentReloadTimeout sets the default per-component Reload timeout.
// If a component implements ReloadTimeouter, its value takes precedence.
// Default is 10s. Zero disables the timeout.
func WithComponentReloadTimeout(d time.Duration) Option {
	return func(a *App) { a.componentReloadTimeout = d }
}

func WithReloadFunc(f func(context.Context) error) Option {
	return func(a *App) { a.reloadFn = f }
}

func WithReloadTimeout(d time.Duration) Option {
	return func(a *App) { a.reloadTimeout = d }
}

// WithTables declares database table specs (model + indexes) for the
// auto-registered DBComponent. When the config struct contains a
// SQLiteOptions or MySQLOptions field and no "db" Component is
// registered explicitly, the framework auto-registers a DBComponent
// that migrates these tables at startup.
//
//	chok.WithTables(
//	    db.Table(&Post{}, db.SoftUnique("uk_slug", "slug")),
//	)
func WithTables(tables ...db.TableSpec) Option {
	return func(a *App) { a.tables = append(a.tables, tables...) }
}

// WithRoutes registers a callback that runs inside an EventAfterStart
// hook — after every Component has Init'd and all non-swagger Router
// Components have been mounted. Use it to wire business routes without
// manual hook orchestration:
//
//	chok.WithRoutes(func(ctx context.Context, a *chok.App) error {
//	    api := a.API("/api/v1")
//	    handler.RegisterPostRoutes(api, postStore)
//	    return nil
//	})
//
// The framework mounts swagger AFTER this callback returns, so
// Populate() sees every route registered here.
func WithRoutes(f func(context.Context, *App) error) Option {
	return func(a *App) { a.routesFn = f }
}

// WithFlags registers a parsed pflag.FlagSet.
// CLI flags take highest priority: flags > env > file > default tag.
// pflag is already an indirect dependency via Viper.
func WithFlags(fs *pflag.FlagSet) Option {
	return func(a *App) { a.flagSet = fs }
}

// WithErrorMapper registers an error mapper scoped to this App instance.
// Unlike the global apierr.RegisterMapper, scoped mappers are isolated
// per App — safe for parallel tests and multi-App scenarios.
//
// Scoped mappers are checked before global mappers when resolving errors
// in HTTP handlers. The mapper is injected into request context via
// middleware, so all handlers mounted on this App benefit automatically.
//
//	chok.New("blog",
//	    chok.WithErrorMapper(store.MapError),
//	)
func WithErrorMapper(m apierr.ErrorMapper) Option {
	return func(a *App) {
		if a.errorMappers == nil {
			a.errorMappers = apierr.NewMapperRegistry()
		}
		a.errorMappers.Register(m)
	}
}

// RunOption configures a Run() call.
type RunOption func(*runConfig)

type runConfig struct {
	signals     bool
	watchConfig bool
}

// WithSignals enables OS signal handling (SIGTERM/SIGINT/SIGHUP/SIGQUIT).
func WithSignals() RunOption {
	return func(rc *runConfig) { rc.signals = true }
}

// WithConfigWatch enables fsnotify-based watching of the config file.
// When the file is written (or replaced by an atomic-save editor),
// App.Reload is invoked with the same dispatch path as SIGHUP — the
// config re-reads, every Reloadable Component gets its Reload call, and
// the optional WithReloadFunc runs last.
//
// Requires WithConfig to have registered a non-empty path; if the app
// was started without an explicit config file this option becomes a
// no-op (logged at Warn at startup).
//
// Events are debounced by 100ms so a single save that triggers multiple
// Write syscalls produces exactly one Reload.
func WithConfigWatch() RunOption {
	return func(rc *runConfig) { rc.watchConfig = true }
}
