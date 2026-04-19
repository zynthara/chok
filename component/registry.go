package component

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zynthara/chok/log"
)

// Registry sequences the lifecycle of a set of Components according to
// their declared dependency graph. It is the engine behind the Component
// abstraction — see the package doc comment for context.
//
// Typical usage from App.Run:
//
//	reg := component.New(cfg, logger)
//	reg.Register(&LoggerComponent{})
//	reg.Register(&DBComponent{})
//	reg.Register(&AccountComponent{}) // Dependencies() = ["db", "log"]
//
//	if err := reg.Start(ctx); err != nil { return err }
//	defer reg.Stop(shutdownCtx)
//
// Registry itself implements Kernel, which is the view each Component
// sees inside its Init method. Registry is safe to use from a single
// goroutine — start/stop/reload must not be called concurrently. Get
// and Health may be called concurrently after Start.
type Registry struct {
	mu sync.RWMutex

	config         any
	configSnapshot atomic.Value // stores a deep copy of config after load/reload
	logger         log.Logger

	components []Component
	byName     map[string]Component
	events     map[Event][]Hook

	// startOrder records Components in the order they were Init'd
	// successfully. Stop walks this list in reverse; partial startup
	// failures still walk whatever made it in here.
	startOrder         []Component
	phase              phase
	stopTimeout        time.Duration // timeout for rollback Stop on Init/Migrate failure
	defaultInitTimeout time.Duration // per-component Init timeout (0 = no timeout)
	healthTimeout      time.Duration // per-probe Health timeout (0 = no timeout)

	// failed tracks optional components whose Init or Migrate failed.
	// Keyed by component name, value is the error message. These
	// components are removed from byName (Get returns nil) but
	// reported as HealthDown in Health() for visibility.
	failed map[string]string

	closeTimeout         time.Duration            // per-component Close timeout (0 = no timeout)
	defaultReloadTimeout time.Duration            // per-component Reload timeout (0 = no timeout)
	hookTimeout          time.Duration            // aggregate timeout for lifecycle hooks (0 = no timeout)
	initDurations        map[string]time.Duration // per-component Init duration (for debug)

	// available tracks components that have completed Init successfully.
	// During the Init phase (initInProgress=true), Get() only returns
	// components from this set, preventing a component in parallel Init
	// from accessing a peer that hasn't finished Init yet. Outside the
	// Init phase, Get() returns from byName directly.
	available      map[string]bool
	initInProgress bool

	// reloadMu serializes Reload and Stop so a custom Reloadable's
	// Reload method never runs concurrently with its own Close. The RW
	// mutex's R-side is unused — every lifecycle-dispatch path is a
	// writer. Kept as a Mutex for documentation: "hold exclusively
	// while dispatching user lifecycle methods".
	reloadMu sync.Mutex
}

type phase int

const (
	phaseBuild    phase = iota // registration only, not yet started
	phaseStarted               // Start completed (successfully or partially)
	phaseStopping              // Rollback or Stop in progress; Reload must bail out
	phaseStopped               // Stop completed
)

// New creates an empty Registry. config is the application's fully-loaded
// configuration structure; Components cast it to the expected concrete
// type via Kernel.Config(). logger is the shared framework logger.
func New(config any, logger log.Logger) *Registry {
	if logger == nil {
		logger = log.Empty()
	}
	return &Registry{
		config:        config,
		logger:        logger,
		byName:        make(map[string]Component),
		events:        make(map[Event][]Hook),
		failed:        make(map[string]string),
		available:     make(map[string]bool),
		initDurations: make(map[string]time.Duration),
		stopTimeout:   30 * time.Second,
	}
}

// SetStopTimeout overrides the timeout used when rolling back components
// after an Init or Migrate failure. Default is 30s.
func (r *Registry) SetStopTimeout(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopTimeout = d
}

// SetDefaultInitTimeout sets the per-component Init timeout. If a component
// implements InitTimeouter, its value takes precedence. Zero disables the
// timeout (the default).
func (r *Registry) SetDefaultInitTimeout(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultInitTimeout = d
}

// SetHealthTimeout sets the per-probe timeout for Health checks. Each
// Healther runs concurrently and is given at most this duration. Zero
// disables the timeout (the default).
func (r *Registry) SetHealthTimeout(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healthTimeout = d
}

// SetCloseTimeout sets the per-component Close timeout. When a
// component's Close exceeds this duration, the context is cancelled
// and the error is recorded, allowing the remaining components to
// proceed with their Close. If a component implements CloseTimeouter,
// its value takes precedence. Zero disables the timeout (the default).
func (r *Registry) SetCloseTimeout(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeTimeout = d
}

// SetHookTimeout sets the aggregate timeout for lifecycle hook
// execution. When set, EventBeforeStop and EventBeforeReload hooks
// are bounded by this duration — a slow hook cannot block the
// shutdown sequence past the Kubernetes terminationGracePeriod.
// Zero disables the timeout (the default).
func (r *Registry) SetHookTimeout(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hookTimeout = d
}

// SetReloadTimeout sets the per-component Reload timeout. When a
// component's Reload exceeds this duration, the context is cancelled
// and the error is recorded, allowing the remaining components to
// proceed with their Reload. If a component implements ReloadTimeouter,
// its value takes precedence. Zero disables the timeout (the default).
func (r *Registry) SetReloadTimeout(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultReloadTimeout = d
}

