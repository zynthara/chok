package chok

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/zynthara/chok/cache"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/log"
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
		case <-sctx.Done():
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
type App struct {
	name            string
	servers         []Server
	logger          log.Logger
	cacher          cache.Cache
	gormDB          any // *gorm.DB — stored as any to avoid importing gorm in root package
	logOpts         *config.SlogOptions
	cacheMemOpts    *config.CacheMemoryOptions
	cacheFileOpts   *config.CacheFileOptions
	setupFn         func(context.Context, *App) error
	cleanupFns      []func(context.Context) error
	reloadFn        func(context.Context) error
	configPtr       any
	configPath      string
	configExplicit  bool
	flagSet         any // *pflag.FlagSet — stored as any to avoid importing pflag in chok.go
	version         version.Info
	envPrefix       string
	shutdownTimeout  time.Duration
	reloadTimeout    time.Duration
	shutdownDeadline time.Time // set at shutdown trigger, shared by stop + cleanup

	once sync.Once // ensures Run/Execute called at most once
}

// New creates an App (pure construction, zero side effects).
func New(name string, opts ...Option) *App {
	a := &App{
		name:            name,
		envPrefix:       strings.ToUpper(strings.ReplaceAll(name, "-", "_")),
		shutdownTimeout: 30 * time.Second,
		reloadTimeout:   10 * time.Second,
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
func (a *App) AddCleanup(f func(context.Context) error) {
	a.cleanupFns = append(a.cleanupFns, f)
}

// Logger returns the current logger.
func (a *App) Logger() log.Logger {
	if a.logger == nil {
		return log.Empty()
	}
	return a.logger
}

// SetCacher registers a Cache on the App. Typically called in Setup after
// building the cache from config. Also registers a cleanup callback to
// close the cache on shutdown.
func (a *App) SetCacher(c cache.Cache) {
	a.cacher = c
	a.AddCleanup(func(_ context.Context) error { return c.Close() })
}

// Cacher returns the registered Cache, or nil if none was set.
func (a *App) Cacher() cache.Cache {
	return a.cacher
}

// SetDB stores a *gorm.DB on the App and registers a Close cleanup.
// Unlike Logger and Cache, DB is not auto-discovered — the user must
// explicitly create the connection in Setup (choose driver, run migrations, etc.).
func (a *App) SetDB(gdb any) {
	a.gormDB = gdb
}

// DB returns the stored *gorm.DB, or nil. The caller must type-assert:
//
//	gdb := a.DB().(*gorm.DB)
func (a *App) DB() any {
	return a.gormDB
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
		a.runCleanups()
		return err
	}

	// 2. Initialize logger (WithLogger > WithLogConfig > default).
	a.initLogger()

	a.logger.Info("starting application",
		"name", a.name,
		"version", a.version.String(),
	)

	// 2.5. Initialize cache (WithCacheConfig > SetCacher in Setup > nil).
	if err := a.initCache(); err != nil {
		a.runCleanups()
		return err
	}

	// 3. Setup.
	if a.setupFn != nil {
		if err := a.setupFn(ctx, a); err != nil {
			a.logger.Error("setup failed", "error", err)
			a.runCleanups()
			return err
		}
	}

	// 4+5. Start servers and wait for exit.
	err := a.runServers(ctx, rc)

	// 6. Cleanup.
	a.runCleanups()

	// 7. Return.
	return err
}

// initLogger sets up the logger based on priority:
// WithLogger > WithLogConfig > auto-discover SlogOptions from config > default (JSON, info).
func (a *App) initLogger() {
	if a.logger != nil {
		return // WithLogger already set
	}
	opts := a.logOpts
	if opts == nil && a.configPtr != nil {
		opts = discover[config.SlogOptions](a.configPtr)
	}
	if opts != nil {
		a.logger = log.NewSlog(opts)
		return
	}
	a.logger = log.NewDefaultSlog()
}

// initCache auto-discovers cache config from the typed config struct and builds
// the cache. Scans for config.CacheMemoryOptions and config.CacheFileOptions
// fields, same pattern as Validate() discovery.
// Priority: SetCacher (explicit in Setup) > auto-discovery from config > nil.
func (a *App) initCache() error {
	if a.cacher != nil {
		return nil // SetCacher already called
	}

	var bopts cache.BuildOptions
	bopts.Logger = a.logger

	// Discover cache config from WithCacheConfig pointers or typed config struct.
	memOpts, fileOpts := a.cacheMemOpts, a.cacheFileOpts
	if memOpts == nil && a.configPtr != nil {
		memOpts = discover[config.CacheMemoryOptions](a.configPtr)
	}
	if fileOpts == nil && a.configPtr != nil {
		fileOpts = discover[config.CacheFileOptions](a.configPtr)
	}

	if memOpts != nil && memOpts.Enabled {
		bopts.Memory = &cache.MemoryOptions{
			Capacity: memOpts.Capacity,
			TTL:      memOpts.TTL,
		}
	}
	if fileOpts != nil && fileOpts.Enabled {
		bopts.File = &cache.FileOptions{
			Path: fileOpts.Path,
			TTL:  fileOpts.TTL,
		}
	}

	c, err := cache.Build(bopts)
	if err != nil {
		return fmt.Errorf("chok: init cache: %w", err)
	}
	if c != nil {
		a.cacher = c
		a.AddCleanup(func(_ context.Context) error { return c.Close() })
	}
	return nil
}

// discover scans a config struct recursively for the first field of type T
// and returns a pointer to it. Returns nil if not found.
func discover[T any](cfg any) *T {
	var result *T
	scanFields(reflect.ValueOf(cfg), &result)
	return result
}

func scanFields[T any](rv reflect.Value, out **T) {
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return
	}
	for i := range rv.NumField() {
		fv := rv.Field(i)
		ft := rv.Type().Field(i)
		if !ft.IsExported() {
			continue
		}
		if fv.Kind() == reflect.Ptr {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
		}
		if fv.CanAddr() {
			if t, ok := fv.Addr().Interface().(*T); ok && *out == nil {
				*out = t
				return
			}
		}
		if fv.Kind() == reflect.Struct {
			scanFields(fv, out)
			if *out != nil {
				return
			}
		}
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
			// Stop all servers — Server contract guarantees Stop-before-Start safety.
			stopErrs := a.stopServersWithDeadline(context.Background(), a.shutdownTimeout)
			return errors.Join(append([]error{err}, stopErrs...)...)
		case <-ctx.Done():
			stopErrs := a.stopServersWithDeadline(context.Background(), a.shutdownTimeout)
			return joinCtxError(ctx, stopErrs)
		}
	}

	a.logger.Info("all servers ready")

	// 5. All ready — wait for exit trigger.
	return a.waitForExit(ctx, rc, doneCh)
}

