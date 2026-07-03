package kernel

import (
	"context"
	"time"

	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/kernel/event"
	"github.com/zynthara/chok/v2/log"
)

// Kernel is what a component sees in Init: configuration, logging,
// the event bus and dependency lookup. Implemented by the Registry.
//
// Dependency access happens in Init (SPEC §11.4): by Close time the
// published view has already shrunk, so "don't call Lookup in Close"
// is not a convention — the peer simply isn't there.
type Kernel interface {
	// Config returns the current immutable configuration snapshot.
	Config() *conf.Snapshot

	// Logger returns the root logger (owned by the App, alive strictly
	// longer than every component).
	Logger() log.Logger

	// Bus returns the process event bus.
	Bus() *event.Bus

	// Lookup returns the component registered under (kind, instance)
	// when it is enabled and currently usable (initialized, not yet
	// closed). Disabled, failed and closed components return false —
	// callers can never observe a half-initialized object.
	Lookup(kind string, instance ...string) (Component, bool)
}

// Get is the typed dependency accessor (SPEC §3.1 definition 2):
//
//	db, ok := kernel.Get[*db.Component](k, "db")          // default
//	ro, ok := kernel.Get[*db.Component](k, "db", "read")  // named
//
// The second value is false when the component is absent, disabled,
// failed, closed, or not of type T.
func Get[T any](k Kernel, kind string, instance ...string) (T, bool) {
	var zero T
	if k == nil {
		return zero, false
	}
	c, ok := k.Lookup(kind, instance...)
	if !ok {
		return zero, false
	}
	t, ok := c.(T)
	if !ok {
		return zero, false
	}
	return t, true
}

// State is a component's lifecycle position as published in the view.
type State string

const (
	StatePending  State = "pending"  // assembled, not yet initialized
	StateReady    State = "ready"    // Init succeeded
	StateDegraded State = "degraded" // Optional component whose Init failed
	StateDisabled State = "disabled" // enabled:false at startup
	StateClosed   State = "closed"   // Close completed (or rolled back)
	StateFailed   State = "failed"   // required Init failure (pre-rollback)
)

// ComponentStatus is the observable record for one component —
// /componentz and Health render from this, disabled entries included
// (SPEC §3.1 definition 3: disabled is visible, not vanished).
type ComponentStatus struct {
	Key        Key
	Descriptor Descriptor
	ConfigKey  string // derived section key ("" = none)
	State      State
	Err        string // last lifecycle error, "" when none
}

// HealthStatus classifies one component's probe outcome.
type HealthStatus string

const (
	HealthUp       HealthStatus = "up"
	HealthDegraded HealthStatus = "degraded"
	HealthDown     HealthStatus = "down"
	HealthDisabled HealthStatus = "disabled" // informational, aggregates as OK
)

// HealthEntry is one component's health line.
type HealthEntry struct {
	Key      Key
	Status   HealthStatus
	Err      string
	Duration time.Duration
}

// HealthReport aggregates probe results: any required component down
// ⇒ Down; optional failures and degraded components ⇒ Degraded;
// disabled entries are informational.
type HealthReport struct {
	Status  HealthStatus
	Entries []HealthEntry
}

// PostReloadFunc is the user post-reload callback (WithReloadFunc):
// invoked synchronously as the last reload stage, only when config
// swap and component dispatch both succeeded; its error fails the
// reload (v1 contract, SPEC §9).
type PostReloadFunc func(ctx context.Context) error

// RoutesFunc is the user route-registration callback, invoked between
// MountOrder ≤ 0 and > 0 mounters during the mount phase.
type RoutesFunc func(r Router) error

// RouterProvider is implemented by the (single) component that owns
// the HTTP router — the web module in the target architecture. The
// mount phase asks it for the Router handed to every Mounter and the
// Routes callback. Assembling more than one provider is a startup
// error; assembling Mounters (or Routes) with no provider is too.
//
// This is a kernel-side discovery contract, not a battery name: any
// component may fill the role (fixtures use a test double).
type RouterProvider interface {
	ProvideRouter() Router
}
