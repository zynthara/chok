// Package component defines the Component abstraction that unifies every
// subsystem in chok (logger, db, cache, scheduler, auth, ...) under a single
// lifecycle + configuration + health + reload contract.
//
// Design rationale and architecture: docs/design.md (§4 核心抽象).
//
// The mandatory Component interface requires only Name, Init, and Close.
// All other capabilities (Reload, Health, Route mounting, Migration,
// Dependencies, ConfigKey, etc.) are opt-in via separate interfaces that the
// Registry discovers through type assertions.
package component

import (
	"context"
	"fmt"
	"time"

	"github.com/zynthara/chok/log"
)

// Component is the mandatory interface every subsystem implements.
// Optional capabilities (Reload, Health, Route mounting, Migration,
// explicit dependencies) are declared by also implementing the
// corresponding interface below; the Registry uses type assertions to
// dispatch to them.
type Component interface {
	// Name is the unique identifier used for registration, dependency
	// lookup, logging, and health reports. Conventionally a short
	// lowercase identifier (e.g. "log", "db", "cache").
	Name() string

	// Init is called once during Registry.Start, after all declared
	// Dependencies have been Init'd. It is where the component acquires
	// resources (opens connections, starts goroutines, mounts routes on
	// the HTTP server, etc.).
	//
	// Returning an error aborts startup: already-initialised components
	// are Close'd in reverse order and Start returns the error.
	Init(ctx context.Context, k Kernel) error

	// Close releases resources acquired in Init. Called in reverse
	// topological order during Registry.Stop. Close should be idempotent
	// and safe to call even if Init failed partway.
	Close(ctx context.Context) error
}

// ConfigKeyer is an optional interface for components that map to a
// specific top-level key in the application config (e.g. "log", "cache").
// Not part of the mandatory Component contract — most components use the
// Resolver pattern instead, making ConfigKey redundant. Implement this
// only when external tooling needs to discover the mapping.
type ConfigKeyer interface {
	ConfigKey() string
}

// Reloadable is implemented by components that support config hot-reload.
// Called by Registry.Reload after the config store has been refreshed.
// The component should re-read its slice of the config and apply whatever
// is safely swappable at runtime (log level, cache TTL, rate-limit quota).
// Non-reloadable properties (listen address, DB DSN) should be ignored
// with a warning log line.
type Reloadable interface {
	Reload(ctx context.Context) error
}

// Healther is implemented by components that expose a health probe.
// The returned HealthStatus is aggregated by Registry.Health into a
// report suitable for /healthz. Non-implementers are assumed healthy.
type Healther interface {
	Health(ctx context.Context) HealthStatus
}

// Router is implemented by components that mount HTTP routes. The
// Registry calls Mount during Init for every Router with a Router-capable
// HTTP server provided in the Kernel. The concrete router type is
// intentionally left as any here so this package stays free of a gin
// import; downstream (server component) will assert the concrete type.
type Router interface {
	Mount(router any) error
}

// Migratable is implemented by components that need to run database or
// schema migrations at startup. Called between Init and Close in
// dependency order (typically db → components-that-own-tables).
type Migratable interface {
	Migrate(ctx context.Context) error
}

// Dependent declares ordering constraints. Components without
// Dependencies are assumed to depend on nothing and are Init'd in
// registration order relative to other no-dependency components.
type Dependent interface {
	// Dependencies returns the names of components that must be Init'd
	// before this one. Unknown names abort startup with a clear error.
	// Cyclic declarations abort startup with a cycle listing.
	Dependencies() []string
}

// OptionalDependent declares soft ordering constraints. Unlike Dependent,
// missing optional dependencies do NOT abort startup — they are silently
// skipped. When an optional dependency IS registered, it will be Init'd
// before this component; when it is absent, the component Init proceeds
// without it.
//
// Typical use: HTTPComponent optionally depends on "metrics" and "log"
// for middleware injection. If those components exist, they Init first;
// if not, HTTP inits without them.
type OptionalDependent interface {
	OptionalDependencies() []string
}

