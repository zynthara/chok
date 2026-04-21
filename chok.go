package chok

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/cache"
	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/parts"
	"github.com/zynthara/chok/version"
)

// Server is the interface all managed servers must implement.
//
// Start(ctx, ready) blocks until Stop is called.
// ready() must be called exactly once after the server is ready to serve.
// Stop(ctx) is the sole shutdown trigger; it must be idempotent and safe
// to call at any point, including before Start has been called.
type Server interface {
	Start(ctx context.Context, ready func()) error
	Stop(ctx context.Context) error
}

// ServerFunc adapts a simple function into a Server.
// Stop cancels the internal ctx, so f exits via <-ctx.Done().
func ServerFunc(f func(ctx context.Context, ready func()) error) Server {
	return &serverFunc{f: f}
}

type serverFunc struct {
	f       func(ctx context.Context, ready func()) error
	cancel  context.CancelFunc
	mu      sync.Mutex
	stopped bool
}

func (sf *serverFunc) Start(ctx context.Context, ready func()) error {
	sf.mu.Lock()
	if sf.stopped {
		sf.mu.Unlock()
		return errors.New("server: stopped before start")
	}
	// Internal context: detached from parent, cancelled only by Stop.
	// Values (request_id, logger, etc.) are preserved via WithoutCancel.
	var sctx context.Context
	sctx, sf.cancel = context.WithCancel(context.WithoutCancel(ctx))
	sf.mu.Unlock()

	// Pre-ready: parent cancel must propagate (design.md contract).
	// Post-ready: only Stop may cancel (sole shutdown trigger).
	// A bridge goroutine forwards parent cancel until ready() disconnects it.
	merged, mergedCancel := context.WithCancel(sctx)
	defer mergedCancel()

	var detachOnce sync.Once
	detached := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			mergedCancel()
		case <-detached:
		case <-merged.Done():
		}
	}()

	return sf.f(merged, func() {
		detachOnce.Do(func() { close(detached) })
		ready()
	})
}

func (sf *serverFunc) Stop(_ context.Context) error {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	sf.stopped = true
	if sf.cancel != nil {
		sf.cancel()
	}
	return nil
}

// ErrServerUnexpectedExit is returned when a server's Start returns without
// being triggered by Stop.
var ErrServerUnexpectedExit = errors.New("chok: server exited unexpectedly without being stopped")

// App is the application instance.
//
// Thread-safety: fields written during setup (before Run's registry
// publish) and read during request handling (cacher, gormDB, registry,
// logger) must be accessed under registryMu. shutdownDeadline is written
// during shutdown and read both from shutdown paths and from cleanup —
// atomic.Int64 (unix nanos) keeps reads lock-free.
type App struct {
	name                   string
	servers                []Server
	logger                 log.Logger
	accessLogger           log.Logger
	cacher                 cache.Cache // writes/reads via registryMu
	gormDB                 any         // *gorm.DB — writes/reads via registryMu
	logOpts                *config.SlogOptions
	cacheMemOpts           *config.CacheMemoryOptions
	cacheFileOpts          *config.CacheFileOptions
	setupFn                func(context.Context, *App) error
	routesFn               func(context.Context, *App) error
	cleanupFns             []func(context.Context) error
	reloadFn               func(context.Context) error
	tables                 []db.TableSpec
	configPtr              any
	configPath             string
	configExplicit         bool
	flagSet                any // *pflag.FlagSet — stored as any to avoid importing pflag in chok.go
	version                version.Info
	envPrefix              string
	shutdownTimeout        time.Duration
	reloadTimeout          time.Duration
	drainDelay             time.Duration          // pause between readyz→503 and server stop
	shutdownNanos          atomic.Int64           // unix nanos of the shutdown deadline; 0 = unset
	initTimeout            time.Duration          // per-component Init timeout
	closeTimeout           time.Duration          // per-component Close timeout
	healthTimeout          time.Duration          // per-probe Health timeout
	hookTimeout            time.Duration          // aggregate timeout for lifecycle hooks
	componentReloadTimeout time.Duration          // per-component Reload timeout
	errorMappers           *apierr.MapperRegistry // per-App error mappers (nil = global only)

	// registry drives the lifecycle of user-registered Components (phase
	// 3 of the redesign). Lazily constructed inside Run once every
	// Register call has happened — nil when the user opts out of
	// Components entirely, so apps that don't use them pay no cost.
	// Access via Registry() which is protected by registryMu for
	// concurrent reads (Run writes once, handlers read concurrently).
	registry          *component.Registry
	registryMu        sync.RWMutex // protects registry pointer for concurrent access
	pendingComponents []component.Component
	pendingHooks      map[component.Event][]component.Hook

	once     sync.Once    // ensures Run/Execute called at most once
	configMu sync.RWMutex // serializes config writes during reload; readers rely on ConfigSnapshot() for multi-field consistency
	reloadMu sync.Mutex   // serializes Reload calls from concurrent triggers (fsnotify, SIGHUP, admin API)
}

