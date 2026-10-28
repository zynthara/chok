package chok

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zynthara/chok/v3/internal/apptest"
	"github.com/zynthara/chok/v3/internal/openapi"
)

// App is one application: its configuration, the components registered
// with Use, and the HTTP handler its routes build. An App runs once.
type App struct {
	base    Base
	logger  *slog.Logger
	level   *slog.LevelVar // nil when the logger was injected
	access  *slog.Logger
	closers []io.Closer
	version string
	wrap    []Middleware
	onError ErrorHook
	reload  *loaded

	mux    *http.ServeMux
	routes routeTable

	handlerOnce sync.Once
	handler     http.Handler

	mu            sync.Mutex
	components    []*component
	instrumenters []Instrumenter
	started       bool

	ready    atomic.Bool
	draining atomic.Bool
	addr     atomic.Pointer[string]

	// wrapSecurity is what the Wrap middleware's authentication accepts,
	// on every route; buildHandler records it.
	wrapSecurity []openapi.Scheme
	// cookieAuth is set when a route accepts a session cookie.
	cookieAuth atomic.Bool

	// crossOrigin is the CSRF protection, in the stack with http.csrf;
	// exempt is the patterns batteries exempted from it (under mu).
	crossOrigin *http.CrossOriginProtection
	exempt      map[string]bool
}

type component struct {
	value any
	name  string
}

// Option configures New.
type Option func(*App)

// Logger injects the root logger; without it New builds one from
// base.Log. An injected logger is never closed by the App, SIGHUP cannot
// change its level, and telemetry's logs.otel bridge does not see it.
func Logger(l *slog.Logger) Option { return func(a *App) { a.logger = l } }

// Wrap adds application middleware, run after the default stack and
// before route groups.
func Wrap(mw ...Middleware) Option {
	return func(a *App) { a.wrap = append(a.wrap, mw...) }
}

// Version is shown on /healthz and in the start log line.
func Version(v string) Option { return func(a *App) { a.version = v } }

// Closer is a component that releases resources when the App stops.
type Closer interface {
	Close(ctx context.Context) error
}

// Starter is a component with long-running work. Start blocks until ctx
// is canceled, then returns nil; an error ends the App.
type Starter interface {
	Start(ctx context.Context) error
}

// Healther is a component that reports its health on /healthz.
type Healther interface {
	Health(ctx context.Context) error
}

// Mounter is a component that registers its own routes. Use calls Mount
// immediately, so a battery's routes precede the ones the application
// declares after Use.
type Mounter interface{ Mount(r Router) }

// Namer names a component in the health report; the default is its
// type name.
type Namer interface{ Name() string }

// New builds an App from its configuration. It never applies defaults:
// base normally comes from Load or MustLoad. Unless Logger is given, New
// builds the root logger from base.Log and installs it as slog.Default.
func New(base Base, opts ...Option) *App {
	a := &App{base: base, mux: http.NewServeMux(), reload: lastLoad.Load()}
	for _, o := range opts {
		o(a)
	}
	if a.logger == nil {
		l := buildLoggers(base.Log)
		a.logger, a.level, a.access, a.closers = l.root, l.level, l.access, l.closers
		slog.SetDefault(a.logger)
	} else if base.Log.Access {
		a.access = a.logger
	}
	a.crossOrigin = a.crossOriginProtection()
	a.mountBuiltins()
	return a
}

