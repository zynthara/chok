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

type healthEntry struct {
	Component  string `json:"component"`
	Status     string `json:"status"`
	Error      string `json:"error,omitzero"`
	DurationMS int64  `json:"duration_ms"`
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
			e := healthEntry{Component: c.name, Status: statusUp, DurationMS: time.Since(begin).Milliseconds()}
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
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("health check panicked: %v", p)
		}
	}()
	done := make(chan error, 1)
	go func() { done <- h.Health(ctx) }()
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

// mountBuiltins registers the probes; WP3 adds the OpenAPI document and
// the debug endpoints.
func (a *App) mountBuiltins() {
	h := a.base.HTTP.Health
	if !h.Enabled {
		return
	}
	path := h.Path
	if path == "" {
		path = "/healthz"
	}
	a.mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		report := a.checkHealth(r.Context(), a.snapshot())
		code := http.StatusOK
		if report.Status != statusUp {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, r, code, report)
	})
	a.mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, http.StatusOK, map[string]string{"status": statusUp})
	})
	a.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !a.ready.Load() || a.draining.Load() {
			writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"status": statusDown})
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]string{"status": statusUp})
	})
}

// probePaths are the endpoints the access log leaves out.
func (a *App) probePaths() []string {
	path := a.base.HTTP.Health.Path
	if path == "" {
		path = "/healthz"
	}
	return []string{path, "/livez", "/readyz"}
}
