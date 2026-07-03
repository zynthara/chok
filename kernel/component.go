// Package kernel is the chok v2 component model: a declarative
// Descriptor, a small set of behaviour interfaces discovered by
// type assertion, and a single-actor control plane (Registry) that
// owns every lifecycle transition.
//
// Design axioms (SPEC §1): invariants live in types, not docs; the
// kernel knows no battery names — ordering, draining and observability
// all flow through Descriptor fields and behaviour interfaces.
package kernel

import (
	"context"
	"time"
)

// Component is the mandatory contract every subsystem implements.
//
// Describe must be pure and callable before Init: the Registry reads
// it during assembly to build the dependency graph, derive the config
// section and order mounting. Init receives the Kernel for config,
// logging, bus access and dependency lookup. Close releases resources;
// it can no longer reach peers (the Kernel view has already shrunk —
// a structural guarantee, not a convention).
type Component interface {
	Describe() Descriptor
	Init(ctx context.Context, k Kernel) error
	Close(ctx context.Context) error
}

// Descriptor is the component's static self-declaration. It replaces
// v1's seven single-method declaration interfaces (Dependent,
// ConfigKeyer, Optionaler, *Timeouter, ...) with plain data.
type Descriptor struct {
	// Kind names the capability: "db", "http", "account", ...
	// Required, non-empty.
	Kind string

	// Instance distinguishes multiple components of one Kind.
	// "" means "default". Named instances resolve their config at
	// ConfigKey + ".instances." + Instance (M1 mini-SPEC §1).
	Instance string

	// ConfigKey is the yaml section this component owns. "" means the
	// component has no configuration: it is always enabled and, when
	// it implements Reloader, it is dispatched on every config reload
	// after all sectioned components (SPEC §3.4).
	ConfigKey string

	// Options is a zero-value sample of the ConfigKey section's typed
	// Options struct. The App collects these at assembly time to drive
	// env binding, `default` tags, validation and reload tag diffing
	// (SPEC §3.4 "framework section types collected via Use()").
	// nil = the section stays untyped (ad-hoc decode only).
	Options any

	// Needs declares dependencies on other components by (Kind,
	// Instance). Hard dependencies gate startup ordering and fail
	// startup when missing or disabled; Optional ones degrade to
	// absence.
	Needs []Dep

	// Timeouts bounds Init / Close / Reload for this component.
	// Zero values fall back to the App-level defaults.
	Timeouts Timeouts

	// Optional marks the whole component non-critical: Init failure
	// logs a warning and reports Degraded instead of aborting startup.
	Optional bool

	// MountOrder orders Mounter invocation within the mount phase:
	// <= 0 mounts before the user Routes callback (ties broken by
	// topological start order), > 0 mounts after it in ascending
	// order. A docs-style component that must see every route (e.g.
	// swagger) declares a large positive value itself — the kernel
	// has no name-based special cases.
	MountOrder int
}

// Dep is a dependency edge in Descriptor.Needs.
type Dep struct {
	Kind     string
	Instance string // "" ⇒ "default"
	Optional bool
}

// Timeouts bounds the component's lifecycle calls. Zero values use the
// registry-wide defaults (Init 30s / Close 15s / Reload 10s unless
// overridden at App level).
type Timeouts struct {
	Init   time.Duration
	Close  time.Duration
	Reload time.Duration
}

// Key is the registry identity of a component: Descriptor.Kind plus
// the normalized instance name ("default" when Instance is "").
type Key struct {
	Kind     string
	Instance string
}

// DefaultInstance is the normalized name of the unnamed instance.
const DefaultInstance = "default"

// KeyOf derives the registry Key from a Descriptor.
func KeyOf(d Descriptor) Key {
	inst := d.Instance
	if inst == "" {
		inst = DefaultInstance
	}
	return Key{Kind: d.Kind, Instance: inst}
}

// String renders "kind" for the default instance and "kind@instance"
// otherwise. Display form only — the @ form is NOT a config key
// (config addressing is nested, see mini-SPEC §1).
func (k Key) String() string {
	if k.Instance == DefaultInstance || k.Instance == "" {
		return k.Kind
	}
	return k.Kind + "@" + k.Instance
}

// --- Behaviour interfaces (discovered by type assertion) -------------

// Reloader receives config-driven reload dispatch. Components with a
// ConfigKey are called only when their section's hot fields changed
// (conf tag diff); components without a ConfigKey are called on every
// config change, after all sectioned components.
type Reloader interface {
	Reload(ctx context.Context) error
}

// Healther reports component health. Probed in parallel with a
// fan-in timeout by the registry's Health aggregation.
type Healther interface {
	Health(ctx context.Context) error
}

// Mounter registers HTTP routes during the mount phase. The Router
// contract is stdlib-only (see router.go); the concrete implementation
// arrives with the web package (M2) — tests use doubles.
type Mounter interface {
	Mount(r Router) error
}

// Migrator runs schema migration. Executed serially within each
// topological level, after that level's Init completes.
type Migrator interface {
	Migrate(ctx context.Context) error
}

// Readier gates readiness beyond plain health: Ready aggregation
// consults it (e.g. connection warm-up, initial cache fill).
type Readier interface {
	ReadyCheck(ctx context.Context) error
}

// Server is the long-running behaviour: Serve blocks until ctx is
// cancelled, calling ready exactly once when it can accept work. All
// Servers run in parallel after mount; the registry aggregates ready
// before reporting the app started. In-flight work must finish before
// Serve returns — the draining phase waits for every Serve to return
// before any component Close runs, so dependencies stay alive during
// wind-down.
type Server interface {
	Serve(ctx context.Context, ready func()) error
}

// Drainer is notified at the start of the draining phase (broadcast,
// parallel, short timeout) so implementations can flip readiness
// endpoints to 503 or stop accepting new work before Serve contexts
// are cancelled. The kernel does not know which component is "health".
type Drainer interface {
	Drain(ctx context.Context)
}