// derivEnvPrefix converts an app name to a valid env var prefix by
// uppercasing and replacing every non-alphanumeric character with "_".
// viper binds env vars by exact name match; characters like ".", "/",
// or whitespace in the prefix silently break env overrides, so
// normalising here keeps `WithEnvPrefix` an escape hatch for
// deliberately different naming.
func derivEnvPrefix(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// New creates an App (pure construction, zero side effects).
func New(name string, opts ...Option) *App {
	a := &App{
		name:                   name,
		envPrefix:              derivEnvPrefix(name),
		shutdownTimeout:        30 * time.Second,
		reloadTimeout:          10 * time.Second,
		initTimeout:            30 * time.Second,
		closeTimeout:           15 * time.Second,
		healthTimeout:          3 * time.Second,
		hookTimeout:            10 * time.Second,
		componentReloadTimeout: 10 * time.Second,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// AddServer registers a Server to be managed by Run.
func (a *App) AddServer(srv Server) {
	a.servers = append(a.servers, srv)
}

// AddCleanup registers a cleanup callback (LIFO order on shutdown).
// Cleanups run after registry.Stop — use AddCleanup for resources owned
// by user code outside the Component abstraction (e.g. ad-hoc goroutines).
// Components should release their own resources via Component.Close.
//
// The callback's ctx carries the original Run-time values (trace_id,
// request_id, logger) and a deadline derived from the shutdown budget.
// Its cancellation is intentionally detached from the parent ctx so
// cleanup keeps making progress even after a shutdown signal cancels
// the upstream context.
//
// The component registry is already torn down when cleanups run —
// App.Cacher(), App.DB(), and Registry().Get() all return nil or
// already-closed instances at this point. Cleanups that need access
// to a long-lived resource should capture it explicitly during
// Setup rather than re-fetch it on shutdown.
func (a *App) AddCleanup(f func(context.Context) error) {
	a.cleanupFns = append(a.cleanupFns, f)
}

// Register adds a Component that will be managed by the App's internal
// component.Registry. Components are Init'd (in topological order by
// declared Dependencies) after the Setup callback runs and before
// servers start. On shutdown, they're Close'd in reverse order, ahead
// of AddCleanup callbacks.
//
// Must be called before Run/Execute. Calling after the registry has
// been constructed panics — a misconfigured registration is a bug
// worth catching at the earliest point.
func (a *App) Register(c component.Component) {
	a.registryMu.Lock()
	defer a.registryMu.Unlock()
	if a.registry != nil {
		panic(fmt.Sprintf("chok: App.Register(%q) called after registry started", c.Name()))
	}
	a.pendingComponents = append(a.pendingComponents, c)
}

// Replace registers a Component that explicitly replaces any previously
// registered Component with the same Name. Unlike Register (which panics
// on duplicates at Registry level), Replace is the intended mechanism for
// overriding auto-registered or framework-provided Components with custom
// implementations.
//
// Must be called before Run/Execute. Calling after the registry has
// been constructed panics.
func (a *App) Replace(c component.Component) {
	a.registryMu.Lock()
	defer a.registryMu.Unlock()
	if a.registry != nil {
		panic(fmt.Sprintf("chok: App.Replace(%q) called after registry started", c.Name()))
	}
	for i, existing := range a.pendingComponents {
		if existing.Name() == c.Name() {
			// Zero the removed element so the removed Component is not
			// retained by the backing array (would leak resources if the
			// Component holds large references).
			a.pendingComponents[i] = nil
			a.pendingComponents = append(a.pendingComponents[:i], a.pendingComponents[i+1:]...)
			break
		}
	}
	a.pendingComponents = append(a.pendingComponents, c)
}

// Registry exposes the App's internal component.Registry. Only valid
// after Run has built the registry (i.e. from inside a component's
// Init, from AddCleanup, or from the user's reload callback). Before
// then it returns nil.
//
// Typical uses: fetching a Component by name during a reload callback
// (e.g. app.Registry().Reload(ctx) from a SIGHUP handler) or inspecting
// the Health report.
func (a *App) Registry() *component.Registry {
	a.registryMu.RLock()
	defer a.registryMu.RUnlock()
	return a.registry
}

// On subscribes hook to the given lifecycle event. Usable before the
// registry exists (e.g. from inside setupFn) — the hook is queued and
// transferred to the registry when it's built.
//
// Classic use: mount Router components on an HTTP engine inside an
// EventAfterStart hook, since by then every Component has Init'd and
// the Router's target resource (gin engine, gRPC server) is safe to
// wire up:
//
//	a.On(component.EventAfterStart, func(ctx context.Context) error {
//	    engine := srv.Engine()
//	    for _, name := range []string{"swagger", "account"} {
//	        if r, ok := a.Registry().Get(name).(component.Router); ok {
//	            if err := r.Mount(engine); err != nil { return err }
//	        }
//	    }
//	    return nil
//	})
func (a *App) On(event component.Event, hook component.Hook) {
	// Hold the lock for the entire check-and-write so we don't miss a
	// hook registered in the window between Run consuming pendingHooks
	// and publishing a.registry (TOCTOU). If registry is already live,
	// delegate to it directly; otherwise append to pendingHooks.
	a.registryMu.Lock()
	if a.registry != nil {
		reg := a.registry
		a.registryMu.Unlock()
		reg.On(event, hook)
		return
	}
	if a.pendingHooks == nil {
		a.pendingHooks = map[component.Event][]component.Hook{}
	}
	a.pendingHooks[event] = append(a.pendingHooks[event], hook)
	a.registryMu.Unlock()
}

// Logger returns the current logger.
func (a *App) Logger() log.Logger {
	if a.logger == nil {
		return log.Empty()
	}
	return a.logger
}

// AccessLogger returns the logger dedicated to access-log entries (HTTP request
// trace). Falls back to the main Logger() when log.access_files is not configured,
// so middleware code can call this unconditionally.
func (a *App) AccessLogger() log.Logger {
	if a.accessLogger != nil {
		return a.accessLogger
	}
	return a.Logger()
}

// ReloadConfig re-reads the config file and env vars using a two-phase
// approach: unmarshal into a fresh copy, validate, then atomically copy
// the result into the live config struct. On any failure the live config
// is left completely untouched — no partial field writes.
// Returns (changed, changedSections, error) where changed indicates if
// the config values actually differ from the previous config, and
// changedSections maps top-level config keys to whether they changed.
func (a *App) ReloadConfig() (bool, map[string]bool, error) {
	if a.configPtr == nil {
		return false, nil, nil
	}
	return a.reloadConfigImmutable()
}

// AccessLogEnabled reports whether the application should mount its own HTTP
// access log middleware. Defaults to true; set log.access_enabled: false to
// suppress (e.g. when a fronting proxy already records access logs).
// When logOpts is nil (no log config wired), defaults to true to match the
// historical behavior of the AccessLog middleware.
func (a *App) AccessLogEnabled() bool {
	if a.logOpts == nil {
		return true
	}
	return a.logOpts.AccessEnabled
}

// ErrorMappers returns the App's scoped error mapper registry. Returns
// nil when no WithErrorMapper options were configured. User code can
// register additional mappers at runtime (thread-safe), though
// registering at construction via WithErrorMapper is preferred.
func (a *App) ErrorMappers() *apierr.MapperRegistry { return a.errorMappers }

// SetCacher registers a Cache on the App. Typically called in Setup to
// override the auto-built cache (initCache runs before setupFn, so
// Cacher() is already populated; SetCacher replaces it).
// autoRegisterCache — which runs after Setup — picks up the final
// value, so the CacheComponent always reflects whatever SetCacher set.
//
// The cache lifecycle is owned by CacheComponent.Close — no separate
// AddCleanup is needed.
//
// Thread-safe: serialized via registryMu so concurrent Cacher() readers
// (e.g. running HTTP handlers) never observe a torn write.
func (a *App) SetCacher(c cache.Cache) {
	a.registryMu.Lock()
	a.cacher = c
	a.registryMu.Unlock()
}

// Cacher returns the registered Cache, or nil if none was set.
func (a *App) Cacher() cache.Cache {
	a.registryMu.RLock()
	defer a.registryMu.RUnlock()
	return a.cacher
}

// SetDB stores a *gorm.DB on the App and registers a Close cleanup.
// Unlike Logger and Cache, DB is not auto-discovered — the user must
// explicitly create the connection in Setup (choose driver, run migrations, etc.).
//
// Thread-safe: serialized via registryMu.
func (a *App) SetDB(gdb any) {
	a.registryMu.Lock()
	a.gormDB = gdb
	a.registryMu.Unlock()
}

// DB returns the application's *gorm.DB. It checks, in order:
//  1. Explicit value set via SetDB in the setup callback.
//  2. The DBComponent in the registry (auto-registered or explicit).
//
// Returns nil if no database is configured. The caller must type-assert:
//
//	gdb := a.DB().(*gorm.DB)
func (a *App) DB() any {
	a.registryMu.RLock()
	gdb := a.gormDB
	reg := a.registry
	a.registryMu.RUnlock()
	if gdb != nil {
		return gdb
	}
	if reg != nil {
		if dbc, ok := reg.Get("db").(*parts.DBComponent); ok && dbc != nil {
			return dbc.DB()
		}
	}
	return nil
}

// Execute is the only method that calls os.Exit.
// It runs with signals enabled.
func (a *App) Execute() {
	if err := a.Run(context.Background(), WithSignals()); err != nil {
		if a.logger != nil {
			a.logger.Error("application error", "error", err)
		} else {
			fmt.Fprintf(os.Stderr, "chok: %v\n", err)
		}
		os.Exit(1)
	}
}

// Run is the pure lifecycle method. It can be called in tests (no os.Exit, no signals by default).
// App is single-use: calling Run or Execute more than once returns an error.
func (a *App) Run(ctx context.Context, opts ...RunOption) error {
	var firstRun bool
	a.once.Do(func() { firstRun = true })
	if !firstRun {
		return errors.New("chok: Run/Execute already called (App is single-use)")
	}

	rc := &runConfig{}
	for _, o := range opts {
		o(rc)
	}

	// 1. Load config.
	if err := a.loadConfig(); err != nil {
		a.runCleanups(ctx)
		return err
	}

	// 1.5. Audit sensitive config fields for placeholder values (after
	// config is loaded but before logger — warnings are deferred).
	sensitiveWarnings := a.auditSensitiveConfig()

	// 2. Initialize logger (WithLogger > WithLogConfig > default).
	// Capture whether the user injected a logger via WithLogger before
	// initLogger runs — if so, its lifecycle belongs to the caller and
	// the framework must not close it behind their back.
	userInjectedLogger := a.logger != nil
	if err := a.initLogger(); err != nil {
		a.runCleanups(ctx)
		return err
	}
	// Register logger file-handle release early so failures between here
	// and registry.Start still flush and close lumberjack backings. The
	// LoggerComponent's own Close is a no-op in WithPreBuilt mode — the
	// App owns the lifecycle for instances it constructed itself.
	if !userInjectedLogger {
		a.AddCleanup(func(_ context.Context) error {
			var firstErr error
			if a.logger != nil {
				if c, ok := a.logger.(io.Closer); ok {
					if err := c.Close(); err != nil {
						firstErr = err
					}
				}
			}
			if a.accessLogger != nil && a.accessLogger != a.logger {
				if c, ok := a.accessLogger.(io.Closer); ok {
					if err := c.Close(); err != nil && firstErr == nil {
						firstErr = err
					}
				}
			}
			return firstErr
		})
	}

	a.logger.Info("starting application",
		"name", a.name,
		"version", a.version.String(),
	)

	// Emit deferred sensitive config warnings now that the logger exists.
	for _, w := range sensitiveWarnings {
		a.logger.Warn("sensitive config field contains a placeholder value; override via environment variable",
			"field", w.Path,
			"env", w.EnvHint,
		)
	}

	// 3. Setup — user code can SetDB, SetCacher, AddServer, AddCleanup,
	// Register Components, or do anything else needed before the
	// registry starts.
	//
	// Note: a.Cacher() returns nil during Setup. The cache is constructed
	// by CacheComponent.Init (which can integrate Redis after it's
	// available). After registry.Start, a.cacher is refreshed from
	// CacheComponent. If Setup needs an early cache, call SetCacher
	// explicitly.
	if a.setupFn != nil {
		if err := a.setupFn(ctx, a); err != nil {
			a.logger.Error("setup failed", "error", err)
			a.runCleanups(ctx)
			return err
		}
	}

	// 3.1. Auto-register Components from config discovery. Only
	// registers components the user hasn't already registered in
	// setupFn. Config ambiguity or conflict is a fatal error — the
	// app must not start silently with missing subsystems.
	if err := a.autoRegisterComponents(); err != nil {
		a.logger.Error("auto-register failed", "error", err)
		a.runCleanups(ctx)
		return err
	}

	// 3.3. Register the internal mount hook that orchestrates Router
	// mounting (non-swagger → user routes → swagger). Fires whenever
	// HTTP capability exists — Router components like Health/Metrics
	// must be mounted even when WithRoutes is not used.
	if a.hasHTTP() {
		a.On(component.EventAfterStart, a.internalMountHook())
		// Warn when HTTP is active but drain delay is zero — Kubernetes
		// deployments should set a small delay so the load balancer can
		// deregister the pod before connections are drained.
		if a.drainDelay == 0 {
			a.logger.Warn("drain delay is 0; in Kubernetes set http.drain_delay or WithDrainDelay to avoid dropped requests during rolling updates")
		}
	}

	// 3.5. Build registry and start Components, if any were registered
	// or event hooks were subscribed via App.On. registry is left nil
	// only when no Components AND no hooks were registered, so legacy
	// apps pay no overhead; in practice initLogger always queues at
	// least the LoggerComponent.
	// Hold registryMu while consuming pending items and publishing the
	// registry pointer. This synchronizes with App.On() which also holds
	// registryMu when writing to pendingHooks, preventing hooks from being
	// silently dropped during the transfer window.
	a.registryMu.Lock()
	hasPending := len(a.pendingComponents) > 0 || len(a.pendingHooks) > 0
	var pendingComponents []component.Component
	var pendingHooks map[component.Event][]component.Hook
	if hasPending {
		pendingComponents = a.pendingComponents
		a.pendingComponents = nil
		pendingHooks = a.pendingHooks
		a.pendingHooks = nil
	}
	a.registryMu.Unlock()

	if hasPending {
		reg := component.New(a.configPtr, a.logger)
		reg.SetStopTimeout(a.shutdownTimeout)
		reg.SetDefaultInitTimeout(a.initTimeout)
		reg.SetCloseTimeout(a.closeTimeout)
		reg.SetHealthTimeout(a.healthTimeout)
		reg.SetHookTimeout(a.hookTimeout)
		reg.SetReloadTimeout(a.componentReloadTimeout)
		reg.PublishConfigSnapshot() // initial snapshot for ConfigSnapshot()
		for _, c := range pendingComponents {
			reg.Register(c)
		}
		for ev, hooks := range pendingHooks {
			for _, h := range hooks {
				reg.On(ev, h)
			}
		}
		// Publish the registry pointer before Start so that hooks
		// running during Start (e.g. EventAfterStart) can safely call
		// a.Registry(). Protected by registryMu for concurrent readers.
		a.registryMu.Lock()
		a.registry = reg
		a.registryMu.Unlock()
		if err := reg.Start(ctx); err != nil {
			a.logger.Error("component registry start failed", "error", err)
			// If the failure came from EventAfterStart, Init'd components
			// are still live (phaseStarted). Registry.Stop is idempotent
			// and a no-op for phaseStopped, so it's safe to call in either
			// case; skipping it would leak DB/cache/etc.
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.shutdownTimeout)
			if stopErr := reg.Stop(stopCtx); stopErr != nil {
				a.logger.Error("component registry stop after failed start", "error", stopErr)
			}
			cancel()
			a.runCleanups(ctx)
			return err
		}
	}

	// 3.55. Log the shutdown time budget so operators can verify the
	// drain + stop phases fit within the Kubernetes terminationGracePeriod.
	a.logShutdownBudget()

	// 3.6. Extract the HTTP server from HTTPComponent (if present) and
	// add it to the servers list. This bridges the Component lifecycle
	// (Init/Close via Registry) with the Server lifecycle (blocking
	// Start/Stop via runServers).
	a.extractHTTPServer()

	// Refresh a.cacher from the CacheComponent so App.Cacher() reflects
	// the full chain (memory → file → Redis) after Component Init.
	// Guarded by registryMu so handlers reading Cacher() concurrently
	// observe either the pre-Start or post-Start value, never a tear.
	if reg := a.Registry(); reg != nil {
		if cc, ok := reg.Get("cache").(*parts.CacheComponent); ok && cc != nil {
			if c := cc.Cache(); c != nil {
				a.registryMu.Lock()
				a.cacher = c
				a.registryMu.Unlock()
			}
		}
	}

	// 4+5. Start servers and wait for exit.
	err := a.runServers(ctx, rc)

	// 6. Stop the registry first (reverse Component Close order).
	// Uses the remaining shutdown budget so that server stop + registry
	// stop together never exceed shutdownTimeout.
	if reg := a.Registry(); reg != nil {
		remaining := a.shutdownTimeout
		if dl, ok := a.loadShutdownDeadline(); ok {
			remaining = time.Until(dl)
			if remaining <= 0 {
				remaining = time.Second // minimum budget for best-effort cleanup
			}
		}
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), remaining)
		if stopErr := reg.Stop(stopCtx); stopErr != nil {
			a.logger.Error("component registry stop", "error", stopErr)
		}
		cancel()
	}

	// 7. Legacy cleanup callbacks (still useful for ad-hoc resources
	// outside the Component abstraction).
	a.runCleanups(ctx)

	// 8. Return.
	return err
}