// waitNoServers handles the case where no servers are registered.
func (a *App) waitNoServers(ctx context.Context, rc *runConfig) error {
	if !rc.signals {
		<-ctx.Done()
		return ctxErr(ctx)
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
			a.handleReload(ctx)
		}
	}
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

	for {
		select {
		case res := <-doneCh:
			// Server exited unexpectedly.
			err := res.err
			if err == nil {
				err = ErrServerUnexpectedExit
			}
			a.logger.Error("server exited unexpectedly", "error", err)
			stopErrs := a.stopServersWithDeadline(context.Background(), a.shutdownTimeout)
			return errors.Join(append([]error{err}, stopErrs...)...)

		case <-ctx.Done():
			a.logger.Info("context done, shutting down...")
			stopErrs := a.stopServersWithDeadline(context.Background(), a.shutdownTimeout)
			return joinCtxError(ctx, stopErrs)

		case ev := <-lcCh:
			switch ev {
			case signalShutdown:
				a.logger.Info("received signal, shutting down...")
				stopErrs := a.stopServersWithDeadline(context.Background(), a.shutdownTimeout)
				a.logger.Info("shutdown complete")
				return errors.Join(stopErrs...)

			case signalQuit:
				a.logger.Info("received SIGQUIT, fast shutdown")
				a.shutdownDeadline = time.Now().Add(a.shutdownTimeout)
				canceledCtx, cancel := context.WithCancel(context.Background())
				cancel()
				stopErrs := a.stopServers(canceledCtx)
				return errors.Join(stopErrs...)
			}

		case <-rlCh:
			a.handleReload(ctx)
		}
	}
}

// handleReload runs the reload callback synchronously.
// Blocking the main loop ensures Run cannot return while reload is in progress.
func (a *App) handleReload(ctx context.Context) {
	if a.reloadFn == nil {
		a.logger.Info("reload not configured")
		return
	}
	rctx, cancel := context.WithTimeout(ctx, a.reloadTimeout)
	defer cancel()
	if err := a.reloadFn(rctx); err != nil {
		a.logger.Error("reload failed", "error", err)
	} else {
		a.logger.Info("reload completed")
	}
}

// stopServersWithDeadline stops all servers in reverse order with a timeout.
func (a *App) stopServersWithDeadline(base context.Context, timeout time.Duration) []error {
	a.shutdownDeadline = time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(base, a.shutdownDeadline)
	defer cancel()
	return a.stopServers(ctx)
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

// runCleanups calls all cleanup functions in LIFO order.
func (a *App) runCleanups() {
	if len(a.cleanupFns) == 0 {
		return
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if !a.shutdownDeadline.IsZero() {
		ctx, cancel = context.WithDeadline(context.Background(), a.shutdownDeadline)
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), a.shutdownTimeout)
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