// Use registers components. Each is handled by the interfaces it
// implements: Starter, Closer, Healther, Mounter, Namer and Instrumenter.
// Use order is Start and Mount order; Close runs in reverse. A value that
// implements none of them is ignored with a warning.
func (a *App) Use(components ...any) {
	var mount []Mounter
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		panic("chok: Use after Run: register every component before running the App")
	}
	for _, c := range components {
		if c == nil {
			a.mu.Unlock()
			panic("chok: Use(nil)")
		}
		_, starter := c.(Starter)
		_, closer := c.(Closer)
		_, healther := c.(Healther)
		m, mounter := c.(Mounter)
		in, instrumenter := c.(Instrumenter)
		if !starter && !closer && !healther && !mounter && !instrumenter {
			a.logger.Warn("chok: Use: value implements none of Start, Close, Health, Mount or Middleware; ignored",
				"type", fmt.Sprintf("%T", c))
			continue
		}
		a.components = append(a.components, &component{value: c, name: nameOf(c)})
		if instrumenter {
			a.instrumenters = append(a.instrumenters, in)
		}
		if mounter {
			mount = append(mount, m)
		}
	}
	a.mu.Unlock()
	for _, m := range mount {
		m.Mount(a.Router())
	}
}

func nameOf(c any) string {
	if n, ok := c.(Namer); ok {
		return n.Name()
	}
	t := reflect.TypeOf(c)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Name() != "" {
		return t.Name()
	}
	return t.String()
}

// Logger returns the root logger.
func (a *App) Logger() *slog.Logger { return a.logger }

// Addr returns the address the HTTP server listens on, "" before it does.
func (a *App) Addr() string {
	if p := a.addr.Load(); p != nil {
		return *p
	}
	return ""
}

var errFastStop = errors.New("chok: fast stop")

func init() {
	apptest.Ready = func(app any) bool {
		a := app.(*App)
		return a.ready.Load() && !a.draining.Load()
	}
}

// Run starts the App and blocks until ctx is done or a component fails.
//
// Start phase: every Starter's Start runs in its own goroutine, in Use
// order; then every Healther must report healthy within app.start_timeout
// (a Start that fails first aborts the start). Only then does the HTTP
// server listen and /readyz turn 200.
//
// Stop phase, under one app.shutdown_timeout budget: /readyz turns 503,
// http.drain_delay passes, the HTTP server shuts down within
// http.shutdown_timeout, the Starters' context is canceled and Run waits
// for them, then every Closer is closed in reverse Use order.
//
// Run returns nil when ctx is canceled, ctx.Err() when its deadline
// expires, and the failure otherwise. It can be called once.
func (a *App) Run(ctx context.Context) error {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return errors.New("chok: Run called twice; an App runs once")
	}
	a.started = true
	comps := slices.Clone(a.components)
	a.mu.Unlock()
	defer a.closeLogs()

	err := a.run(ctx, comps)
	if err != nil {
		a.logger.Error("chok: stopped with an error", "error", err)
	}
	return err
}

func (a *App) run(ctx context.Context, comps []*component) error {
	begin := time.Now()
	attrs := []any{"components", len(comps)}
	if a.version != "" {
		attrs = append(attrs, "version", a.version)
	}
	a.logger.Info("chok: starting", attrs...)
	a.Handler() // the middleware stack is fixed: every route's schemes are known
	a.warnCookieWithoutCSRF()

	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	failed := make(chan error, len(comps)+1)
	var starters sync.WaitGroup
	for _, c := range comps {
		s, ok := c.value.(Starter)
		if !ok {
			continue
		}
		starters.Go(func() {
			err := callStart(runCtx, s)
			switch {
			case err != nil && runCtx.Err() == nil:
				failed <- fmt.Errorf("%s: %w", c.name, err)
			case err != nil:
				a.logger.Warn("chok: component returned an error while stopping", "component", c.name, "error", err)
			}
		})
	}

	stop := func(fast bool) error {
		return a.stop(ctx, nil, cancelRun, &starters, comps, fast)
	}
	if err := a.awaitHealthy(ctx, comps, failed); err != nil {
		return errors.Join(fmt.Errorf("chok: start: %w", err), stop(false))
	}

	var srv *http.Server
	if a.base.HTTP.Enabled {
		ln, err := net.Listen("tcp", a.base.HTTP.Addr)
		if err != nil {
			return errors.Join(fmt.Errorf("chok: http: %w", err), stop(false))
		}
		addr := ln.Addr().String()
		a.addr.Store(&addr)
		srv = a.server()
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- fmt.Errorf("http: %w", err)
			}
		}()
		a.logger.Info("chok: http listening", "addr", addr)
	}
	a.ready.Store(true)
	a.logger.Info("chok: started", "duration", time.Since(begin).Round(time.Millisecond))

	var runErr error
	fast := false
	select {
	case <-ctx.Done():
		switch {
		case errors.Is(context.Cause(ctx), errFastStop):
			fast = true
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			runErr = ctx.Err()
		}
	case err := <-failed:
		runErr = err
		a.logger.Error("chok: component failed; stopping", "error", err)
	}
	return errors.Join(runErr, a.stop(ctx, srv, cancelRun, &starters, comps, fast))
}