// initLogger sets up the logger based on priority:
// WithLogger > WithLogConfig > auto-discover SlogOptions from config > default (JSON, info).
// When opts.AccessFiles is non-empty, also constructs a separate access logger
// that writes to opts.Output + opts.AccessFiles (skipping opts.Files).
//
// After the logger is built, a LoggerComponent is auto-registered in
// pre-built mode so App.Registry().Reload can reach SetLevel without
// duplicating lifecycle code.
func (a *App) initLogger() error {
	if a.logger == nil {
		opts := a.logOpts
		if opts == nil && a.configPtr != nil {
			var err error
			opts, err = discoverOne[config.SlogOptions](a.configPtr)
			if err != nil {
				return fmt.Errorf("chok: %w", err)
			}
		}
		if opts != nil {
			a.logOpts = opts // cache for AccessLogEnabled() and other queries
			a.logger = log.NewSlog(opts)
			if len(opts.AccessFiles) > 0 {
				accessOpts := *opts
				accessOpts.Files = opts.AccessFiles
				accessOpts.AccessFiles = nil
				a.accessLogger = log.NewSlog(&accessOpts)
			}
		} else {
			a.logger = log.NewDefaultSlog()
		}
	}

	// Auto-register LoggerComponent (pre-built) so the registry owns
	// Reload dispatch without replacing the App's existing Logger()
	// accessor semantics.
	a.registerBuiltinLogger()
	return nil
}