// Optionaler is implemented by components whose Init failure should not
// abort application startup. When Init returns an error for an optional
// component, the Registry logs a warning and continues — the component
// is not added to the start order and Kernel.Get returns nil for it.
//
// Typical candidates: cache, metrics, tracing — subsystems that enhance
// but are not required for core business logic.
type Optionaler interface {
	Optional() bool
}

// InitTimeouter is implemented by components that need a custom Init
// timeout. When a component's Init exceeds this duration, the context
// passed to Init is cancelled and the startup is aborted.
//
// Components that don't implement this interface use the Registry's
// default init timeout (see Registry.SetDefaultInitTimeout).
type InitTimeouter interface {
	InitTimeout() time.Duration
}

// CloseTimeouter is implemented by components that need a custom Close
// timeout. When a component's Close exceeds this duration, the context
// passed to Close is cancelled and the error is recorded.
//
// Components that don't implement this interface use the Registry's
// default close timeout (see Registry.SetCloseTimeout).
type CloseTimeouter interface {
	CloseTimeout() time.Duration
}

// ReloadTimeouter is implemented by components that need a custom Reload
// timeout. When a component's Reload exceeds this duration, the context
// passed to Reload is cancelled and the error is recorded.
//
// Components that don't implement this interface use the Registry's
// default reload timeout (see Registry.SetReloadTimeout).
type ReloadTimeouter interface {
	ReloadTimeout() time.Duration
}

// DepsValidator is implemented by components that want to validate their
// dependencies are properly configured before Init runs. ValidateDeps is
// called after topological sort but before any Init — catching "account
// enabled but db not configured" errors early rather than deep inside
// Init.
type DepsValidator interface {
	ValidateDeps(k Kernel) error
}

// ReadyChecker is an optional interface for components that need a
// warm-up period after Init before they can serve traffic. A component
// whose Init succeeds but whose ReadyCheck returns a non-nil error is
// treated as "started but not yet ready" — /readyz will return 503
// until all ReadyCheckers pass.
//
// Typical candidates: caches that need pre-warming, components that
// load initial state from a remote service, connection pools that
// must establish a minimum number of connections.
//
// The context carries a deadline derived from the health timeout; the
// check should be lightweight and fast.
type ReadyChecker interface {
	ReadyCheck(ctx context.Context) error
}

// Kernel is the minimal view a Component has of the surrounding
// application. It deliberately excludes App-specific lifecycle methods
// so Components cannot re-enter Run/Stop or mutate registration. The
// interface is also easy to mock in tests.
type Kernel interface {
	// Config returns the application config pointer. Components cast it
	// to the concrete struct type they expect. The pointer refers to the
	// live config struct that is updated in-place during Reload via
	// reflect.Value.Set under configMu. Single-field reads are safe on
	// aligned architectures, but multi-field reads across a reload
	// boundary may observe inconsistent state. For consistent multi-field
	// access, use ConfigSnapshot() instead.
	Config() any

	// ConfigSnapshot returns a shallow copy of the top-level config
	// struct, captured atomically after each Reload (or after initial
	// load). Resolvers that read multiple fields should prefer this
	// over Config() to avoid torn reads: the top-level struct value
	// is a fresh allocation, so value-type fields are fully
	// independent of subsequent reloads.
	//
	// Slice/map/pointer fields still share backing storage with the
	// live config. Mutating elements of such a field (e.g. indexing
	// into a []string TrustedProxies or map[string]any Metadata) CAN
	// affect the live config — treat the snapshot as read-only. For
	// deeply nested dynamic config, copy the slice/map explicitly
	// before handing it out to caller code that might mutate it.
	ConfigSnapshot() any

	// Logger returns the shared framework logger. Components should
	// derive per-component loggers via Logger().With("component", Name).
	Logger() log.Logger

	// Get returns a previously-registered component by name, or nil if
	// unregistered or disabled. Components use this to reach declared
	// dependencies — e.g. the cache component obtains the redis client
	// via k.Get("redis").(*RedisComponent).Client().
	Get(name string) Component

	// On subscribes a hook to an application-level event. The hook fires
	// during the corresponding phase in Registry.Start/Stop/Reload. See
	// the Event constants for the available phases.
	On(event Event, hook Hook)

	// Health aggregates every Healther component into a single report.
	// Intended for HealthComponent (which exposes /healthz) and admin
	// tooling; callers that only need one component's status should
	// use Get(name) and type-assert instead.
	Health(ctx context.Context) HealthReport

	// ReadyCheck polls every ReadyChecker component and returns the
	// first non-nil error. Returns nil when all components (or none)
	// are ready. Intended for /readyz to gate traffic until warm-up
	// completes.
	ReadyCheck(ctx context.Context) error
}

