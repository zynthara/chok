package chok

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	statusUp   = "up"
	statusDown = "down"
)

type healthReport struct {
	Status     string        `json:"status"`
	Version    string        `json:"version,omitzero"`
	Components []healthEntry `json:"components"`
}

// healthEntry is one component's check. Error is the text of its
// failure, which failures() and the log read; the health endpoint drops
// it from the body unless http.health.details is on.
type healthEntry struct {
	Component  string `json:"component"`
	Status     string `json:"status"`
	Error      string `json:"error,omitzero"`
	DurationMS int64  `json:"duration_ms"`

	elapsed time.Duration
}

func (r healthReport) failures() string {
	var out []string
	for _, c := range r.Components {
		if c.Status != statusUp {
			out = append(out, c.Component+": "+c.Error)
		}
	}
	return strings.Join(out, "; ")
}

// checkHealth runs every Healther in parallel, each bounded by
// http.health.probe_timeout.
func (a *App) checkHealth(ctx context.Context, comps []*component) healthReport {
	timeout := a.base.HTTP.Health.ProbeTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	report := healthReport{Status: statusUp, Version: a.version, Components: []healthEntry{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range comps {
		h, ok := c.value.(Healther)
		if !ok {
			continue
		}
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			begin := time.Now()
			err := callHealth(cctx, h)
			elapsed := time.Since(begin)
			e := healthEntry{Component: c.name, Status: statusUp, DurationMS: elapsed.Milliseconds(), elapsed: elapsed}
			if err != nil {
				e.Status, e.Error = statusDown, err.Error()
			}
			mu.Lock()
			report.Components = append(report.Components, e)
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.SortStableFunc(report.Components, func(x, y healthEntry) int { return strings.Compare(x.Component, y.Component) })
	for _, e := range report.Components {
		if e.Status != statusUp {
			report.Status = statusDown
		}
	}
	return report
}

func callHealth(ctx context.Context, h Healther) (err error) {
	done := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("health check panicked: %v", p)
			}
			done <- err
		}()
		err = h.Health(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("health check timed out: %w", ctx.Err())
	}
}

func (a *App) snapshot() []*component {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.components)
}

// mountBuiltins registers the probes, the OpenAPI document and its UI,
// and the debug endpoints. They are not part of the document.
func (a *App) mountBuiltins() {
	a.mountOpenAPI()
	a.mountDebug()
	h := a.base.HTTP.Health
	if !h.Enabled {
		return
	}
	path := h.Path
	if path == "" {
		path = "/healthz"
	}
	a.handleBuiltin(http.MethodGet, path, func(w http.ResponseWriter, r *http.Request) {
		// A load balancer may watch this path instead of /readyz: it too
		// makes a stop drain, and it turns 503 while the stop drains,
		// without the checks.
		a.probed.Store(true)
		if a.draining.Load() {
			writeJSON(w, r, http.StatusServiceUnavailable, healthReport{Status: statusDown, Version: a.version, Components: []healthEntry{}})
			return
		}
		ctx := r.Context()
		report := a.checkHealth(ctx, a.snapshot())
		code := http.StatusOK
		if report.Status != statusUp {
			code = http.StatusServiceUnavailable
		}
		// A failure is logged with its error; the body carries the error
		// only with http.health.details, as it can hold an address or
		// part of a DSN.
		for i, e := range report.Components {
			if e.Status == statusUp {
				continue
			}
			LoggerFrom(ctx).WarnContext(ctx, "chok: health check failed", "component", e.Component, "error", e.Error, "elapsed", e.elapsed)
			if !h.Details {
				report.Components[i].Error = ""
			}
		}
		writeJSON(w, r, code, report)
	})
	a.handleBuiltin(http.MethodGet, "/livez", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, http.StatusOK, map[string]string{"status": statusUp})
	})
	a.handleBuiltin(http.MethodGet, "/readyz", func(w http.ResponseWriter, r *http.Request) {
		a.probed.Store(true)
		if !a.ready.Load() || a.draining.Load() {
			writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"status": statusDown})
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]string{"status": statusUp})
	})
}

// handleBuiltin registers one of the App's own endpoints, marked with its
// pattern as a route is, so the access log and telemetry name it.
func (a *App) handleBuiltin(method, pattern string, h http.HandlerFunc) {
	a.mux.Handle(method+" "+pattern, routeMarker(pattern, h))
}

// probePaths are the endpoints the access log leaves out.
func (a *App) probePaths() []string {
	path := a.base.HTTP.Health.Path
	if path == "" {
		path = "/healthz"
	}
	return []string{path, "/livez", "/readyz"}
}
