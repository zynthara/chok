// Package chok is the v2 application shell: explicit typed assembly
// over the kernel control plane. The whole wiring of an app is
//
//	chok.New("blog",
//	    chok.Use(log.Module(), web.Module(), db.Module(...)),
//	    chok.Routes(registerRoutes),
//	).Execute()
//
// No reflection scanning, no auto-registration: what you Use is what
// links, yaml stays the runtime switch (SPEC §3.1).
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
	"time"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/kernel/event"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/version"
)

// Application code speaks the chok vocabulary: the kernel contracts
// that appear in root-package signatures are aliased here so an app
// never imports kernel just to write its Routes callback. Module
// authors (Descriptor, Mounter, ...) keep the kernel spelling — that
// is the extension surface, not the assembly surface.
type (
	// Router is the route-registration contract handed to the Routes
	// callback (contract in kernel, implementation in web).
	Router = kernel.Router
	// Kernel is the component-lookup handle passed to Routes and
	// consumed by the battery accessors (db.From, log.From, ...).
	Kernel = kernel.Kernel
	// Middleware is the framework-wide middleware shape: a plain
	// http.Handler decorator.
	Middleware = kernel.Middleware
	// Component is what Use assembles; built-in modules construct
	// theirs via their Module() functions.
	Component = kernel.Component
)

// Get re-exports kernel.Get: typed (kind, instance) component access.
func Get[T any](k Kernel, kind string, instance ...string) (T, bool) {
	return kernel.Get[T](k, kind, instance...)
}

// App is the single-use application container.
type App struct {
	name      string
	envPrefix string
	version   version.Info

	modules   []Component
	overrides []Component
	routes    func(r Router, k Kernel) error
	reloadFn  kernel.PostReloadFunc

	errorMappers *apierr.MapperRegistry

	configFile string
	flags      any // *pflag.FlagSet (kept loose to avoid the hard dep here)

	logger     log.Logger
	userLogger bool

	timeouts        kernel.Timeouts
	shutdownTimeout time.Duration
	reloadTimeout   time.Duration
	drainDelay      time.Duration

	mu       sync.Mutex
	sections map[string]any // business Section[T] registrations
	running  bool

	store *conf.Store
	reg   *kernel.Registry

	ranOnce sync.Once
}