// Register adds a Component to the Registry. Components must be registered
// before Start is called; registration after Start panics. Duplicate names
// also panic — a name collision is a programming error that should fail
// at startup, not return an error upward.
func (r *Registry) Register(c Component) {
	if c == nil {
		panic("component: Register called with nil Component")
	}
	// Reject typed-nil: a *Comp held by a non-nil interface variable
	// still passes `c != nil` but method calls (Init/Close/etc.) will
	// segfault later. Catching this at Register makes the error land
	// where the registration happened rather than in Start.
	if rv := reflect.ValueOf(c); rv.Kind() == reflect.Ptr && rv.IsNil() {
		panic("component: Register called with typed-nil pointer")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.phase != phaseBuild {
		panic(fmt.Sprintf("component: Register(%q) called after Start", c.Name()))
	}
	name := c.Name()
	if name == "" {
		panic("component: Register called with empty Name()")
	}
	if _, dup := r.byName[name]; dup {
		panic(fmt.Sprintf("component: duplicate registration for %q", name))
	}
	r.components = append(r.components, c)
	r.byName[name] = c
}

// --- Kernel implementation -------------------------------------------------

// Config returns the application config that was passed to New.
func (r *Registry) Config() any { return r.config }

// ConfigSnapshot returns the last atomically-published config snapshot.
// The snapshot is a shallow copy of the top-level config struct — all
// value-type fields (string, int, bool, nested structs) are independent
// of the live config. Slice and map fields share backing storage with
// the live config; however, the framework's built-in Options types use
// only value fields, so this is safe for standard usage.
//
// Resolvers should prefer ConfigSnapshot() over Config() to avoid
// observing a partially-written struct during a concurrent Reload.
// Returns the live config if no snapshot has been published yet.
func (r *Registry) ConfigSnapshot() any {
	if snap := r.configSnapshot.Load(); snap != nil {
		return snap
	}
	return r.config
}

// PublishConfigSnapshot creates a shallow copy of the top-level config
// struct and stores it atomically. Called by App after loadConfig and
// after each successful ReloadConfig. Value-type fields are fully
// independent; slice/map fields share backing storage (acceptable for
// the framework's built-in Options which are value-only).
//
// The caller must hold a.configMu (or equivalent) while mutating config
// and calling this method to prevent torn reads during the copy.
func (r *Registry) PublishConfigSnapshot() {
	if r.config == nil {
		return
	}
	rv := reflect.ValueOf(r.config)
	if rv.Kind() == reflect.Ptr && !rv.IsNil() {
		// Shallow copy: allocate a new struct, copy top-level fields.
		cp := reflect.New(rv.Elem().Type())
		cp.Elem().Set(rv.Elem())
		r.configSnapshot.Store(cp.Interface())
	} else {
		r.configSnapshot.Store(r.config)
	}
}

// Logger returns the shared framework logger.
func (r *Registry) Logger() log.Logger { return r.logger }

// Get returns a registered Component by name, or nil if not registered.
//
// During the Init phase (initInProgress=true), Get only returns
// components that have already finished Init — preventing a parallel
// peer from touching a half-built sibling.
//
// During Stop (phase == phaseStopping/phaseStopped), Get only returns
// components still in the "available" set. Stop scrubs `available`
// entries as it Close's each component, so Get cannot hand back a
// reference to an already-torn-down peer. EventBeforeStop hooks fire
// before any Close runs, so they still observe the full set.
//
// Outside those windows (phaseBuild for inspection, phaseStarted for
// runtime), Get returns directly from byName.
//
// Safe to call from any goroutine.
func (r *Registry) Get(name string) Component {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := r.byName[name]
	if c == nil {
		return nil
	}
	if r.initInProgress && !r.available[name] {
		return nil
	}
	if (r.phase == phaseStopping || r.phase == phaseStopped) && !r.available[name] {
		return nil
	}
	return c
}

// On subscribes hook to the given event. Registered hooks fire in
// registration order when the event phase is emitted.
//
// Registration is permitted during phaseBuild (pre-Start) and
// phaseStarted (post-Start extensions like admin APIs that want to
// observe Reload/Stop). It is rejected in phaseStopping / phaseStopped
// with a panic — adding a hook to a registry that's already tearing
// down is almost always a bug (the hook would never fire for
// lifecycle events that already emitted, and the caller usually
// expects otherwise).
func (r *Registry) On(event Event, hook Hook) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.phase == phaseStopping || r.phase == phaseStopped {
		panic(fmt.Sprintf("component: On(%q) called after Stop", event))
	}
	r.events[event] = append(r.events[event], hook)
}

// --- Verification ----------------------------------------------------------

// Verify validates the dependency graph without initialising any
// components. It checks for: duplicate names, missing dependencies,
// self-dependencies, and cycles. Returns nil when the graph is valid.
//
// Intended for dev-time CLI checks (e.g. `chok verify`) so developers
// get fast feedback on wiring errors without waiting for real Init.
//
// Two checks are performed:
//  1. topoSort — validates the dependency graph (cycles, unknown deps,
//     self-deps).
//  2. DepsValidator — every component implementing ValidateDeps is
//     called with the Kernel so it can assert that declared runtime
//     prerequisites (env vars, paths, sibling component presence) are
//     in place. Errors from both phases are joined.
func (r *Registry) Verify() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, err := r.topoSort(); err != nil {
		return err
	}
	var errs []error
	for _, c := range r.components {
		if dv, ok := c.(DepsValidator); ok {
			if err := dv.ValidateDeps(r); err != nil {
				errs = append(errs, fmt.Errorf("component %q: %w", c.Name(), err))
			}
		}
	}
	return errors.Join(errs...)
}

// Order returns the component names in topological (init) order.
// Returns an error if the graph is invalid. Useful for diagnostics.
func (r *Registry) Order() ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	order, err := r.topoSort()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(order))
	for i, c := range order {
		names[i] = c.Name()
	}
	return names, nil
}

// StartOnly initialises only the named components and their transitive
// dependencies. Components not in the transitive closure are skipped.
// This is intended for integration tests that need a working subset of
// the application (e.g. "db" + "cache") without starting HTTP servers,
// auth modules, or other irrelevant components.
//
// The returned Registry behaves like a normal started Registry: Stop
// closes only the Init'd subset, Health/ReadyCheck operate on the
// started components, and Get returns nil for components outside the
// subset.
//
// Example:
//
//	reg := component.New(cfg, logger)
//	reg.Register(&DBComponent{...})
//	reg.Register(&CacheComponent{...})  // Dependencies: ["redis"]
//	reg.Register(&RedisComponent{...})
//	reg.Register(&HTTPComponent{...})
//
//	// Start only cache + its transitive deps (redis).
//	err := reg.StartOnly(ctx, "cache")
//	// → redis Init'd, cache Init'd; DB and HTTP untouched.
func (r *Registry) StartOnly(ctx context.Context, names ...string) error {
	r.mu.Lock()
	if r.phase != phaseBuild {
		r.mu.Unlock()
		return errors.New("component: Registry.StartOnly called after Start")
	}

	// Resolve the transitive dependency closure. We include both hard and
	// optional dependencies when they are registered — the full-graph
	// topoSort adds ordering edges for registered optional deps, so the
	// StartOnly subset must match that behaviour. Otherwise, calling
	// StartOnly(ctx, "cache") with Redis auto-registered would skip
	// Redis even though a normal Start would have started it.
	needed := make(map[string]bool, len(names))
	var resolve func(name string) error
	resolve = func(name string) error {
		if needed[name] {
			return nil
		}
		c, ok := r.byName[name]
		if !ok {
			return fmt.Errorf("component: StartOnly: unknown component %q", name)
		}
		needed[name] = true
		for _, dep := range dependenciesOf(c) {
			if err := resolve(dep); err != nil {
				return err
			}
		}
		for _, dep := range optionalDependenciesOf(c) {
			// Only include optional deps that are actually registered.
			// Missing optional deps are silently skipped — same contract
			// as full-graph Start.
			if _, known := r.byName[dep]; !known {
				continue
			}
			if err := resolve(dep); err != nil {
				return err
			}
		}
		return nil
	}
	for _, name := range names {
		if err := resolve(name); err != nil {
			r.mu.Unlock()
			return err
		}
	}

	// Filter registered components to only the needed subset, preserving
	// registration order for deterministic topo sort within levels.
	filtered := make([]Component, 0, len(needed))
	filteredByName := make(map[string]Component, len(needed))
	for _, c := range r.components {
		if needed[c.Name()] {
			filtered = append(filtered, c)
			filteredByName[c.Name()] = c
		}
	}
	r.components = filtered
	r.byName = filteredByName
	r.mu.Unlock()

	return r.Start(ctx)
}