// Event identifies an application-level lifecycle phase. Hooks registered
// via Kernel.On run in registration order when the phase fires.
type Event string

const (
	// EventBeforeStart fires after config is loaded and the Registry is
	// about to Init the first component.
	EventBeforeStart Event = "before_start"

	// EventAfterStart fires once every component's Init has succeeded and
	// the application is fully running.
	EventAfterStart Event = "after_start"

	// EventBeforeStop fires when shutdown is initiated, before the first
	// Close is invoked. Useful for draining traffic or posting a "going
	// away" notice.
	EventBeforeStop Event = "before_stop"

	// EventAfterStop fires after the last component has Close'd
	// successfully. The process is about to exit.
	EventAfterStop Event = "after_stop"

	// EventBeforeReload fires when a reload is triggered (SIGHUP / API
	// call), before any component sees the new config.
	EventBeforeReload Event = "before_reload"

	// EventAfterReload fires after every Reloadable.Reload has been
	// invoked (even if some returned errors).
	EventAfterReload Event = "after_reload"
)

// Hook is a lifecycle event callback. Return an error to surface it to
// the reload/shutdown caller; most hooks should only return an error for
// critical issues since hook errors do not necessarily abort the phase.
//
// The ctx passed to hooks carries enrichment values accessible via
// EventFrom(ctx) and ReasonFrom(ctx).
type Hook func(ctx context.Context) error

// --- Hook context enrichment -------------------------------------------------

type eventCtxKey struct{}
type reasonCtxKey struct{}

// WithEvent stores the lifecycle event name in ctx.
// Injected automatically by Registry.emit — hooks receive it for free.
func WithEvent(ctx context.Context, event Event) context.Context {
	return context.WithValue(ctx, eventCtxKey{}, event)
}

// EventFrom retrieves the event name from ctx. Returns "" if absent.
func EventFrom(ctx context.Context) Event {
	if v, ok := ctx.Value(eventCtxKey{}).(Event); ok {
		return v
	}
	return ""
}

// WithReason stores a human-readable trigger reason in ctx (e.g.
// "signal", "file_change", "api_call"). Callers inject this before
// invoking Reload or Stop so hooks can distinguish the trigger source.
func WithReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, reasonCtxKey{}, reason)
}

// ReasonFrom retrieves the trigger reason from ctx. Returns "" if absent.
func ReasonFrom(ctx context.Context) string {
	if v, ok := ctx.Value(reasonCtxKey{}).(string); ok {
		return v
	}
	return ""
}

type configChangedCtxKey struct{}

// WithConfigChanged marks in ctx whether the config file actually changed
// during a reload. Components can check this via ConfigChanged(ctx) to
// skip expensive re-initialization when the reload was triggered manually
// (e.g. SIGHUP) without actual config changes.
func WithConfigChanged(ctx context.Context, changed bool) context.Context {
	return context.WithValue(ctx, configChangedCtxKey{}, changed)
}

// ConfigChanged returns whether the config file changed during this reload.
// Returns true if absent (conservative default: assume config changed).
func ConfigChanged(ctx context.Context) bool {
	if v, ok := ctx.Value(configChangedCtxKey{}).(bool); ok {
		return v
	}
	return true // conservative default
}

type phaseResultCtxKey struct{}

// PhaseResult carries the outcome of a lifecycle phase (Start/Stop/Reload)
// so hooks can observe success/failure and duration for observability.
type PhaseResult struct {
	Duration time.Duration // wall-clock duration of the phase
	Err      error         // nil on success
}