// registerBuiltinLogger adds a LoggerComponent with WithPreBuilt(a.logger)
// and a resolver that re-discovers SlogOptions on every call. The
// resolver always re-reads the incoming cfg snapshot rather than
// returning the live a.logOpts pointer it captured at initLogger — if
// it returned the live pointer, LoggerComponent.Reload's prev/next
// comparison would see the same memory address on both sides (configMu
// writes into the same struct) and the "restart required" warnings
// for format/output/files changes would never fire.
func (a *App) registerBuiltinLogger() {
	resolver := func(cfg any) *config.SlogOptions {
		if cfg != nil {
			// discoverOne already validated in initLogger; Reload-time
			// re-discovery can safely fall back to first match because
			// ambiguity was caught at startup.
			opts, _ := discoverOne[config.SlogOptions](cfg)
			if opts != nil {
				// Return a value-copy so downstream comparisons see
				// a distinct *SlogOptions per call, enabling the
				// reload diff to detect field changes even when the
				// snapshot shares slice backing storage with live.
				cp := *opts
				return &cp
			}
		}
		// Final fallback: use the cached live pointer from initLogger.
		// This matters for apps that never ran a reload (cfg snapshot
		// wasn't populated for some reason) and for WithLogConfig usage.
		return a.logOpts
	}
	a.Register(parts.NewLoggerComponent(resolver).WithPreBuilt(a.logger, a.accessLogger))
}