// Components returns all registered components in registration order.
// Includes components that failed Init (optional skip). For only
// successfully-started components, use StartedComponents().
// Safe to call after Start from any goroutine.
func (r *Registry) Components() []Component {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Component, len(r.components))
	copy(out, r.components)
	return out
}

// StartedComponents returns only the components that successfully
// completed Init (and Migrate), in topological start order. Optional
// components that failed Init/Migrate are excluded. This is the
// correct source for Health probes and Router mounting — probing or
// mounting a component that never Init'd risks panics on nil state.
// Safe to call after Start from any goroutine.
func (r *Registry) StartedComponents() []Component {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Component, len(r.startOrder))
	copy(out, r.startOrder)
	return out
}

// --- Lifecycle -------------------------------------------------------------

// Start Init's every registered Component in dependency order. On any
// Init error, already-initialised Components are Close'd in reverse
// order and the error is returned. Start also runs Migrate for Components
// implementing Migratable, immediately after each Init.
//
// Events fired:
//   - EventBeforeStart before the first Init
//   - EventAfterStart  after every Init (and Migrate) succeeded
//
// Hook errors during EventBeforeStart abort Start without any Init;
// errors during EventAfterStart are joined and returned alongside the
// nominally-successful startup (Components stay running).
func (r *Registry) Start(ctx context.Context) error {
	startTime := time.Now()
	r.mu.Lock()
	if r.phase != phaseBuild {
		r.mu.Unlock()
		return errors.New("component: Registry.Start called twice")
	}
	levels, err := r.topoSortLeveled()
	if err != nil {
		r.mu.Unlock()
		return err
	}
	total := 0
	for _, lvl := range levels {
		total += len(lvl)
	}
	r.mu.Unlock()
	r.logger.Info("starting component registry", "count", total, "levels", len(levels))

	if err := r.emit(ctx, EventBeforeStart); err != nil {
		return fmt.Errorf("before_start hook failed: %w", err)
	}

	// Validate cross-component dependencies before any Init runs.
	if err := r.validateDeps(); err != nil {
		return err
	}

	// Mark Init phase so Get() only returns components that have
	// completed Init — prevents undeclared cross-level dependencies.
	r.mu.Lock()
	r.initInProgress = true
	r.mu.Unlock()

	for _, level := range levels {
		if len(level) == 1 {
			// Single-component level: sequential (common case, zero goroutine overhead).
			if err := r.initOne(ctx, level[0]); err != nil {
				return err
			}
		} else {
			// Multi-component level: init in parallel.
			if err := r.initLevel(ctx, level); err != nil {
				return err
			}
		}

		// Migrate runs sequentially after all Init in a level complete.
		// This preserves schema safety (parallel migrations risk conflicts).
		if err := r.migrateLevel(ctx, level); err != nil {
			return err
		}
	}

	r.mu.Lock()
	r.phase = phaseStarted
	r.initInProgress = false
	r.mu.Unlock()

	dur := time.Since(startTime)
	r.logger.Info("component registry started",
		"count", total,
		"duration", dur)
	afterCtx := WithPhaseResult(ctx, PhaseResult{Duration: dur})
	return r.emit(afterCtx, EventAfterStart)
}

// initOne initializes a single component. On failure it either skips
// (optional) or rolls back and returns an error.
func (r *Registry) initOne(ctx context.Context, c Component) error {
	initCtx, initCancel := r.initContext(ctx, c)
	defer initCancel()
	initStart := time.Now()

	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("component %q init panicked: %v", c.Name(), p)
			}
		}()
		err = c.Init(initCtx, r)
	}()
	if err != nil {
		r.logger.Error("component init failed",
			"component", c.Name(),
			"duration", time.Since(initStart),
			"error", err)
		if isOptional(c) {
			r.logger.Warn("optional component skipped", "component", c.Name())
			r.mu.Lock()
			delete(r.byName, c.Name())
			r.failed[c.Name()] = err.Error()
			r.mu.Unlock()
			return nil
		}
		return r.rollbackAndError(c.Name(), "init", err)
	}
	dur := time.Since(initStart)
	r.logger.Info("component initialized",
		"component", c.Name(),
		"duration", dur)
	r.mu.Lock()
	r.startOrder = append(r.startOrder, c)
	r.available[c.Name()] = true
	r.mu.Unlock()
	r.RecordInitDuration(c.Name(), dur)
	return nil
}

// initLevel initializes all components in a level concurrently. When a
// non-optional component fails, the level context is cancelled so peers
// don't waste time running to completion (or their timeout).
func (r *Registry) initLevel(ctx context.Context, level []Component) error {
	type initResult struct {
		c   Component
		err error
		dur time.Duration
	}

	levelCtx, levelCancel := context.WithCancel(ctx)
	defer levelCancel()

	results := make(chan initResult, len(level))
	for _, c := range level {
		go func(c Component) {
			initCtx, initCancel := r.initContext(levelCtx, c)
			defer initCancel()
			start := time.Now()
			var err error
			func() {
				defer func() {
					if p := recover(); p != nil {
						err = fmt.Errorf("component %q init panicked: %v", c.Name(), p)
					}
				}()
				err = c.Init(initCtx, r)
			}()
			results <- initResult{c: c, err: err, dur: time.Since(start)}
		}(c)
	}

	// Collect results into a map keyed by name, then rebuild the
	// started list in the original level order so StartedComponents()
	// returns a deterministic sequence regardless of Init duration.
	succeeded := make(map[string]bool, len(level))
	var failed bool
	var failName string
	var failErr error
	for range level {
		res := <-results
		if res.err != nil {
			r.logger.Error("component init failed",
				"component", res.c.Name(),
				"duration", res.dur,
				"error", res.err)
			if isOptional(res.c) {
				r.logger.Warn("optional component skipped", "component", res.c.Name())
				r.mu.Lock()
				delete(r.byName, res.c.Name())
				r.failed[res.c.Name()] = res.err.Error()
				r.mu.Unlock()
				continue
			}
			if !failed {
				// First non-optional failure: cancel peer goroutines.
				failed = true
				failName = res.c.Name()
				failErr = res.err
				levelCancel()
			}
			continue
		}
		r.logger.Info("component initialized",
			"component", res.c.Name(),
			"duration", res.dur)
		r.RecordInitDuration(res.c.Name(), res.dur)
		succeeded[res.c.Name()] = true
	}

	// Rebuild started slice in the original level order (registration
	// order within the topo level) so that StartedComponents() —and
	// therefore Router mount order— is deterministic.
	started := make([]Component, 0, len(succeeded))
	for _, c := range level {
		if succeeded[c.Name()] {
			started = append(started, c)
		}
	}

	r.mu.Lock()
	r.startOrder = append(r.startOrder, started...)
	for _, c := range started {
		r.available[c.Name()] = true
	}
	r.mu.Unlock()

	if failed {
		return r.rollbackAndError(failName, "init", failErr)
	}
	return nil
}

