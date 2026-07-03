// Package metrics is the chok v2 Prometheus module: it owns the
// process metrics registry (Go runtime + process collectors included)
// and exposes it over /metrics. Lifecycle gauges come from the event
// bus — layer-two observability with no phase coupling (SPEC §3.5).
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/kernel/event"
)

// Options is the "metrics" yaml section.
type Options struct {
	Enabled bool   `mapstructure:"enabled" default:"true"`
	Path    string `mapstructure:"path"    default:"/metrics" reload:"restart"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Path != "" && !strings.HasPrefix(o.Path, "/") {
		return fmt.Errorf("metrics: path must start with /, got %q", o.Path)
	}
	return nil
}

// Module returns the metrics component for chok.Use.
func Module() kernel.Component {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return &Component{registry: reg}
}

// Component is exported so peers can reach the Prometheus registry:
//
//	mc, ok := chok.Get[*metrics.Component](k, "metrics")
//	mc.Registry().MustRegister(myCollector)
type Component struct {
	registry *prometheus.Registry
	opts     Options

	componentUp *prometheus.GaugeVec
	reloadTotal prometheus.Counter
	startedAt   time.Time

	unsubs []func()
}

// Registry exposes the Prometheus registry for custom collectors.
func (c *Component) Registry() *prometheus.Registry { return c.registry }

func (c *Component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "metrics",
		ConfigKey: "metrics",
		Options:   Options{},
	}
}

func (c *Component) Init(ctx context.Context, k kernel.Kernel) error {
	if err := k.Config().Section("metrics", &c.opts); err != nil {
		return fmt.Errorf("metrics: decode section: %w", err)
	}

	c.componentUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "chok_component_up",
		Help: "1 when the component initialized successfully, 0 after close/degrade.",
	}, []string{"component"})
	c.reloadTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "chok_reload_total",
		Help: "Number of successfully applied config reloads.",
	})
	c.startedAt = time.Now()
	uptime := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "chok_uptime_seconds",
		Help: "Seconds since the metrics component initialized.",
	}, func() float64 { return time.Since(c.startedAt).Seconds() })

	if err := c.registry.Register(c.componentUp); err != nil {
		return fmt.Errorf("metrics: register component gauge: %w", err)
	}
	if err := c.registry.Register(c.reloadTotal); err != nil {
		return fmt.Errorf("metrics: register reload counter: %w", err)
	}
	if err := c.registry.Register(uptime); err != nil {
		return fmt.Errorf("metrics: register uptime gauge: %w", err)
	}

	// Lifecycle taps: bus subscriptions, no veto power by construction.
	bus := k.Bus()
	c.unsubs = append(c.unsubs,
		event.Subscribe(bus, func(_ context.Context, e kernel.ComponentInitialized) {
			c.componentUp.WithLabelValues(e.Key.String()).Set(1)
		}),
		event.Subscribe(bus, func(_ context.Context, e kernel.ComponentDegraded) {
			c.componentUp.WithLabelValues(e.Key.String()).Set(0)
		}),
		event.Subscribe(bus, func(_ context.Context, e kernel.ComponentClosed) {
			c.componentUp.WithLabelValues(e.Key.String()).Set(0)
		}),
		event.Subscribe(bus, func(_ context.Context, e kernel.ReloadApplied) {
			c.reloadTotal.Inc()
		}),
	)
	return nil
}

func (c *Component) Close(ctx context.Context) error {
	for _, u := range c.unsubs {
		u()
	}
	return nil
}

// Mount implements kernel.Mounter.
func (c *Component) Mount(r kernel.Router) error {
	h := promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{})
	r.Handle(http.MethodGet, c.opts.Path, h)
	return nil
}