// initCache is intentionally removed. Cache construction is unified in
// CacheComponent.Init via DefaultCacheBuilder, which can integrate Redis
// (via optional dependency) into the chain. The old initCache pre-built
// memory/file cache before the registry started, preventing Redis from
// entering the chain — a design bug documented in changelog round 12.

// discoverOne scans a config struct tree for fields of type T. Returns a
// pointer to the single match, or nil if not found. Returns an error when
// more than one field of type T exists anywhere in the tree — this catches
// misconfigured config structs that embed the same Options type in
// multiple places.
//
// Scanning rules:
//   - Nested value-type structs are descended into, so users can organise
//     their Config naturally (e.g. `Config.Cache.Memory`, `Config.Cache.File`).
//   - Pointer-typed fields are skipped entirely (neither matched nor
//     descended). This preserves the no-pointer-Options contract enforced
//     by validateNoPointerOptions — pointer Options break the reload
//     invariant because Reload's Set() copies values in-place and a
//     cached *PointerField would still hold the old object.
//   - Once a field matches *T, the walker does NOT descend into its own
//     fields — prevents double-counting if T happens to embed another T.
//   - config.SelfValidating types (e.g. DatabaseOptions with its mutually
//     exclusive SQLite/MySQL branches) are treated as opaque: the walker
//     neither matches them nor descends. Symmetric with validateFields,
//     which already stops recursing at SelfValidating so unselected
//     branches are not evaluated.
//   - Value-semantic structs like time.Time are skipped via isAtomicStruct
//     to avoid fruitless recursion into standard-library primitives.
func discoverOne[T any](cfg any) (*T, error) {
	rv := reflect.ValueOf(cfg)
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil, nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, nil
	}

	var matches []*T
	walkForDiscover(rv, &matches)

	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		var zero T
		return nil, fmt.Errorf("found %d fields of type %T in config tree, expected at most 1", len(matches), zero)
	}
}