// New constructs an App. Assembly problems (duplicate keys, Override
// on a missing key, config validation) surface at Run — fail-fast at
// startup, never silently.
func New(name string, opts ...Option) *App {
	a := &App{
		name:            name,
		envPrefix:       defaultEnvPrefix(name),
		sections:        make(map[string]any),
		shutdownTimeout: 30 * time.Second,
		reloadTimeout:   30 * time.Second,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

func defaultEnvPrefix(name string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

// Section registers a typed business config section and returns its
// handle. Must be called before Run — the loader needs the complete
// type set to bind env vars, apply defaults and validate (SPEC §3.4);
// calling it after Run panics.
func Section[T any](a *App, key string) *SectionHandle[T] {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		panic(fmt.Sprintf("chok: Section(%q) must be called before Run", key))
	}
	var zero T
	if reflect.TypeOf(zero).Kind() != reflect.Struct {
		panic(fmt.Sprintf("chok: Section(%q) requires a struct type, got %T", key, zero))
	}
	a.sections[strings.ToLower(key)] = zero
	return &SectionHandle[T]{app: a, key: strings.ToLower(key)}
}

// SectionHandle is the typed accessor for one business config section.
type SectionHandle[T any] struct {
	app *App
	key string
}

// Get decodes the section from the current snapshot. The value is a
// copy — treat it as immutable, re-Get after reloads. Panics when
// called before Run (no snapshot exists yet).
func (h *SectionHandle[T]) Get() T {
	store := h.app.storeRef()
	if store == nil {
		panic(fmt.Sprintf("chok: Section(%q).Get before Run — config is not loaded yet", h.key))
	}
	var out T
	if err := store.Snapshot().Section(h.key, &out); err != nil {
		// Registered sections were validated at load; failure here is a
		// programming error, not an operator error.
		panic(fmt.Sprintf("chok: Section(%q).Get: %v", h.key, err))
	}
	return out
}

func (a *App) storeRef() *conf.Store {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.store
}

// Logger returns the root logger. Before Run it returns the injected
// logger or an inert one — startup/shutdown logging only; request-path
// code uses the context logger.
func (a *App) Logger() log.Logger {
	if a.logger == nil {
		return log.Empty()
	}
	return a.logger
}

// ErrorMappers exposes the per-App error mapper registry (consumed by
// the web middleware stack from M2 on).
func (a *App) ErrorMappers() *apierr.MapperRegistry { return a.errorMappers }

// Kernel returns the running kernel (nil before Run) — mainly for
// tests and advanced integrations; components receive it in Init.
func (a *App) Kernel() Kernel {
	if r := a.regRef(); r != nil {
		return r
	}
	return nil
}

func (a *App) regRef() *kernel.Registry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reg
}

// Execute runs the app with OS signal handling and exits non-zero on
// error. main() convenience; tests use Run.
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

// Run assembles, starts and supervises the app until ctx ends, a
// shutdown signal arrives (WithSignals) or a server fails. Single-use.
func (a *App) Run(ctx context.Context, opts ...RunOption) error {
	first := false
	a.ranOnce.Do(func() { first = true })
	if !first {
		return errors.New("chok: Run/Execute already called (App is single-use)")
	}

	rc := &runConfig{}
	for _, o := range opts {
		o(rc)
	}

	// Ownership decision happens before anything can fail: a logger the
	// caller injected is never closed by the App, even on early errors.
	a.userLogger = a.logger != nil

	if err := a.assemble(); err != nil {
		return err
	}
	// The App owns the root logger it built: flush/close after the
	// control plane has fully stopped, so component Close and bus
	// drain can always log (SPEC §3.5). Injected loggers belong to
	// the caller.
	defer func() {
		if !a.userLogger {
			if c, ok := a.logger.(io.Closer); ok {
				_ = c.Close()
			}
		}
	}()

	a.logger.Info("starting application", "name", a.name, "version", a.version.String())

	if err := a.reg.Start(ctx); err != nil {
		a.logger.Error("startup failed", "error", err)
		return err
	}

	var (
		lcCh    <-chan signalEvent
		rlCh    <-chan struct{}
		watchCh <-chan struct{}
	)
	if rc.signals {
		lcCh, rlCh = signalWatcher(ctx, a.logger)
	}
	if rc.watchConfig {
		path := a.store.Path()
		if path == "" {
			a.logger.Warn("config watch requested but no config file was loaded; watching disabled")
		} else {
			watchCh = configFileWatcher(ctx, path, a.logger)
		}
	}

	var runErr error
	fast := false
loop:
	for {
		select {
		case <-ctx.Done():
			// Caller cancellation is an orderly stop (nil); an expired
			// deadline is an error (v1 contract).
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				runErr = ctx.Err()
			}
			break loop
		case ev := <-lcCh:
			a.logger.Info("received signal, shutting down...", "fast", ev == signalQuit)
			fast = ev == signalQuit
			break loop
		case err := <-a.reg.Failed():
			a.logger.Error("server failure, shutting down...", "error", err)
			runErr = err
			break loop
		case <-rlCh:
			a.reloadLogged(ctx, "signal")
		case <-watchCh:
			a.reloadLogged(ctx, "config file change")
		}
	}

	budget := a.shutdownTimeout
	if fast && budget > 5*time.Second {
		budget = 5 * time.Second
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	if err := a.reg.Stop(stopCtx); err != nil {
		runErr = errors.Join(runErr, err)
	}
	a.logger.Info("shutdown complete")
	return runErr
}

// Reload triggers the three-stage reload pipeline (config swap →
// component dispatch → post-reload callback). Concurrent calls get
// kernel.ErrReloadInProgress.
func (a *App) Reload(ctx context.Context) error {
	reg := a.regRef()
	if reg == nil {
		return kernel.ErrNotStarted
	}
	rctx, cancel := context.WithTimeout(ctx, a.reloadTimeout)
	defer cancel()
	return reg.Reload(rctx)
}

func (a *App) reloadLogged(ctx context.Context, trigger string) {
	a.logger.Info("reload triggered", "by", trigger)
	if err := a.Reload(ctx); err != nil {
		if errors.Is(err, kernel.ErrReloadInProgress) {
			a.logger.Warn("reload skipped: previous reload still in progress")
			return
		}
		a.logger.Error("reload failed", "error", err)
		return
	}
	a.logger.Info("reload complete")
}

// assemble builds loader → store → root logger → kernel registry.
func (a *App) assemble() error {
	a.mu.Lock()
	a.running = true
	sections := a.sections
	a.mu.Unlock()

	comps, err := a.resolveModules()
	if err != nil {
		return err
	}

	// Hand the per-App error-mapper registry to whichever assembled
	// component consumes it (the web module's middleware stack) — a
	// structural handshake, so the kernel keeps knowing no names and
	// WithErrorMapper works for any RouterProvider implementation.
	if a.errorMappers != nil {
		for _, c := range comps {
			if am, ok := c.(interface{ AttachErrorMappers(*apierr.MapperRegistry) }); ok {
				am.AttachErrorMappers(a.errorMappers)
			}
		}
	}

	loader := conf.NewLoader(a.name, a.envPrefix)
	if a.configFile != "" {
		loader.SetPath(a.configFile)
	}
	if fs := a.pflags(); fs != nil {
		loader.SetFlags(fs)
	}

	// The root logger's section is an App-level convention: register
	// it unconditionally so level/format defaults apply even before
	// (or without) log.Module — the kernel itself knows no names.
	if err := loader.Register("log", log.Options{}); err != nil {
		return fmt.Errorf("chok: %w", err)
	}
	for _, c := range comps {
		d := c.Describe()
		key := kernel.SectionKeyOf(d)
		if key == "" || d.Options == nil {
			continue
		}
		if err := loader.Register(key, d.Options); err != nil {
			return fmt.Errorf("chok: module %s: %w", kernel.KeyOf(d), err)
		}
	}
	for key, sample := range sections {
		if err := loader.Register(key, sample); err != nil {
			return fmt.Errorf("chok: business section: %w", err)
		}
	}

	store, err := conf.NewStore(loader)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.store = store
	a.mu.Unlock()

	// Drain-delay inheritance (SPEC §9): an explicit WithDrainDelay
	// wins; otherwise the "http" section's drain_delay applies when an
	// http-owning module is assembled and enabled. Like the "log"
	// section above, "http" is an App-level section convention — the
	// kernel itself keeps knowing no names.
	a.drainDelay = inheritDrainDelay(a.drainDelay, comps, store.Snapshot())

	if !a.userLogger {
		var lo log.Options
		if err := store.Snapshot().Section("log", &lo); err != nil {
			return fmt.Errorf("chok: log section: %w", err)
		}
		a.logger = log.New(lo)
	}

	var routes kernel.RoutesFunc
	if a.routes != nil {
		routes = func(r Router) error { return a.routes(r, a.regRef()) }
	}

	reg, err := kernel.New(kernel.Config{
		Logger:     a.logger,
		Store:      store,
		Bus:        event.NewBus(event.WithLogger(a.logger)),
		Components: comps,
		Routes:     routes,
		PostReload: a.reloadFn,
		Defaults:   a.timeouts,
		DrainDelay: a.drainDelay,
	})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.reg = reg
	a.mu.Unlock()
	return nil
}

// inheritDrainDelay resolves the effective kernel drain delay:
// explicit (non-zero) App option first, then the http section's
// drain_delay when a component owning the "http" section is assembled
// and enabled (v1 autoRegisterHTTP inheritance, SPEC §9).
func inheritDrainDelay(explicit time.Duration, comps []Component, snap *conf.Snapshot) time.Duration {
	if explicit != 0 {
		return explicit
	}
	for _, c := range comps {
		if kernel.SectionKeyOf(c.Describe()) != "http" {
			continue
		}
		if !snap.EnabledFor("http") {
			return explicit
		}
		var o struct {
			DrainDelay time.Duration `mapstructure:"drain_delay"`
		}
		if err := snap.Section("http", &o); err == nil && o.DrainDelay > 0 {
			return o.DrainDelay
		}
		return explicit
	}
	return explicit
}

// resolveModules merges Use and Override: duplicates inside Use fail
// fast; each Override must replace an existing key (typo guard).
func (a *App) resolveModules() ([]Component, error) {
	seen := make(map[kernel.Key]int, len(a.modules))
	out := make([]Component, 0, len(a.modules))
	for _, c := range a.modules {
		d := c.Describe()
		if d.Kind == "" {
			return nil, fmt.Errorf("chok: module %T declares an empty Kind", c)
		}
		k := kernel.KeyOf(d)
		if _, dup := seen[k]; dup {
			return nil, fmt.Errorf("chok: Use lists %s twice — remove the duplicate or replace intentionally with chok.Override", k)
		}
		seen[k] = len(out)
		out = append(out, c)
	}
	for _, c := range a.overrides {
		k := kernel.KeyOf(c.Describe())
		idx, ok := seen[k]
		if !ok {
			return nil, fmt.Errorf("chok: Override(%s) matches no assembled module — check the kind/instance", k)
		}
		out[idx] = c
	}
	return out, nil
}
