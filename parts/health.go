package parts

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/component"
)

// HealthComponent exposes three Kubernetes-style health endpoints:
//
//   - GET /healthz — full diagnostic report (aggregates all Healther components)
//   - GET /livez  — lightweight liveness probe (always 200 unless shutting down)
//   - GET /readyz — readiness probe (aggregates Healther; returns 503 during shutdown)
//
// HTTP status codes:
//
//   - HealthOK       → 200
//   - HealthDegraded → 200  (still serving, but flagged in the body)
//   - HealthDown     → 503
//
// During graceful shutdown (after SetShuttingDown is called), /readyz
// immediately returns 503 so load balancers drain traffic before the
// process exits.
//
// Aggregation logic lives in Registry — the component just formats
// the result. This keeps HealthComponent itself tiny and unaffected
// by what other Components register.
//
// No Dependencies: the component can sit at the root of the topo
// graph. It does not probe anything itself.
type HealthComponent struct {
	path         string
	kernel       component.Kernel
	shuttingDown atomic.Bool
}

// NewHealthComponent constructs the component. Passing an empty path
// defaults to "/healthz" — the canonical Kubernetes naming — but
// operators can override to match an existing probe convention.
// /livez and /readyz are always registered alongside the main path.
func NewHealthComponent(path string) *HealthComponent {
	if path == "" {
		path = "/healthz"
	}
	return &HealthComponent{path: path}
}

// Name implements component.Component.
func (h *HealthComponent) Name() string { return "health" }

// ConfigKey implements component.Component.
func (h *HealthComponent) ConfigKey() string { return "health" }

// Init captures the kernel and subscribes a BeforeStop hook that flips
// the readiness flag so /readyz starts returning 503 during drain.
func (h *HealthComponent) Init(ctx context.Context, k component.Kernel) error {
	h.kernel = k
	k.On(component.EventBeforeStop, func(context.Context) error {
		h.SetShuttingDown()
		return nil
	})
	return nil
}

// Close is a no-op.
func (h *HealthComponent) Close(ctx context.Context) error { return nil }

// SetShuttingDown marks the component as shutting down. After this call
// /readyz returns 503 and /livez continues to return 200.
func (h *HealthComponent) SetShuttingDown() { h.shuttingDown.Store(true) }

// Mount implements component.Router. Expects a gin router (engine or
// group) — registers /healthz, /livez, and /readyz endpoints.
func (h *HealthComponent) Mount(router any) error {
	r, ok := router.(interface {
		GET(string, ...gin.HandlerFunc) gin.IRoutes
	})
	if !ok {
		return fmt.Errorf("health: Mount expected a gin router, got %T", router)
	}
	r.GET(h.path, h.serveHealthz)
	r.GET("/livez", h.serveLivez)
	r.GET("/readyz", h.serveReadyz)
	return nil
}

// serveHealthz returns the full aggregated health report.
func (h *HealthComponent) serveHealthz(c *gin.Context) {
	report := h.kernel.Health(c.Request.Context())
	code := http.StatusOK
	if report.Status == component.HealthDown {
		code = http.StatusServiceUnavailable
	}
	c.JSON(code, report)
}

// serveLivez is a lightweight liveness check. It always returns 200
// as long as the process is running — it does NOT check DB/Redis/etc.
func (h *HealthComponent) serveLivez(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// serveReadyz returns 503 when shutting down, when any component is down,
// or when any ReadyChecker reports not-ready (warm-up in progress) —
// signalling the pod should be removed from the load balancer.
func (h *HealthComponent) serveReadyz(c *gin.Context) {
	if h.shuttingDown.Load() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "shutting_down"})
		return
	}
	report := h.kernel.Health(c.Request.Context())
	code := http.StatusOK
	if report.Status == component.HealthDown {
		code = http.StatusServiceUnavailable
	}
	// Readiness gate: if any ReadyChecker reports not-ready, return 503
	// even when health probes pass. This handles the warm-up period
	// between Init success and actual traffic readiness.
	if code == http.StatusOK {
		if err := h.kernel.ReadyCheck(c.Request.Context()); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "warming_up",
				"error":  err.Error(),
			})
			return
		}
	}
	c.JSON(code, report)
}

// Path returns the configured endpoint path (e.g. "/healthz").
// Useful for tests and diagnostics.
func (h *HealthComponent) Path() string { return h.path }