// walkForDiscover recursively collects every *T reachable from rv under
// the rules documented on discoverOne. rv must already be a struct Value.
func walkForDiscover[T any](rv reflect.Value, out *[]*T) {
	t := rv.Type()
	for i := range rv.NumField() {
		fv := rv.Field(i)
		ft := t.Field(i)
		if !ft.IsExported() {
			continue
		}
		// Pointer fields: skip entirely. See doc on discoverOne.
		if fv.Kind() == reflect.Ptr {
			continue
		}
		if fv.CanAddr() {
			if tv, ok := fv.Addr().Interface().(*T); ok {
				*out = append(*out, tv)
				continue
			}
		}
		if fv.Kind() != reflect.Struct {
			continue
		}
		if isAtomicStruct(fv.Type()) {
			continue
		}
		// SelfValidating types own their own internal structure; the
		// framework must not assume anything about their fields.
		if fv.CanAddr() {
			if _, ok := fv.Addr().Interface().(config.SelfValidating); ok {
				continue
			}
		}
		walkForDiscover(fv, out)
	}
}

// isAtomicStruct returns true for value-semantic struct types that must
// never be descended into by discoverOne. time.Duration is int64, not a
// struct, so it doesn't need an entry here — only struct-kind values do.
func isAtomicStruct(t reflect.Type) bool {
	switch t {
	case reflect.TypeFor[time.Time]():
		return true
	}
	return false
}

// logShutdownBudget outputs the shutdown time allocation so operators can
// verify the configuration fits within the Kubernetes terminationGracePeriod.
// Warns when the budget is obviously tight (drain delay consumes > 50% of
// the overall shutdown timeout).
func (a *App) logShutdownBudget() {
	if a.drainDelay <= 0 {
		return // no drain = no coordination concern
	}
	remaining := a.shutdownTimeout - a.drainDelay
	a.logger.Info("shutdown time budget",
		"total", a.shutdownTimeout,
		"drain_delay", a.drainDelay,
		"remaining_for_stop", remaining,
		"close_timeout_per_component", a.closeTimeout,
	)
	if remaining < a.closeTimeout {
		a.logger.Warn("shutdown budget may be insufficient: drain_delay + close_timeout exceeds shutdown_timeout",
			"drain_delay", a.drainDelay,
			"close_timeout", a.closeTimeout,
			"shutdown_timeout", a.shutdownTimeout,
		)
	}
}

type startResult struct {
	index int
	err   error
}

// runServers handles steps 4-5: concurrent server start, readiness, and shutdown.
func (a *App) runServers(ctx context.Context, rc *runConfig) error {
	if len(a.servers) == 0 {
		// No servers — just wait for ctx or signal.
		return a.waitNoServers(ctx, rc)
	}

	n := len(a.servers)
	readyCh := make(chan int, n)
	doneCh := make(chan startResult, n)

	// Launch all servers.
	for i, srv := range a.servers {
		go func(idx int, s Server) {
			defer func() {
				if r := recover(); r != nil {
					doneCh <- startResult{index: idx, err: fmt.Errorf("server %d panicked: %v", idx, r)}
				}
			}()
			var readyOnce sync.Once
			err := s.Start(ctx, func() {
				readyOnce.Do(func() { readyCh <- idx })
			})
			doneCh <- startResult{index: idx, err: err}
		}(i, srv)
	}

	// Wait for all ready OR any failure OR ctx done.
	readyCount := 0
	for readyCount < n {
		select {
		case <-readyCh:
			readyCount++
		case res := <-doneCh:
			// A server returned during startup.
			err := res.err
			if err == nil {
				err = ErrServerUnexpectedExit
			}
			deadline := time.Now().Add(a.shutdownTimeout)
			// Only drain when at least one server is already ready —
			// otherwise no traffic is flowing, nothing for the LB to
			// deregister, and paying drainDelay here just prolongs a
			// fail-fast startup crash.
			if readyCount > 0 {
				a.beginDrain(deadline)
			}
			stopErrs := a.stopServersWithDeadline(ctx, time.Until(deadline))
			return errors.Join(append([]error{err}, stopErrs...)...)
		case <-ctx.Done():
			deadline := time.Now().Add(a.shutdownTimeout)
			a.beginDrain(deadline)
			stopErrs := a.stopServersWithDeadline(ctx, time.Until(deadline))
			return joinCtxError(ctx, stopErrs)
		}
	}

	a.logger.Info("all servers ready")

	// 5. All ready — wait for exit trigger.
	return a.waitForExit(ctx, rc, doneCh)
}

// waitNoServers handles the case where no servers are registered.
// Both the signals-enabled and signals-disabled paths loop so multiple
// reloads can be serviced across the App's lifetime. The signals-disabled
// loop exits only when the parent ctx is cancelled.
func (a *App) waitNoServers(ctx context.Context, rc *runConfig) error {
	fileRlCh := a.maybeStartFileWatcher(ctx, rc)

	if !rc.signals {
		for {
			select {
			case <-ctx.Done():
				return ctxErr(ctx)
			case <-fileRlCh:
				a.handleReloadWithReason(ctx, "file_change")
			}
		}
	}

	sigCtx, sigCancel := context.WithCancel(ctx)
	defer sigCancel()
	lcCh, rlCh := signalWatcher(sigCtx, a.logger)

	for {
		select {
		case <-ctx.Done():
			return ctxErr(ctx)
		case ev := <-lcCh:
			switch ev {
			case signalQuit, signalShutdown:
				return nil
			}
		case <-rlCh:
			a.handleReloadWithReason(ctx, "signal")
		case <-fileRlCh:
			a.handleReloadWithReason(ctx, "file_change")
		}
	}
}

// maybeStartFileWatcher returns a channel that fires when the config
// file changes. Returns nil when watching is disabled, the config path
// is empty, or the watcher couldn't be created. A nil channel in a
// select case is permanently blocked rather than panicking, so callers
// can include `case <-fileRlCh:` unconditionally and the case is simply
// ignored when no watcher is running.
func (a *App) maybeStartFileWatcher(ctx context.Context, rc *runConfig) <-chan struct{} {
	if !rc.watchConfig {
		return nil
	}
	if a.configPath == "" {
		a.logger.Warn("config watch: no config path configured, watcher not started")
		return nil
	}
	return configFileWatcher(ctx, a.configPath, a.logger)
}