func callStart(ctx context.Context, s Starter) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("start panicked: %v", p)
		}
	}()
	return s.Start(ctx)
}

// awaitHealthy polls every Healther until all pass, a Start fails or
// app.start_timeout elapses.
func (a *App) awaitHealthy(ctx context.Context, comps []*component, failed <-chan error) error {
	timeout := a.base.App.StartTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for wait := 10 * time.Millisecond; ; wait = min(2*wait, 500*time.Millisecond) {
		select {
		case err := <-failed:
			return err
		default:
		}
		report := a.checkHealth(ctx, comps)
		if report.Status == statusUp {
			return nil
		}
		select {
		case err := <-failed:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("not healthy after app.start_timeout (%s): %s", timeout, report.failures())
		case <-time.After(wait):
		}
	}
}

func (a *App) server() *http.Server {
	h := a.base.HTTP
	srv := &http.Server{
		Handler:           a.Handler(),
		ReadTimeout:       h.ReadTimeout,
		ReadHeaderTimeout: h.ReadHeaderTimeout,
		WriteTimeout:      h.WriteTimeout,
		IdleTimeout:       h.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(a.logger.Handler(), slog.LevelWarn),
	}
	if h.H2C {
		var p http.Protocols
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(true)
		srv.Protocols = &p
	}
	return srv
}

func (a *App) stop(ctx context.Context, srv *http.Server, cancelRun context.CancelFunc, starters *sync.WaitGroup, comps []*component, fast bool) error {
	budget := a.base.App.ShutdownTimeout
	if budget <= 0 {
		budget = 30 * time.Second
	}
	if fast {
		budget = min(budget, 5*time.Second)
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	begin := time.Now()
	a.logger.Info("chok: stopping", "fast", fast, "budget", budget)
	a.ready.Store(false)
	a.draining.Store(true)

	var errs []error
	if srv != nil {
		if d := a.base.HTTP.DrainDelay; d > 0 && !fast {
			select {
			case <-time.After(d):
			case <-stopCtx.Done():
			}
		}
		timeout := a.base.HTTP.ShutdownTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		sctx, scancel := context.WithTimeout(stopCtx, timeout)
		if err := srv.Shutdown(sctx); err != nil {
			errs = append(errs, fmt.Errorf("chok: http shutdown: %w", err))
			_ = srv.Close()
		}
		scancel()
	}

	cancelRun()
	done := make(chan struct{})
	go func() { starters.Wait(); close(done) }()
	select {
	case <-done:
	case <-stopCtx.Done():
		errs = append(errs, fmt.Errorf("chok: components still running after app.shutdown_timeout (%s)", budget))
	}

	for _, comp := range slices.Backward(comps) {
		c, ok := comp.value.(Closer)
		if !ok {
			continue
		}
		if err := callClose(stopCtx, c); err != nil {
			errs = append(errs, fmt.Errorf("chok: close %s: %w", comp.name, err))
		}
	}
	a.logger.Info("chok: stopped", "duration", time.Since(begin).Round(time.Millisecond))
	return errors.Join(errs...)
}

func callClose(ctx context.Context, c Closer) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("close panicked: %v", p)
		}
	}()
	return c.Close(ctx)
}

func (a *App) closeLogs() {
	for _, c := range a.closers {
		_ = c.Close()
	}
}
