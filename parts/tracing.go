package parts

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/zynthara/chok/component"
)

// TracingSettings is the flat view of tracing configuration expected
// by TracingComponent. Kept independent of any particular config
// schema so the resolver can map from whatever the user has.
//
// When Enabled is false (or settings is nil) the component still
// Init's successfully but no TracerProvider is installed — callers
// use TracerProvider() which falls back to a noop instance so
// application code can instrument unconditionally.
type TracingSettings struct {
	// Enabled gates everything. Disabled tracing has zero overhead.
	Enabled bool

	// ServiceName is written as the service.name resource attribute.
	// Defaults to "chok" when empty — override in production.
	ServiceName string

	// Exporter picks the span exporter. Supported:
	//   - "stdout" → human-readable console output (dev)
	//   - "otlp"   → OTLP/HTTP to OTLPEndpoint (prod)
	//
	// An unrecognised value errors out of Init to avoid the silent
	// "traces go nowhere" failure mode. Use "" with Enabled=false to
	// disable tracing entirely.
	Exporter string

	// OTLPEndpoint is the collector URL for Exporter="otlp" (e.g.
	// "http://localhost:4318"). Unused by other exporters.
	// TLS is disabled by default; callers that need HTTPS should set
	// the full URL and arrange credentials out-of-band.
	OTLPEndpoint string
}

// TracingResolver extracts TracingSettings from the app config.
type TracingResolver func(appConfig any) *TracingSettings

// TracingComponent owns the OpenTelemetry SDK's TracerProvider for the
// application. It builds the provider at Init (if enabled), installs
// it as the otel global so instrumented code can use otel.Tracer(),
// and Shuts it down cleanly during Close.
//
// No Dependencies / Reload / Healther:
//
//   - TracerProvider config is hot-swappable in theory, but a Reload
//     would require stopping in-flight spans and recreating batchers
//     — the failure modes are subtle enough that restart is the
//     simpler story. If you really need dynamic sampling, layer it
//     via a sampler that reads live config, not via TracingComponent.
//   - An exporter outage doesn't block application traffic; the SDK
//     buffers and drops silently. Surfacing that through /healthz
//     would be misleading, so no Healther.
type TracingComponent struct {
	resolve     TracingResolver
	tp          *sdktrace.TracerProvider
	noop        trace.TracerProvider
	serviceName string // cached at Init for ServiceName() accessor
}

// NewTracingComponent constructs the component. resolve may be nil
// or return nil — both map to "disabled", which is the safe default.
func NewTracingComponent(resolve TracingResolver) *TracingComponent {
	return &TracingComponent{
		resolve: resolve,
		noop:    noop.NewTracerProvider(),
	}
}

// Name implements component.Component.
func (t *TracingComponent) Name() string { return "tracing" }

// ConfigKey implements component.Component.
func (t *TracingComponent) ConfigKey() string { return "tracing" }

// Init builds the TracerProvider when settings are enabled. Unknown
// exporters return an error — silent misconfiguration has too high a
// diagnostic cost.
//
// The exporter and resource are built from a detached context so the
// Init-scoped deadline (Registry default 30s) does not leak into the
// exporter's long-lived retry/connection loops. Without detaching, the
// OTLP exporter's background goroutines inherit a context that is
// cancelled as soon as Init returns.
func (t *TracingComponent) Init(ctx context.Context, k component.Kernel) error {
	if t.resolve == nil {
		return nil
	}
	settings := t.resolve(k.ConfigSnapshot())
	if settings == nil || !settings.Enabled {
		return nil
	}

	detached := context.WithoutCancel(ctx)
	exporter, err := buildTraceExporter(detached, settings)
	if err != nil {
		return fmt.Errorf("tracing init: exporter: %w", err)
	}

	serviceName := settings.ServiceName
	if serviceName == "" {
		serviceName = "chok"
	}
	t.serviceName = serviceName
	res, err := resource.New(detached,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return fmt.Errorf("tracing init: resource: %w", err)
	}

	t.tp = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(t.tp)
	return nil
}

// Close flushes pending spans and shuts down the exporter. Bounded
// by the provided ctx so slow exporters can't block shutdown
// indefinitely.
//
// Swap the otel global to the noop provider BEFORE calling Shutdown:
// if Shutdown blocks (e.g. OTLP collector unreachable and ctx has no
// deadline), instrumentation code calling otel.Tracer() concurrently
// still needs to land on a working provider. Writing spans into a
// provider that is in the middle of draining is undefined behaviour.
func (t *TracingComponent) Close(ctx context.Context) error {
	if t.tp == nil {
		return nil
	}
	tp := t.tp
	// Flip to noop first so any concurrent instrumentation code
	// gets a safe provider while Shutdown is in flight.
	otel.SetTracerProvider(t.noop)
	t.tp = nil
	if err := tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("tracing close: %w", err)
	}
	return nil
}

// Enabled reports whether a real TracerProvider was created (i.e. tracing
// config was present and Enabled=true). Used by HTTPComponent to decide
// whether to add the Tracing middleware.
func (t *TracingComponent) Enabled() bool { return t.tp != nil }

// ServiceName returns the configured service name for use as the tracer
// name in middleware. Returns "chok" as fallback. Cached at Init.
func (t *TracingComponent) ServiceName() string {
	if t.serviceName != "" {
		return t.serviceName
	}
	return "chok"
}

// TracerProvider returns the live provider, or a noop provider when
// tracing is disabled. This lets application code call
// comp.TracerProvider().Tracer("x") unconditionally without nil
// checks — disabled tracing silently drops spans.
func (t *TracingComponent) TracerProvider() trace.TracerProvider {
	if t.tp != nil {
		return t.tp
	}
	return t.noop
}

// buildTraceExporter picks the exporter based on settings.Exporter.
// Split out so tests can hit each branch without constructing a full
// TracerProvider.
func buildTraceExporter(ctx context.Context, s *TracingSettings) (sdktrace.SpanExporter, error) {
	switch s.Exporter {
	case "stdout":
		return stdouttrace.New(stdouttrace.WithPrettyPrint())
	case "otlp":
		opts := []otlptracehttp.Option{otlptracehttp.WithInsecure()}
		if s.OTLPEndpoint != "" {
			opts = append(opts, otlptracehttp.WithEndpointURL(s.OTLPEndpoint))
		}
		return otlptracehttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown exporter %q (use \"stdout\" or \"otlp\")", s.Exporter)
	}
}