// migrateLevel runs Migrate sequentially for all successfully-Init'd
// Migratable components in the level. Only components still in startOrder
// (not skipped optional) are migrated.
func (r *Registry) migrateLevel(ctx context.Context, level []Component) error {
	for _, c := range level {
		m, ok := c.(Migratable)
		if !ok {
			continue
		}
		// Skip components that were removed from byName (optional failures).
		r.mu.RLock()
		_, alive := r.byName[c.Name()]
		r.mu.RUnlock()
		if !alive {
			continue
		}

		initCtx, initCancel := r.initContext(ctx, c)
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("component %q migrate panicked: %v", c.Name(), p)
				}
			}()
			err = m.Migrate(initCtx)
		}()
		if err != nil {
			initCancel()
			if isOptional(c) {
				r.logger.Warn("optional component migrate failed, skipping",
					"component", c.Name(), "error", err)
				closeCtx, closeCancel := r.closeContext(ctx, c)
				if cerr := c.Close(closeCtx); cerr != nil {
					r.logger.Warn("optional component close after migrate failure",
						"component", c.Name(), "error", cerr)
				}
				closeCancel()
				r.removeFromStartOrder(c.Name(), err.Error())
				continue
			}
			return r.rollbackAndError(c.Name(), "migrate", err)
		}
		initCancel()
	}
	return nil
}

// rollbackAndError marks the registry as stopping, Close's every
// successfully-started component in reverse order, and returns a
// formatted error.
//
// The phase is set to phaseStopping (not phaseStarted) so concurrent
// Reload triggers (SIGHUP, fsnotify) observe the transition and bail
// out rather than dispatching Reload onto components currently being
// Closed. Stop itself accepts both phaseStarted and phaseStopping.
func (r *Registry) rollbackAndError(name, phase string, err error) error {
	r.mu.Lock()
	r.phase = phaseStopping
	r.initInProgress = false
	r.mu.Unlock()
	stopCtx, cancel := context.WithTimeout(context.Background(), r.stopTimeout)
	_ = r.Stop(stopCtx)
	cancel()
	return fmt.Errorf("component %q %s: %w", name, phase, err)
}

// Stop Close's every successfully-started Component in reverse
// dependency order. Components at the same topological level are
// Close'd in parallel (mirroring the parallel Init pattern in Start);
// levels are processed in reverse order so dependents close before
// their dependencies.
//
// Events fired:
//   - EventBeforeStop before the first Close
//   - EventAfterStop  after every Close returns
//
// Safe to call even after a partial Start failure; only the components
// that successfully Init'd get Close'd. Safe to call multiple times
// (subsequent calls are no-ops).
func (r *Registry) Stop(ctx context.Context) error {
	stopStart := time.Now()
	r.mu.Lock()
	// Accept both phaseStarted (normal shutdown) and phaseStopping
	// (rollback path set by rollbackAndError). Everything else is a
	// no-op — in particular phaseStopped means Stop already ran.
	if r.phase != phaseStarted && r.phase != phaseStopping {
		r.mu.Unlock()
		return nil
	}
	toClose := make([]Component, len(r.startOrder))
	copy(toClose, r.startOrder)
	r.startOrder = nil
	// Transition to phaseStopping (not phaseStopped) so EventBeforeStop
	// hooks and any concurrent observers can distinguish "draining" from
	// "fully stopped". phaseStopped is set at the end once every Close
	// has run. Reload and ReadyCheck both reject non-phaseStarted, which
	// still applies here.
	r.phase = phaseStopping
	r.mu.Unlock()

	r.logger.Info("stopping component registry", "count", len(toClose))

	// EventBeforeStop runs WITHOUT reloadMu so a hook that invokes
	// Reload/Stop (e.g. an admin hook that chains shutdown actions)
	// doesn't deadlock on the non-reentrant mutex.
	beforeErr := r.emit(ctx, EventBeforeStop)

	var errs []error
	if beforeErr != nil {
		errs = append(errs, fmt.Errorf("before_stop: %w", beforeErr))
	}

	// Close phase holds reloadMu exclusively so a concurrent Reload waits
	// for Close to finish before dispatching rl.Reload on an already
	// being-closed component. The lock does NOT wrap the emit calls
	// above/below — hooks are intentionally left re-entry-safe.
	func() {
		r.reloadMu.Lock()
		defer r.reloadMu.Unlock()

		// Build reverse-topological levels from the started components.
		levels := r.buildCloseLevels(toClose)
		for i := len(levels) - 1; i >= 0; i-- {
			level := levels[i]
			if len(level) == 1 {
				// Single-component level: sequential (common case, zero goroutine overhead).
				c := level[0]
				closeCtx, closeCancel := r.closeContext(ctx, c)
				closeStart := time.Now()
				var closeErr error
				func() {
					defer func() {
						if p := recover(); p != nil {
							closeErr = fmt.Errorf("component %q close panicked: %v", c.Name(), p)
						}
					}()
					closeErr = c.Close(closeCtx)
				}()
				closeCancel() // cancel immediately, not deferred to Stop return
				r.markUnavailable(c.Name())
				if closeErr != nil {
					r.logger.Error("component close failed",
						"component", c.Name(),
						"duration", time.Since(closeStart),
						"error", closeErr)
					errs = append(errs, fmt.Errorf("component %q close: %w", c.Name(), closeErr))
				}
			} else {
				// Multi-component level: close in parallel.
				levelErrs := r.closeLevel(ctx, level)
				errs = append(errs, levelErrs...)
			}
		}
	}()

	// Every Close has now run. Move to phaseStopped so any late-arriving
	// Reload/ReadyCheck/Start call sees the terminal state, and emit
	// AfterStop hooks with the stable "fully stopped" view.
	r.mu.Lock()
	r.phase = phaseStopped
	r.mu.Unlock()

	stopDur := time.Since(stopStart)
	stopErr := errors.Join(errs...)
	afterCtx := WithPhaseResult(ctx, PhaseResult{Duration: stopDur, Err: stopErr})
	if afterErr := r.emit(afterCtx, EventAfterStop); afterErr != nil {
		errs = append(errs, fmt.Errorf("after_stop: %w", afterErr))
	}

	r.logger.Info("component registry stopped", "duration", stopDur)
	return errors.Join(errs...)
}