// waitForExit handles step 5: wait for signal, ctx cancel, or server exit.
func (a *App) waitForExit(ctx context.Context, rc *runConfig, doneCh <-chan startResult) error {
	var lcCh <-chan signalEvent
	var rlCh <-chan struct{}
	var sigCancel context.CancelFunc

	if rc.signals {
		var sigCtx context.Context
		sigCtx, sigCancel = context.WithCancel(ctx)
		lcCh, rlCh = signalWatcher(sigCtx, a.logger)
		defer sigCancel()
	}

	fileRlCh := a.maybeStartFileWatcher(ctx, rc)

	for {
		select {
		case res := <-doneCh:
			// Server exited unexpectedly.
			err := res.err
			if err == nil {
				err = ErrServerUnexpectedExit
			}
			a.logger.Error("server exited unexpectedly", "error", err)
			deadline := time.Now().Add(a.shutdownTimeout)
			// Drain so /readyz flips to 503 and in-flight requests complete
			// before we tear down the remaining servers.
			a.beginDrain(deadline)
			stopErrs := a.stopServersWithDeadline(ctx, time.Until(deadline))
			return errors.Join(append([]error{err}, stopErrs...)...)

		case <-ctx.Done():
			a.logger.Info("context done, shutting down...")
			deadline := time.Now().Add(a.shutdownTimeout)
			a.beginDrain(deadline)
			stopErrs := a.stopServersWithDeadline(ctx, time.Until(deadline))
			return joinCtxError(ctx, stopErrs)

		case ev := <-lcCh:
			switch ev {
			case signalShutdown:
				a.logger.Info("received signal, shutting down...")
				deadline := time.Now().Add(a.shutdownTimeout)
				a.beginDrain(deadline)
				stopErrs := a.stopServersWithDeadline(ctx, time.Until(deadline))
				a.logger.Info("shutdown complete")
				return errors.Join(stopErrs...)

			case signalQuit:
				a.logger.Info("received SIGQUIT, fast shutdown")
				a.storeShutdownDeadline(time.Now().Add(a.shutdownTimeout))
				// SIGQUIT requests an immediate teardown — synthesize an
				// already-cancelled context but preserve trace_id /
				// request_id values from ctx so shutdown logs stay
				// correlated. WithoutCancel detaches ctx's own cancel
				// chain so the new cancel() below doesn't race with the
				// upstream one.
				canceledCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
				cancel()
				stopErrs := a.stopServers(canceledCtx)
				return errors.Join(stopErrs...)
			}

		case <-rlCh:
			a.handleReloadWithReason(ctx, "signal")

		case <-fileRlCh:
			a.handleReloadWithReason(ctx, "file_change")
		}
	}
}

// ErrReloadInProgress is returned by Reload when another reload is
// already executing. Callers triggered by external events (SIGHUP,
// fsnotify, admin API) should treat this as benign — the in-flight
// reload will pick up the latest on-disk config when it re-reads.
var ErrReloadInProgress = errors.New("chok: reload already in progress")

// Reload is the unified reload entry point. It performs, in order:
//
//  1. Re-read the config file + env (ReloadConfig). Validation errors
//     abort reload — the old config keeps running.
//  2. Dispatch Reload to every Reloadable Component registered with
//     the internal registry. Errors are logged; peers still get their
//     chance to absorb new config.
//  3. Invoke the user's reloadFn (WithReloadFunc) for ad-hoc work
//     that doesn't fit the Component model.
//
// Reload is public so external triggers (fsnotify, admin HTTP, tests)
// can invoke the same sequence as SIGHUP. Safe to call at any point
// after Run has built the registry; earlier calls skip the registry
// step.
//
// Concurrent invocations are coalesced: only one Reload runs at a
// time. Callers that arrive while a reload is already executing
// receive ErrReloadInProgress immediately rather than queueing.
// Queueing would let a slow reload cascade-block subsequent triggers
// (and ultimately the main loop), and the second caller would re-read
// stale config anyway since the in-flight reload already took the
// latest snapshot.
func (a *App) Reload(ctx context.Context) error {
	if !a.reloadMu.TryLock() {
		return ErrReloadInProgress
	}
	defer a.reloadMu.Unlock()

	changed, sections, err := a.ReloadConfig()
	if err != nil {
		return fmt.Errorf("reload config: %w", err)
	}
	// Snapshot the registry pointer once (race-safe) for use below.
	// ReloadConfig already publishes the new config snapshot inside
	// configMu, so we do NOT publish again here — a second publish
	// would read a.config without the lock (torn read on multi-field
	// structs) and gives nothing new.
	reg := a.Registry()
	if reg != nil {
		reloadOpts := []component.ReloadOption{
			component.WithReloadConfigChanged(changed),
		}
		if sections != nil {
			reloadOpts = append(reloadOpts, component.WithReloadChangedSections(sections))
		}
		if err := reg.Reload(ctx, reloadOpts...); err != nil {
			return fmt.Errorf("reload components: %w", err)
		}
	}
	if a.reloadFn != nil {
		if err := a.reloadFn(ctx); err != nil {
			return fmt.Errorf("reload callback: %w", err)
		}
	}
	return nil
}