// WithPhaseResult stores the lifecycle phase result in ctx.
// Injected by Registry into after-event hooks (EventAfterStart,
// EventAfterStop, EventAfterReload).
func WithPhaseResult(ctx context.Context, result PhaseResult) context.Context {
	return context.WithValue(ctx, phaseResultCtxKey{}, result)
}

// PhaseResultFrom retrieves the phase result from ctx. Returns a zero
// PhaseResult if absent.
func PhaseResultFrom(ctx context.Context) (PhaseResult, bool) {
	if v, ok := ctx.Value(phaseResultCtxKey{}).(PhaseResult); ok {
		return v, true
	}
	return PhaseResult{}, false
}

type changedSectionsCtxKey struct{}

// WithChangedSections stores the set of config section keys that changed
// during a reload. Components can check SectionChanged(ctx, key) to skip
// expensive re-initialization when their specific config section didn't change.
func WithChangedSections(ctx context.Context, sections map[string]bool) context.Context {
	return context.WithValue(ctx, changedSectionsCtxKey{}, sections)
}

// SectionChanged reports whether the named config section changed during
// this reload cycle. Returns true if:
//   - the section is in the changed set
//   - no section info is available (conservative default)
//   - ConfigChanged(ctx) is false (no config changed at all → false for all sections)
//
// Components should use their ConfigKey() as the section name:
//
//	func (l *LoggerComponent) Reload(ctx context.Context) error {
//	    if !component.SectionChanged(ctx, l.ConfigKey()) {
//	        return nil // my config didn't change, skip
//	    }
//	    // ... actual reload
//	}
func SectionChanged(ctx context.Context, section string) bool {
	// If config didn't change at all, no section changed.
	if !ConfigChanged(ctx) {
		return false
	}
	sections, ok := ctx.Value(changedSectionsCtxKey{}).(map[string]bool)
	if !ok {
		return true // no section info available, conservative default
	}
	return sections[section]
}

// HealthLevel is a coarse status classification used by /healthz.
type HealthLevel string

const (
	// HealthOK means the component is fully operational.
	HealthOK HealthLevel = "ok"

	// HealthDegraded means the component is serving but impaired
	// (e.g. fallback cache path active, replica lag high). Aggregated
	// report still returns HTTP 200 but flags the component.
	HealthDegraded HealthLevel = "degraded"

	// HealthDown means the component cannot serve requests. Causes the
	// aggregated /healthz to return 503.
	HealthDown HealthLevel = "down"
)

// HealthStatus is a single component's health probe result.
type HealthStatus struct {
	Status  HealthLevel    `json:"status"`
	Details map[string]any `json:"details,omitempty"`
	// Error is the stringified error (if any) that caused degraded/down
	// status. Kept separate from Details so probes can render it
	// consistently without nested type assertions.
	Error string `json:"error,omitempty"`
}

// HealthReport is the aggregated view returned by Registry.Health.
// Status is "ok" only when every component is OK; any down component
// forces Status=down; any degraded (with no down) yields Status=degraded.
type HealthReport struct {
	Status     HealthLevel             `json:"status"`
	Components map[string]HealthStatus `json:"components"`
}

// Get is a type-safe helper for retrieving a Component from a Kernel.
// It replaces the common k.Get("name").(*SomeComponent) pattern with a
// nil-safe generic alternative that never panics on type mismatch:
//
//	redis, ok := component.Get[*parts.RedisComponent](k, "redis")
func Get[T Component](k Kernel, name string) (T, bool) {
	c := k.Get(name)
	if c == nil {
		var zero T
		return zero, false
	}
	t, ok := c.(T)
	return t, ok
}

// MustGet is like Get but panics if the component is not found or has the
// wrong type. Intended for Init methods where a missing declared dependency
// is a programming error that should fail fast:
//
//	redis := component.MustGet[*parts.RedisComponent](k, "redis")
func MustGet[T Component](k Kernel, name string) T {
	t, ok := Get[T](k, name)
	if !ok {
		var zero T
		panic(fmt.Sprintf("component: MustGet[%T](%q) failed — not registered or wrong type", zero, name))
	}
	return t
}