// closeLevel closes all components in a level concurrently.
func (r *Registry) closeLevel(ctx context.Context, level []Component) []error {
	type closeResult struct {
		name string
		err  error
	}
	results := make(chan closeResult, len(level))
	for _, c := range level {
		go func(c Component) {
			closeCtx, closeCancel := r.closeContext(ctx, c)
			defer closeCancel()
			start := time.Now()
			var err error
			func() {
				defer func() {
					if p := recover(); p != nil {
						err = fmt.Errorf("component %q close panicked: %v", c.Name(), p)
					}
				}()
				err = c.Close(closeCtx)
			}()
			r.markUnavailable(c.Name())
			if err != nil {
				r.logger.Error("component close failed",
					"component", c.Name(),
					"duration", time.Since(start),
					"error", err)
			}
			results <- closeResult{name: c.Name(), err: err}
		}(c)
	}
	var errs []error
	for range level {
		res := <-results
		if res.err != nil {
			errs = append(errs, fmt.Errorf("component %q close: %w", res.name, res.err))
		}
	}
	return errs
}

// buildCloseLevels groups started components into topological levels
// using the same Kahn's algorithm as Init but restricted to the set of
// actually-started components. Components that started but aren't in
// the dependency graph (no deps declared) end up at level 0.
func (r *Registry) buildCloseLevels(started []Component) [][]Component {
	if len(started) == 0 {
		return nil
	}

	// Build a set of started component names for fast lookup.
	startedSet := make(map[string]bool, len(started))
	byName := make(map[string]Component, len(started))
	regOrder := make(map[string]int, len(started))
	for i, c := range started {
		startedSet[c.Name()] = true
		byName[c.Name()] = c
		regOrder[c.Name()] = i
	}

	// Build in-degree map using only started components.
	inDeg := make(map[string]int, len(started))
	dependents := make(map[string][]string, len(started))
	for _, c := range started {
		inDeg[c.Name()] = 0
	}
	for _, c := range started {
		edgeSeen := make(map[string]bool)
		for _, dep := range dependenciesOf(c) {
			if !startedSet[dep] {
				continue // dependency didn't start (optional skip, etc.)
			}
			if edgeSeen[dep] {
				continue
			}
			edgeSeen[dep] = true
			inDeg[c.Name()]++
			dependents[dep] = append(dependents[dep], c.Name())
		}
	}

	// Kahn's algorithm.
	var roots []string
	for name, deg := range inDeg {
		if deg == 0 {
			roots = append(roots, name)
		}
	}
	sort.Slice(roots, func(i, j int) bool {
		return regOrder[roots[i]] < regOrder[roots[j]]
	})

	var levels [][]Component
	for len(roots) > 0 {
		level := make([]Component, len(roots))
		for i, name := range roots {
			level[i] = byName[name]
		}
		levels = append(levels, level)

		var nextRoots []string
		for _, name := range roots {
			for _, child := range dependents[name] {
				inDeg[child]--
				if inDeg[child] == 0 {
					nextRoots = append(nextRoots, child)
				}
			}
		}
		sort.Slice(nextRoots, func(i, j int) bool {
			return regOrder[nextRoots[i]] < regOrder[nextRoots[j]]
		})
		roots = nextRoots
	}

	return levels
}

// ReloadOption configures a Reload call.
type ReloadOption func(*reloadConfig)

type reloadConfig struct {
	configChanged   *bool           // nil = unknown (default to true)
	changedSections map[string]bool // per-section diff (nil = unknown)
}

// WithConfigChanged marks whether the config file actually changed during
// this reload cycle. Components can check component.ConfigChanged(ctx) to
// skip expensive re-initialization when no config change occurred.
func WithReloadConfigChanged(changed bool) ReloadOption {
	return func(rc *reloadConfig) { rc.configChanged = &changed }
}

// WithReloadChangedSections passes the per-section diff map so
// components can check SectionChanged(ctx, key) to skip work when
// their specific config section didn't change.
func WithReloadChangedSections(sections map[string]bool) ReloadOption {
	return func(rc *reloadConfig) { rc.changedSections = sections }
}

// Reload calls Reload on every Reloadable Component in dependency order.
// Errors are collected (not short-circuited) and returned joined — a
// single component failing to absorb new config should not prevent its
// peers from trying.
//
// Events fired:
//   - EventBeforeReload before the first Reload
//   - EventAfterReload  after every Reload returns
func (r *Registry) Reload(ctx context.Context, opts ...ReloadOption) error {
	reloadStart := time.Now()
	r.mu.RLock()
	if r.phase != phaseStarted {
		r.mu.RUnlock()
		return errors.New("component: Reload requires Start to have run")
	}
	order := make([]Component, len(r.startOrder))
	copy(order, r.startOrder)
	r.mu.RUnlock()

	// Apply reload options.
	rc := &reloadConfig{}
	for _, o := range opts {
		o(rc)
	}
	if rc.configChanged != nil {
		ctx = WithConfigChanged(ctx, *rc.configChanged)
	}
	if rc.changedSections != nil {
		ctx = WithChangedSections(ctx, rc.changedSections)
	}

	r.logger.Info("reloading components")

	// EventBeforeReload runs WITHOUT reloadMu so a hook that itself
	// invokes Reload/Stop (admin APIs daisy-chaining configuration
	// changes) doesn't deadlock on the non-reentrant mutex.
	var errs []error
	if beforeErr := r.emit(ctx, EventBeforeReload); beforeErr != nil {
		errs = append(errs, fmt.Errorf("before_reload: %w", beforeErr))
	}

	// reloadMu only wraps the per-component dispatch — the section that
	// must not race with Close. Hooks run outside so they remain safe
	// re-entry points for Reload/Stop.
	func() {
		r.reloadMu.Lock()
		defer r.reloadMu.Unlock()

		for _, c := range order {
			// Guard against concurrent Stop: if the registry is no longer
			// running, abort the reload loop to avoid calling Reload on
			// components that are being or have been Close'd.
			r.mu.RLock()
			stopped := r.phase != phaseStarted
			r.mu.RUnlock()
			if stopped {
				errs = append(errs, errors.New("component: Reload aborted: registry no longer running"))
				break
			}

			rl, ok := c.(Reloadable)
			if !ok {
				continue
			}
			reloadCtx, reloadCancel := r.reloadContext(ctx, c)
			var err error
			func() {
				defer func() {
					if p := recover(); p != nil {
						err = fmt.Errorf("component %q reload panicked: %v", c.Name(), p)
					}
				}()
				err = rl.Reload(reloadCtx)
			}()
			if err != nil {
				r.logger.Error("component reload failed",
					"component", c.Name(), "error", err)
				errs = append(errs, fmt.Errorf("component %q reload: %w", c.Name(), err))
			}
			reloadCancel()
		}
	}()

	reloadDur := time.Since(reloadStart)
	reloadErr := errors.Join(errs...)
	afterCtx := WithPhaseResult(ctx, PhaseResult{Duration: reloadDur, Err: reloadErr})
	if afterErr := r.emit(afterCtx, EventAfterReload); afterErr != nil {
		errs = append(errs, fmt.Errorf("after_reload: %w", afterErr))
	}

	r.logger.Info("component reload complete", "duration", reloadDur)
	return errors.Join(errs...)
}