// handleReloadWithReason runs Reload synchronously with a timeout,
// injecting the trigger reason into the context so hooks can
// distinguish signal vs file_change vs api_call. Blocking the main
// loop ensures Run cannot return while a reload is in progress.
//
// ErrReloadInProgress is logged at Info (not Error) — a coalesced
// reload is expected behaviour when SIGHUP and fsnotify fire at the
// same time, not a fault.
func (a *App) handleReloadWithReason(ctx context.Context, reason string) {
	rctx, cancel := context.WithTimeout(ctx, a.reloadTimeout)
	defer cancel()
	rctx = component.WithReason(rctx, reason)
	switch err := a.Reload(rctx); {
	case err == nil:
		a.logger.Info("reload completed")
	case errors.Is(err, ErrReloadInProgress):
		a.logger.Info("reload coalesced", "reason", reason)
	default:
		a.logger.Error("reload failed", "error", err)
	}
}

// shutdownMarker is implemented by components that support pre-drain
// shutdown signaling (e.g. HealthComponent flips readyz → 503).
type shutdownMarker interface {
	SetShuttingDown()
}

// beginDrain unconditionally marks readyz as 503, then pauses for the
// configured drain delay so load balancers can deregister the pod before
// in-flight connections are drained. The readyz flip always happens —
// even when drainDelay is zero — so that any in-flight readiness probe
// sees the shutdown signal. Only the sleep is conditional on drainDelay.
func (a *App) beginDrain(deadline time.Time) {
	// Always flip readyz → 503.
	if reg := a.Registry(); reg != nil {
		if hc, ok := reg.Get("health").(shutdownMarker); ok && hc != nil {
			hc.SetShuttingDown()
		}
	}
	if a.drainDelay <= 0 {
		return
	}
	// Reserve a minimum budget for the subsequent Stop calls. Without
	// this, a drainDelay >= shutdownTimeout would consume the entire
	// window and every server.Stop(ctx) would see an immediately-cancelled
	// context. 1s is enough for http.Server.Shutdown to trigger the
	// shutdown flow on already-idle connections.
	const minStopBudget = 1 * time.Second
	remaining := time.Until(deadline)
	if remaining <= minStopBudget {
		// Not enough budget to both drain AND stop — skip drain entirely
		// and preserve whatever time is left for Stop. /readyz has
		// already been flipped to 503 so the LB sees the shutdown
		// signal; skipping the sleep just means we don't wait for LB
		// deregistration before tearing down connections.
		a.logger.Warn("drain delay skipped: remaining budget below minimum",
			"remaining", remaining, "min_stop_budget", minStopBudget)
		return
	}
	delay := a.drainDelay
	if delay > remaining-minStopBudget {
		delay = remaining - minStopBudget
	}
	a.logger.Info("drain delay started, readyz returning 503", "delay", delay)
	time.Sleep(delay)
}

// stopServersWithDeadline stops all servers in reverse order with a
// timeout. base supplies trace_id / request_id / logger values; its
// cancellation is intentionally detached via context.WithoutCancel so
// shutdown work continues even when the parent ctx (request scope or
// signal-derived) has already been cancelled. The deadline still bounds
// the shutdown window.
func (a *App) stopServersWithDeadline(base context.Context, timeout time.Duration) []error {
	deadline := time.Now().Add(timeout)
	a.storeShutdownDeadline(deadline)
	ctx, cancel := context.WithDeadline(context.WithoutCancel(base), deadline)
	defer cancel()
	return a.stopServers(ctx)
}

// storeShutdownDeadline records the shutdown deadline. Safe for
// concurrent access — readers use loadShutdownDeadline.
func (a *App) storeShutdownDeadline(t time.Time) {
	a.shutdownNanos.Store(t.UnixNano())
}

// loadShutdownDeadline returns the shutdown deadline if one has been
// set. Safe for concurrent access.
func (a *App) loadShutdownDeadline() (time.Time, bool) {
	ns := a.shutdownNanos.Load()
	if ns == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
}

// stopServers stops all servers in reverse order.
func (a *App) stopServers(ctx context.Context) []error {
	var errs []error
	for i := len(a.servers) - 1; i >= 0; i-- {
		if err := a.servers[i].Stop(ctx); err != nil {
			a.logger.Error("server stop error", "index", i, "error", err)
			errs = append(errs, err)
		}
	}
	return errs
}

// runCleanups calls all cleanup functions in LIFO order. base supplies
// trace/request values for shutdown logs; its cancellation is detached
// (context.WithoutCancel) so cleanup keeps running even when the parent
// has already been cancelled. The deadline still bounds the work.
func (a *App) runCleanups(base context.Context) {
	if len(a.cleanupFns) == 0 {
		return
	}
	parent := context.WithoutCancel(base)
	var ctx context.Context
	var cancel context.CancelFunc
	if dl, ok := a.loadShutdownDeadline(); ok {
		ctx, cancel = context.WithDeadline(parent, dl)
	} else {
		ctx, cancel = context.WithTimeout(parent, a.shutdownTimeout)
	}
	defer cancel()
	for i := len(a.cleanupFns) - 1; i >= 0; i-- {
		if err := a.cleanupFns[i](ctx); err != nil {
			if a.logger != nil {
				a.logger.Error("cleanup error", "error", err)
			} else {
				fmt.Fprintf(os.Stderr, "chok: cleanup error: %v\n", err)
			}
		}
	}
}

func ctxErr(ctx context.Context) error {
	if ctx.Err() == context.Canceled {
		return nil
	}
	return ctx.Err()
}

func joinCtxError(ctx context.Context, stopErrs []error) error {
	ce := ctxErr(ctx)
	if ce == nil && len(stopErrs) == 0 {
		return nil
	}
	if ce == nil {
		return errors.Join(stopErrs...)
	}
	return errors.Join(append([]error{ce}, stopErrs...)...)
}
