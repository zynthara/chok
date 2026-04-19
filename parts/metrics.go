package parts

import (
	"context"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zynthara/chok/component"
)

// MetricsComponent owns a Prometheus registry, pre-populated with the
// Go runtime + process collectors, and exposes it at /metrics on the
// application's gin router.
//
// Other Components or user code obtain the underlying registry via
// PrometheusRegistry() to register custom collectors — for example a
// middleware that counts HTTP requests, or a cache wrapper that
// tracks hit rate.
//
// Lifecycle metrics (component health, reload counts, uptime) are
// registered automatically in Init and updated via EventAfterStart
// and EventAfterReload hooks — the component package itself stays
// free of prometheus imports.
type MetricsComponent struct {
	path     string
	registry *prometheus.Registry

	// lifecycle metrics — registered in Init, updated via hooks.
	// uptimeGauge is a GaugeFunc so scrapes always see live uptime
	// (no staleness between reloads).
	healthGauge *prometheus.GaugeVec
	reloadTotal *prometheus.CounterVec
	uptimeGauge prometheus.Collector
	startedAt   time.Time
}

// NewMetricsComponent constructs the component with a fresh registry.
// path defaults to "/metrics" (the Prometheus convention).
//
// The registry contains GoCollector + ProcessCollector by default so
// scraping is immediately useful. Pass WithoutDefaults to start with
// an empty registry (e.g. when integrating with an external collector
// that already registers those).
func NewMetricsComponent(path string) *MetricsComponent {
	if path == "" {
		path = "/metrics"
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return &MetricsComponent{path: path, registry: reg}
}

// WithoutDefaults returns a MetricsComponent with an empty Prometheus
// registry (no GoCollector / ProcessCollector). Useful when the host
// already collects those metrics elsewhere or for deterministic tests.
func (m *MetricsComponent) WithoutDefaults() *MetricsComponent {
	m.registry = prometheus.NewRegistry()
	return m
}

// Name implements component.Component.
func (m *MetricsComponent) Name() string { return "metrics" }

// ConfigKey implements component.Component.
func (m *MetricsComponent) ConfigKey() string { return "metrics" }

// Init registers lifecycle metrics (health gauges, reload counter, uptime)
// and subscribes to EventAfterStart / EventAfterReload hooks to keep them
// updated. The prometheus dependency stays in parts/ — the component
// package itself remains free of it.
func (m *MetricsComponent) Init(_ context.Context, k component.Kernel) error {
	m.startedAt = time.Now()

	healthGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "chok_component_health",
		Help: "Component health status: 1=ok, 0.5=degraded, 0=down.",
	}, []string{"component"})

	reloadTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "chok_reload_total",
		Help: "Total number of config reloads by result.",
	}, []string{"result"})

	// GaugeFunc reads uptime live on every scrape, so it never goes
	// stale between reloads. Capture m (not a local copy of startedAt)
	// so Close-then-Init reads the *current* startedAt on every scrape
	// rather than the one frozen into the first closure.
	uptimeGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "chok_app_uptime_seconds",
		Help: "Seconds since application started.",
	}, func() float64 {
		return time.Since(m.startedAt).Seconds()
	})

	// On a shared registry (tests that spin up multiple apps), a stale
	// uptime collector from a previous Init would be reused via
	// AlreadyRegisteredError — but its closure captured a different
	// MetricsComponent instance, so uptime would never reflect this
	// one. Unregister first to guarantee our fresh closure wins.
	if m.uptimeGauge != nil {
		m.registry.Unregister(m.uptimeGauge)
		m.uptimeGauge = nil
	}

	// Register each collector; if a previous Init already registered an
	// equivalent collector (Init → Close → Init sequence), reuse the
	// live instance so writes here reach the scraped collector, not an
	// orphan pointer. Otherwise a re-Init leaves the scrape endpoint
	// returning stale metrics forever.
	m.healthGauge = registerOrReuseGauge(m.registry, healthGauge)
	m.reloadTotal = registerOrReuseCounter(m.registry, reloadTotal)
	m.uptimeGauge = registerOrReuseCollector(m.registry, uptimeGauge)
	if m.healthGauge == nil || m.reloadTotal == nil || m.uptimeGauge == nil {
		return fmt.Errorf("metrics: failed to register lifecycle collectors")
	}

	// After every start/reload, refresh health gauges and bump counters.
	k.On(component.EventAfterStart, func(ctx context.Context) error {
		m.refreshHealthGauges(k)
		return nil
	})
	k.On(component.EventAfterReload, func(ctx context.Context) error {
		result := "success"
		if pr, ok := component.PhaseResultFrom(ctx); ok && pr.Err != nil {
			result = "failure"
		}
		m.reloadTotal.WithLabelValues(result).Inc()
		m.refreshHealthGauges(k)
		return nil
	})

	return nil
}