// Health polls every Healther Component and aggregates the results into
// a report. Probes run in parallel with a per-probe timeout (see
// SetHealthTimeout). Non-Healther components default to HealthOK. The
// aggregate Status follows the rule:
//
//	any component down      → HealthDown
//	none down, any degraded → HealthDegraded
//	otherwise               → HealthOK
//
// Safe to call concurrently with application traffic; does not take
// the write lock.
func (r *Registry) Health(ctx context.Context) HealthReport {
	r.mu.RLock()
	// Use startOrder (only successfully Init'd components), not the
	// full r.components list. Probing a component that failed Init
	// (optional skip) risks panics on nil state — those are reported
	// separately from the failed map below.
	live := make([]Component, len(r.startOrder))
	copy(live, r.startOrder)
	timeout := r.healthTimeout
	r.mu.RUnlock()

	report := HealthReport{
		Status:     HealthOK,
		Components: make(map[string]HealthStatus, len(live)),
	}

	type result struct {
		name   string
		status HealthStatus
	}

	// Count how many probes we expect, then collect results with a
	// hard deadline. Even if a probe ignores its context and blocks
	// forever, the fan-in loop below will return after the timeout
	// instead of hanging the entire /healthz handler.
	//
	// All probe goroutines derive from probeGroupCtx. When Health()
	// returns (either normally or via fan-in timeout), the deferred
	// cancel cascades to every still-running probe, preventing
	// goroutine leaks from rogue probes that ignore their per-probe
	// timeout.
	probeGroupCtx, probeGroupCancel := context.WithCancel(ctx)
	defer probeGroupCancel()

	var expected int
	ch := make(chan result, len(live))
	pending := make(map[string]struct{}, len(live))

	for _, c := range live {
		h, ok := c.(Healther)
		if !ok {
			report.Components[c.Name()] = HealthStatus{Status: HealthOK}
			continue
		}
		expected++
		pending[c.Name()] = struct{}{}
		go func(name string, h Healther) {
			probeCtx := probeGroupCtx
			var cancel context.CancelFunc
			if timeout > 0 {
				probeCtx, cancel = context.WithTimeout(probeGroupCtx, timeout)
				defer cancel()
			}
			var status HealthStatus
			func() {
				defer func() {
					if p := recover(); p != nil {
						status = HealthStatus{
							Status: HealthDown,
							Error:  fmt.Sprintf("health probe panicked: %v", p),
						}
					}
				}()
				status = h.Health(probeCtx)
			}()
			if status.Status == "" {
				status.Status = HealthOK
			}
			ch <- result{name: name, status: status}
		}(c.Name(), h)
	}

	// Collect results. When a per-probe timeout is configured, enforce
	// a hard fan-in deadline so a rogue probe that ignores its context
	// cannot block the aggregator. The deadline is timeout + 1s headroom
	// to give well-behaved probes time to return after their ctx expires.
	//
	// Use time.NewTimer (not time.After) so the backing runtime timer is
	// released when Health returns on the fast path. Under K8s liveness
	// probing this endpoint is hit every second; time.After would leak
	// one timer per call until the full window elapsed.
	var fanInTimer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout + time.Second)
		defer t.Stop()
		fanInTimer = t.C
	}

	var anyDegraded, anyDown bool
	collected := 0
	for collected < expected {
		select {
		case res := <-ch:
			collected++
			delete(pending, res.name)
			report.Components[res.name] = res.status
			switch res.status.Status {
			case HealthDown:
				anyDown = true
			case HealthDegraded:
				anyDegraded = true
			}
		case <-fanInTimer:
			// Hard deadline reached — report timed-out probes individually.
			for name := range pending {
				report.Components[name] = HealthStatus{
					Status: HealthDown,
					Error:  "health probe timeout",
				}
			}
			anyDown = true
			collected = expected // break the loop
		}
	}

	// Report optional components that failed Init/Migrate as degraded
	// (not down). They are optional precisely because the application
	// can serve without them — marking them down would make /readyz
	// return 503, contradicting the "optional init failure doesn't
	// block startup" contract.
	r.mu.RLock()
	for name, errMsg := range r.failed {
		report.Components[name] = HealthStatus{
			Status: HealthDegraded,
			Error:  errMsg,
		}
		anyDegraded = true
	}
	r.mu.RUnlock()

	switch {
	case anyDown:
		report.Status = HealthDown
	case anyDegraded:
		report.Status = HealthDegraded
	}
	return report
}

// --- Debug info ------------------------------------------------------------

// ComponentInfo describes a registered component for debug/diagnostics.
type ComponentInfo struct {
	Name                 string   `json:"name"`
	Level                int      `json:"level"`
	Dependencies         []string `json:"dependencies,omitempty"`
	OptionalDependencies []string `json:"optional_dependencies,omitempty"`
	Capabilities         []string `json:"capabilities"`
	InitDurationMs       int64    `json:"init_duration_ms,omitempty"`
}

// DebugInfo returns a structured snapshot of the registry state for
// diagnostics. Includes component topology, capabilities, init timing,
// and failed optional components. Intended for /componentz debug
// endpoints — disabled in production by default.
func (r *Registry) DebugInfo() map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var phaseStr string
	switch r.phase {
	case phaseBuild:
		phaseStr = "build"
	case phaseStarted:
		phaseStr = "started"
	case phaseStopping:
		phaseStr = "stopping"
	case phaseStopped:
		phaseStr = "stopped"
	}

	// Build level map from topo sort.
	levelMap := make(map[string]int)
	if levels, err := r.topoSortLeveled(); err == nil {
		for lvl, comps := range levels {
			for _, c := range comps {
				levelMap[c.Name()] = lvl
			}
		}
	}

	infos := make([]ComponentInfo, 0, len(r.components))
	for _, c := range r.components {
		info := ComponentInfo{
			Name:                 c.Name(),
			Level:                levelMap[c.Name()],
			Dependencies:         dependenciesOf(c),
			OptionalDependencies: optionalDependenciesOf(c),
		}
		// Discover capabilities via type assertions.
		if _, ok := c.(Reloadable); ok {
			info.Capabilities = append(info.Capabilities, "reloadable")
		}
		if _, ok := c.(Healther); ok {
			info.Capabilities = append(info.Capabilities, "healther")
		}
		if _, ok := c.(Router); ok {
			info.Capabilities = append(info.Capabilities, "router")
		}
		if _, ok := c.(Migratable); ok {
			info.Capabilities = append(info.Capabilities, "migratable")
		}
		if _, ok := c.(ReadyChecker); ok {
			info.Capabilities = append(info.Capabilities, "ready_checker")
		}
		if isOptional(c) {
			info.Capabilities = append(info.Capabilities, "optional")
		}
		if len(info.Capabilities) == 0 {
			info.Capabilities = []string{} // ensure JSON array, not null
		}
		if d, ok := r.initDurations[c.Name()]; ok {
			info.InitDurationMs = d.Milliseconds()
		}
		infos = append(infos, info)
	}

	result := map[string]any{
		"phase":      phaseStr,
		"components": infos,
	}
	if len(r.failed) > 0 {
		result["failed_optional"] = r.failed
	}
	return result
}

