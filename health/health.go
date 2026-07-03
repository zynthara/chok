// Package health is the chok v2 health module: /healthz (full
// diagnostic report), /livez (process liveness) and /readyz
// (traffic-worthiness incl. drain state), all rendered from the
// kernel's read-path aggregation APIs.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zynthara/chok/v2/kernel"
)

// Options is the "health" yaml section.
type Options struct {
	Enabled bool `mapstructure:"enabled" default:"true"`
	// Path is the diagnostic endpoint; /livez and /readyz mount
	// alongside it at fixed canonical paths.
	Path string `mapstructure:"path" default:"/healthz" reload:"restart"`
	// ProbeTimeout bounds one aggregated health probe pass.
	ProbeTimeout time.Duration `mapstructure:"probe_timeout" default:"3s" reload:"hot"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Path != "" && !strings.HasPrefix(o.Path, "/") {
		return fmt.Errorf("health: path must start with /, got %q", o.Path)
	}
	if o.ProbeTimeout <= 0 {
		return fmt.Errorf("health: probe_timeout must be positive, got %s", o.ProbeTimeout)
	}
	return nil
}

// Module returns the health component for chok.Use.
func Module() kernel.Component { return &component{} }

type component struct {
	k        kernel.Kernel
	opts     Options
	draining atomic.Bool
}

func (c *component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "health",
		ConfigKey: "health",
		Options:   Options{},
	}
}

func (c *component) Init(ctx context.Context, k kernel.Kernel) error {
	c.k = k
	if err := k.Config().Section("health", &c.opts); err != nil {
		return fmt.Errorf("health: decode section: %w", err)
	}
	return nil
}

func (c *component) Close(ctx context.Context) error { return nil }

// Reload accepts hot changes (probe_timeout). Handlers read the
// current snapshot per request — the RCU decode cache makes that
// cheap — so applying the change is validating it decodes.
func (c *component) Reload(ctx context.Context) error {
	var o Options
	if err := c.k.Config().Section("health", &o); err != nil {
		return fmt.Errorf("health: decode section: %w", err)
	}
	return nil
}

// Drain implements kernel.Drainer: the draining phase flips /readyz
// to 503 before Serve contexts are cancelled, so load balancers pull
// the pod first (SPEC §3.2 — the kernel broadcasts to the interface,
// it does not know this component is "health").
func (c *component) Drain(ctx context.Context) {
	c.draining.Store(true)
}

// Mount implements kernel.Mounter.
func (c *component) Mount(r kernel.Router) error {
	r.Handle(http.MethodGet, c.opts.Path, http.HandlerFunc(c.serveHealthz))
	r.Handle(http.MethodGet, "/livez", http.HandlerFunc(c.serveLivez))
	r.Handle(http.MethodGet, "/readyz", http.HandlerFunc(c.serveReadyz))
	return nil
}

type healthResponse struct {
	Status     string        `json:"status"`
	Components []healthEntry `json:"components"`
}

type healthEntry struct {
	Component  string `json:"component"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

func (c *component) serveHealthz(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), c.probeTimeout())
	defer cancel()
	rep := c.k.Health(ctx)

	resp := healthResponse{Status: string(rep.Status)}
	for _, e := range rep.Entries {
		resp.Components = append(resp.Components, healthEntry{
			Component:  e.Key.String(),
			Status:     string(e.Status),
			Error:      e.Err,
			DurationMS: e.Duration.Milliseconds(),
		})
	}
	code := http.StatusOK
	if rep.Status == kernel.HealthDown {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, resp)
}

func (c *component) serveLivez(w http.ResponseWriter, req *http.Request) {
	if c.draining.Load() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "shutting-down"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (c *component) serveReadyz(w http.ResponseWriter, req *http.Request) {
	if c.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), c.probeTimeout())
	defer cancel()
	if err := c.k.Ready(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not-ready", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (c *component) probeTimeout() time.Duration {
	// ProbeTimeout is reload:"hot": re-read the section each probe so
	// a reloaded value applies without restart.
	var o Options
	if err := c.k.Config().Section("health", &o); err == nil && o.ProbeTimeout > 0 {
		return o.ProbeTimeout
	}
	return 3 * time.Second
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