// refreshHealthGauges updates the per-component health gauge from the
// latest aggregate report. Uses a bounded ctx so a slow Healther can't
// block the after-reload hook chain indefinitely. The 5s budget covers
// the registry's default 3s per-probe timeout plus a 1s fan-in
// headroom, with a small margin for Prometheus registry contention
// when many gauges are being written concurrently.
func (m *MetricsComponent) refreshHealthGauges(k component.Kernel) {
	// uptime is exposed as a GaugeFunc — no explicit update needed here.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report := k.Health(ctx)
	for name, status := range report.Components {
		var val float64
		switch status.Status {
		case component.HealthOK:
			val = 1
		case component.HealthDegraded:
			val = 0.5
		case component.HealthDown:
			val = 0
		}
		m.healthGauge.WithLabelValues(name).Set(val)
	}
}

// Close unregisters lifecycle collectors so a subsequent Init (e.g. in
// tests or after a full restart) can register fresh instances without
// silent AlreadyRegistered duplicates. The registry itself is kept for
// inspection; user-registered collectors are not touched.
func (m *MetricsComponent) Close(_ context.Context) error {
	if m.registry == nil {
		return nil
	}
	if m.healthGauge != nil {
		m.registry.Unregister(m.healthGauge)
		m.healthGauge = nil
	}
	if m.reloadTotal != nil {
		m.registry.Unregister(m.reloadTotal)
		m.reloadTotal = nil
	}
	if m.uptimeGauge != nil {
		m.registry.Unregister(m.uptimeGauge)
		m.uptimeGauge = nil
	}
	return nil
}

// registerOrReuseGauge registers gv with reg; on AlreadyRegisteredError
// it returns the live collector so the returned pointer always matches
// what the registry actually scrapes. Returns nil on any other error.
func registerOrReuseGauge(reg *prometheus.Registry, gv *prometheus.GaugeVec) *prometheus.GaugeVec {
	if err := reg.Register(gv); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := are.ExistingCollector.(*prometheus.GaugeVec); ok {
				return existing
			}
		}
		return nil
	}
	return gv
}

func registerOrReuseCounter(reg *prometheus.Registry, cv *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(cv); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := are.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing
			}
		}
		return nil
	}
	return cv
}

// registerOrReuseCollector registers any prometheus.Collector (Gauge,
// GaugeFunc, Counter, etc.) and on AlreadyRegisteredError returns the
// live collector. Used for collectors whose concrete type is not known
// at call site (e.g. GaugeFunc, which does not implement Gauge).
func registerOrReuseCollector(reg *prometheus.Registry, c prometheus.Collector) prometheus.Collector {
	if err := reg.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return are.ExistingCollector
		}
		return nil
	}
	return c
}

// Mount implements component.Router. Installs the Prometheus HTTP
// handler at the configured path on the supplied gin router.
func (m *MetricsComponent) Mount(router any) error {
	r, ok := router.(interface {
		GET(string, ...gin.HandlerFunc) gin.IRoutes
	})
	if !ok {
		return fmt.Errorf("metrics: Mount expected a gin router, got %T", router)
	}
	r.GET(m.path, gin.WrapH(promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})))
	return nil
}

// PrometheusRegistry returns the underlying registry so other
// components and user code can register their own collectors / metrics.
// Callers should register at startup; changes after the first scrape
// are honoured but may produce scraping artefacts.
func (m *MetricsComponent) PrometheusRegistry() *prometheus.Registry { return m.registry }

// Path returns the configured endpoint path. Useful in tests.
func (m *MetricsComponent) Path() string { return m.path }