// RecordInitDuration stores the Init duration for a component so it
// can be reported via DebugInfo. Called by the registry itself during
// Start — not intended for external use.
func (r *Registry) RecordInitDuration(name string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initDurations[name] = d
}

// removeFromStartOrder drops a component from startOrder/byName/available
// and records its failure reason under failed. Called when an optional
// component fails Migrate after a successful Init — we keep it in the
// registry for Health reporting but stop it from participating in
// rollback or reload.
func (r *Registry) removeFromStartOrder(name, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, sc := range r.startOrder {
		if sc.Name() == name {
			r.startOrder = append(r.startOrder[:i], r.startOrder[i+1:]...)
			break
		}
	}
	delete(r.byName, name)
	delete(r.available, name)
	r.failed[name] = reason
}

// markUnavailable removes a component from `available` so concurrent
// Get() callers stop seeing the reference once Close has run. byName is
// retained because Health reporting and AfterStop hooks still need to
// enumerate names. Called by Stop after each Close completes.
func (r *Registry) markUnavailable(name string) {
	r.mu.Lock()
	delete(r.available, name)
	r.mu.Unlock()
}

// --- Topological sort ------------------------------------------------------

// topoSort returns Components in a flat order that respects Dependencies.
// Delegates to topoSortLeveled and flattens the result.
func (r *Registry) topoSort() ([]Component, error) {
	levels, err := r.topoSortLeveled()
	if err != nil {
		return nil, err
	}
	var flat []Component
	for _, lvl := range levels {
		flat = append(flat, lvl...)
	}
	return flat, nil
}

// topoSortLeveled returns Components grouped by dependency level.
// Components within the same level have no mutual dependencies and
// can be Init'd in parallel. It uses Kahn's algorithm: all nodes
// with in-degree zero form one level; removing their edges reveals
// the next level, and so on. Cycles and dangling references surface
// as errors.
//
// Ties within a level are broken by original registration order,
// so the output is deterministic.
func (r *Registry) topoSortLeveled() ([][]Component, error) {
	inDeg := make(map[string]int, len(r.components))
	dependents := make(map[string][]string, len(r.components))
	regOrder := make(map[string]int, len(r.components))

	for i, c := range r.components {
		regOrder[c.Name()] = i
		inDeg[c.Name()] = 0
	}

	for _, c := range r.components {
		// Deduplicate edges: a duplicate dependency (or overlap between
		// hard and optional deps) would inflate in-degree, potentially
		// causing a component to appear multiple times in a level.
		edgeSeen := make(map[string]bool)
		deps := dependenciesOf(c)
		for _, dep := range deps {
			if _, known := r.byName[dep]; !known {
				return nil, fmt.Errorf("component %q depends on unregistered %q", c.Name(), dep)
			}
			if dep == c.Name() {
				return nil, fmt.Errorf("component %q self-dependency", c.Name())
			}
			if edgeSeen[dep] {
				continue
			}
			edgeSeen[dep] = true
			inDeg[c.Name()]++
			dependents[dep] = append(dependents[dep], c.Name())
		}
		// Optional dependencies add ordering edges only when the dep is
		// registered; missing optional deps are silently skipped.
		for _, dep := range optionalDependenciesOf(c) {
			if _, known := r.byName[dep]; !known {
				continue // not registered, skip
			}
			if dep == c.Name() {
				continue
			}
			if edgeSeen[dep] {
				continue
			}
			edgeSeen[dep] = true
			inDeg[c.Name()]++
			dependents[dep] = append(dependents[dep], c.Name())
		}
	}

	// Initial roots, sorted by registration order.
	var roots []string
	for name, deg := range inDeg {
		if deg == 0 {
			roots = append(roots, name)
		}
	}
	sort.Slice(roots, func(i, j int) bool {
		return regOrder[roots[i]] < regOrder[roots[j]]
	})

	var levels [][]Component
	total := 0
	for len(roots) > 0 {
		// All current roots form one level.
		level := make([]Component, len(roots))
		for i, name := range roots {
			level[i] = r.byName[name]
		}
		levels = append(levels, level)
		total += len(level)

		// Relax edges and collect next level's roots.
		var nextRoots []string
		for _, name := range roots {
			for _, child := range dependents[name] {
				inDeg[child]--
				if inDeg[child] == 0 {
					nextRoots = append(nextRoots, child)
				}
			}
		}
		sort.Slice(nextRoots, func(i, j int) bool {
			return regOrder[nextRoots[i]] < regOrder[nextRoots[j]]
		})
		roots = nextRoots
	}

	if total != len(r.components) {
		var stuck []string
		for name, deg := range inDeg {
			if deg > 0 {
				stuck = append(stuck, name)
			}
		}
		sort.Strings(stuck)

		// Attempt to extract an actual cycle path via DFS for a clearer
		// error message (e.g. "a → b → c → a" instead of just "[a b c]").
		if path := r.findCyclePath(stuck); len(path) > 0 {
			return nil, fmt.Errorf("component: dependency cycle: %s", strings.Join(path, " → "))
		}
		return nil, fmt.Errorf("component: dependency cycle among %v", stuck)
	}

	return levels, nil
}

