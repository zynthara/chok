package kernel

import "time"

// Lifecycle events published to the bus (layer two, SPEC §3.5):
// observability taps — metrics gauges, /componentz history, tracing —
// subscribe to these. They carry no veto power by construction.

// AppStarted fires once when startup completes (all components
// initialized, routes mounted, every Server ready).
type AppStarted struct {
	Duration time.Duration
}

// AppStopped fires after the last component closed, immediately
// before the bus itself drains and closes.
type AppStopped struct {
	Duration time.Duration
}

// ComponentInitialized fires per component as Init succeeds.
type ComponentInitialized struct {
	Key      Key
	Duration time.Duration
}

// ComponentDegraded fires when an Optional component's Init fails and
// startup continues without it.
type ComponentDegraded struct {
	Key Key
	Err string
}

// ComponentClosed fires per component as Close completes during
// shutdown or startup rollback.
type ComponentClosed struct {
	Key      Key
	Duration time.Duration
	Err      string // "" on clean close
}

// ReloadApplied fires when a reload fully succeeds (config swapped,
// components dispatched, user callback returned nil).
type ReloadApplied struct {
	Duration time.Duration
	// Reloaded lists components that received a Reload call.
	Reloaded []string
	// RestartPending lists changed restart-only field paths that were
	// warn-logged and NOT applied to running components.
	RestartPending []string
}
