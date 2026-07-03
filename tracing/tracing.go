// Package tracing is the chok v2 OpenTelemetry module: it owns the
// SDK TracerProvider lifecycle (build at Init when enabled, install as
// the otel global, drain at Close). The per-request server spans come
// from middleware.Tracing, which web.Module wires automatically when
// this module is assembled and enabled — discovery is by role
// (Enabled/ServiceName), never by import (SPEC §2.2, §6).
package tracing

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/zynthara/chok/v2/kernel"
)

// Options is the "tracing" yaml section. Tracing is opt-in
// (enabled: false by default — it has an external side effect, the
// exporter); every field shapes provider construction, so the whole
// section is restart-only.
type Options struct {
	Enabled bool `mapstructure:"enabled" default:"false"`

	// ServiceName is the service.name resource attribute (and the
	// tracer name the HTTP middleware uses). Empty falls back to "chok".
	ServiceName string `mapstructure:"service_name" reload:"restart"`

	// Exporter picks the span exporter: "stdout" (dev, human-readable)
	// or "otlp" (OTLP/HTTP to OTLPEndpoint). Unknown values fail
	// validation — silent "traces go nowhere" is the failure mode this
	// guards.
	Exporter string `mapstructure:"exporter" default:"stdout" reload:"restart"`

	// OTLPEndpoint is the collector URL for exporter "otlp"
	// (e.g. "http://localhost:4318"). TLS is not configured here;
	// HTTPS collectors arrange credentials out-of-band.
	OTLPEndpoint string `mapstructure:"otlp_endpoint" reload:"restart"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	switch o.Exporter {
	case "stdout", "otlp":
	default:
		return fmt.Errorf("tracing: unknown exporter %q (use \"stdout\" or \"otlp\")", o.Exporter)
	}
	if o.Exporter == "otlp" && o.OTLPEndpoint != "" && !strings.Contains(o.OTLPEndpoint, "://") {
		return fmt.Errorf("tracing: otlp_endpoint must be a URL, got %q", o.OTLPEndpoint)
	}
	return nil
}

// Module returns the tracing component for chok.Use.
func Module() kernel.Component {
	return &Component{noop: noop.NewTracerProvider()}
}

// Component owns the application's TracerProvider. Exported so peers
// can instrument explicitly:
//
//	tc, ok := chok.Get[*tracing.Component](k, "tracing")
//	tracer := tc.TracerProvider().Tracer("worker")
type Component struct {
	opts Options
	tp   *sdktrace.TracerProvider
	noop trace.TracerProvider
}

func (c *Component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "tracing",
		ConfigKey: "tracing",
		Options:   Options{},
	}
}

// Init builds the TracerProvider when enabled and installs it as the
// otel global. The exporter and resource are built from a detached
// context so the Init deadline does not leak into the exporter's
// long-lived retry/connection loops.
func (c *Component) Init(ctx context.Context, k kernel.Kernel) error {
	if err := k.Config().Section("tracing", &c.opts); err != nil {
		return fmt.Errorf("tracing: decode section: %w", err)
	}
	if !c.opts.Enabled {
		return nil
	}

	detached := context.WithoutCancel(ctx)
	exporter, err := buildExporter(detached, c.opts)
	if err != nil {
		return fmt.Errorf("tracing: exporter: %w", err)
	}

	res, err := resource.New(detached,
		resource.WithAttributes(semconv.ServiceName(c.ServiceName())),
	)
	if err != nil {
		return fmt.Errorf("tracing: resource: %w", err)
	}

	c.tp = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(c.tp)
	return nil
}

// Close flushes pending spans and shuts down the exporter, bounded by
// ctx. The otel global flips to noop BEFORE Shutdown: concurrent
// instrumentation must land on a working provider while draining.
func (c *Component) Close(ctx context.Context) error {
	if c.tp == nil {
		return nil
	}
	tp := c.tp
	otel.SetTracerProvider(c.noop)
	c.tp = nil
	if err := tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("tracing: close: %w", err)
	}
	return nil
}

// Enabled reports whether a real TracerProvider is installed — the
// role signal web.Module checks before adding the HTTP span middleware.
func (c *Component) Enabled() bool { return c.tp != nil }

// ServiceName returns the configured service name, "chok" as fallback
// — the tracer name for middleware.Tracing.
func (c *Component) ServiceName() string {
	if c.opts.ServiceName != "" {
		return c.opts.ServiceName
	}
	return "chok"
}

// TracerProvider returns the live provider, or a noop provider when
// tracing is disabled, so callers instrument unconditionally.
func (c *Component) TracerProvider() trace.TracerProvider {
	if c.tp != nil {
		return c.tp
	}
	return c.noop
}

// buildExporter picks the exporter from Options. Split out so tests
// hit each branch without constructing a full TracerProvider.
func buildExporter(ctx context.Context, o Options) (sdktrace.SpanExporter, error) {
	switch o.Exporter {
	case "stdout":
		return stdouttrace.New(stdouttrace.WithPrettyPrint())
	case "otlp":
		opts := []otlptracehttp.Option{otlptracehttp.WithInsecure()}
		if o.OTLPEndpoint != "" {
			opts = append(opts, otlptracehttp.WithEndpointURL(o.OTLPEndpoint))
		}
		return otlptracehttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown exporter %q (use \"stdout\" or \"otlp\")", o.Exporter)
	}
}