// findCyclePath uses DFS to extract one cycle path from the set of stuck
// components (those with non-zero in-degree after Kahn's algorithm). Returns
// a slice like ["a", "b", "c", "a"] that can be joined with " → ".
func (r *Registry) findCyclePath(stuck []string) []string {
	stuckSet := make(map[string]bool, len(stuck))
	for _, s := range stuck {
		stuckSet[s] = true
	}

	// Build adjacency: node → its dependencies (restricted to stuck nodes).
	// Deduplicate so overlapping hard/optional deps don't cause redundant DFS edges.
	deps := make(map[string][]string, len(stuck))
	for _, s := range stuck {
		c := r.byName[s]
		seen := make(map[string]bool)
		for _, d := range dependenciesOf(c) {
			if stuckSet[d] && !seen[d] {
				seen[d] = true
				deps[s] = append(deps[s], d)
			}
		}
		for _, d := range optionalDependenciesOf(c) {
			if stuckSet[d] && !seen[d] {
				seen[d] = true
				deps[s] = append(deps[s], d)
			}
		}
	}

	visited := make(map[string]bool, len(stuck))
	inStack := make(map[string]bool, len(stuck))
	var path []string

	var dfs func(node string) []string
	dfs = func(node string) []string {
		visited[node] = true
		inStack[node] = true
		path = append(path, node)

		for _, dep := range deps[node] {
			if !visited[dep] {
				if result := dfs(dep); result != nil {
					return result
				}
			} else if inStack[dep] {
				// Found cycle — extract the cycle portion and close the loop.
				for i, p := range path {
					if p == dep {
						cycle := make([]string, len(path[i:])+1)
						copy(cycle, path[i:])
						cycle[len(cycle)-1] = dep
						return cycle
					}
				}
			}
		}

		path = path[:len(path)-1]
		inStack[node] = false
		return nil
	}

	for _, s := range stuck {
		if !visited[s] {
			if cycle := dfs(s); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// dependenciesOf extracts the declared dependency list, or nil for
// Components that don't implement Dependent.
func dependenciesOf(c Component) []string {
	if d, ok := c.(Dependent); ok {
		return d.Dependencies()
	}
	return nil
}

// optionalDependenciesOf extracts the declared optional dependency list,
// or nil for Components that don't implement OptionalDependent.
func optionalDependenciesOf(c Component) []string {
	if d, ok := c.(OptionalDependent); ok {
		return d.OptionalDependencies()
	}
	return nil
}

// isOptional returns true if c implements Optionaler and returns true.
func isOptional(c Component) bool {
	if o, ok := c.(Optionaler); ok {
		return o.Optional()
	}
	return false
}

// --- Dependency validation ---------------------------------------------------

// validateDeps calls ValidateDeps on every component that implements
// DepsValidator. This runs after topo sort but before any Init, catching
// "account enabled but db not configured" errors early. All errors are
// collected and returned joined.
//
// User implementations are wrapped in defer-recover so a panic in one
// validator surfaces as a structured error rather than crashing the
// startup path. Init / Migrate / Reload / Health all do the same — keep
// every user-supplied callback equally fault-tolerant.
func (r *Registry) validateDeps() error {
	r.mu.RLock()
	cs := make([]Component, len(r.components))
	copy(cs, r.components)
	r.mu.RUnlock()

	var errs []error
	for _, c := range cs {
		v, ok := c.(DepsValidator)
		if !ok {
			continue
		}
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("validate deps panicked: %v", p)
				}
			}()
			err = v.ValidateDeps(r)
		}()
		if err != nil {
			errs = append(errs, fmt.Errorf("component %q: %w", c.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// --- Init timeout ---------------------------------------------------------

// initContext returns ctx wrapped with a per-component timeout when
// applicable. Priority: InitTimeouter on the component, then the
// registry's defaultInitTimeout. Zero means no timeout.
func (r *Registry) initContext(ctx context.Context, c Component) (context.Context, context.CancelFunc) {
	d := r.defaultInitTimeout
	if it, ok := c.(InitTimeouter); ok {
		d = it.InitTimeout()
	}
	if d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return ctx, func() {}
}

// reloadContext returns ctx wrapped with a per-component Reload timeout.
// Priority: ReloadTimeouter on the component, then the registry's
// defaultReloadTimeout. Zero means no timeout (use parent ctx as-is).
func (r *Registry) reloadContext(ctx context.Context, c Component) (context.Context, context.CancelFunc) {
	d := r.defaultReloadTimeout
	if rt, ok := c.(ReloadTimeouter); ok {
		d = rt.ReloadTimeout()
	}
	if d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return ctx, func() {}
}

// closeContext returns ctx wrapped with a per-component Close timeout.
// Priority: CloseTimeouter on the component, then the registry's
// closeTimeout. Zero means no timeout (use parent ctx as-is).
func (r *Registry) closeContext(ctx context.Context, c Component) (context.Context, context.CancelFunc) {
	d := r.closeTimeout
	if ct, ok := c.(CloseTimeouter); ok {
		d = ct.CloseTimeout()
	}
	if d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return ctx, func() {}
}

// --- Readiness ------------------------------------------------------------

// ReadyCheck polls every ReadyChecker Component sequentially and returns
// the first non-nil error. Intended for the /readyz endpoint to
// distinguish "started but warming up" from "fully ready to serve
// traffic".
//
// Each check is bounded by the health timeout to prevent a hung
// ReadyChecker from blocking the /readyz endpoint indefinitely. Before
// each iteration, the caller's context is also checked — if the
// /readyz handler is using a bounded ctx (as the HealthComponent does),
// hitting that deadline aborts the remaining probes instead of letting
// total latency grow with the component count.
//
// Components that don't implement ReadyChecker are assumed ready.
// Safe to call concurrently after Start.
func (r *Registry) ReadyCheck(ctx context.Context) error {
	r.mu.RLock()
	if r.phase != phaseStarted {
		r.mu.RUnlock()
		return errors.New("component: registry not running")
	}
	order := make([]Component, len(r.startOrder))
	copy(order, r.startOrder)
	timeout := r.healthTimeout
	r.mu.RUnlock()

	for _, c := range order {
		// Respect the caller's deadline — abort before probing the next
		// component if ctx is already done.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("component: ready-check aborted: %w", err)
		}
		// Re-check phase on every iteration so a concurrent Stop doesn't
		// keep feeding ReadyCheck calls to components that are being or
		// have already been Close'd.
		r.mu.RLock()
		running := r.phase == phaseStarted
		r.mu.RUnlock()
		if !running {
			return errors.New("component: registry stopping")
		}
		rc, ok := c.(ReadyChecker)
		if !ok {
			continue
		}
		checkCtx, cancel := ctx, context.CancelFunc(func() {})
		if timeout > 0 {
			checkCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("component %q ready-check panicked: %v", c.Name(), p)
				}
			}()
			err = rc.ReadyCheck(checkCtx)
		}()
		cancel()
		if err != nil {
			return fmt.Errorf("component %q not ready: %w", c.Name(), err)
		}
	}
	return nil
}

// --- Events ---------------------------------------------------------------

// emit fires every hook registered for event. It does not short-circuit:
// every hook sees the event regardless of earlier errors. All errors are
// collected and returned via errors.Join.
//
// The event name is injected into ctx via WithEvent so hooks can
// introspect which phase triggered them. Any existing reason (from
// WithReason) is preserved.
//
// When hookTimeout is configured, critical events (BeforeStop,
// BeforeReload) are bounded so a slow hook cannot block the shutdown
// sequence past the Kubernetes terminationGracePeriod.
func (r *Registry) emit(ctx context.Context, event Event) error {
	r.mu.RLock()
	hooks := make([]Hook, len(r.events[event]))
	copy(hooks, r.events[event])
	timeout := r.hookTimeout
	r.mu.RUnlock()

	// Apply timeout to lifecycle events where a slow hook can delay shutdown.
	if timeout > 0 {
		switch event {
		case EventBeforeStart, EventAfterStart, EventBeforeStop, EventAfterStop, EventBeforeReload, EventAfterReload:
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
	}

	ctx = WithEvent(ctx, event)

	var errs []error
	for _, h := range hooks {
		if ctx.Err() != nil {
			errs = append(errs, fmt.Errorf("hook timeout exceeded for %s", event))
			break
		}
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("hook for %s panicked: %v", event, p)
				}
			}()
			err = h(ctx)
		}()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
