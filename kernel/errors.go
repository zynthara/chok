package kernel

import "errors"

// ErrReloadInProgress is returned by Reload when another reload is
// still being processed. Concurrent triggers coalesce — the loser is
// told, never queued (v1 contract preserved, SPEC §3.3).
var ErrReloadInProgress = errors.New("kernel: reload already in progress")

// ErrNotStarted is returned by Reload when the registry has not
// completed startup (or has already stopped).
var ErrNotStarted = errors.New("kernel: registry not started")

// ErrAlreadyStarted is returned by Start on a second call — the
// registry, like the App, is single-use.
var ErrAlreadyStarted = errors.New("kernel: registry already started")

// ErrDraining is returned by Ready while the draining phase is in
// progress: readiness endpoints turn 503 before traffic is dropped.
var ErrDraining = errors.New("kernel: draining")
