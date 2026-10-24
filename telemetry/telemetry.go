package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/conf"
	"github.com/zynthara/chok/v3/internal/logtap"
)

// scope names the instrumentation in spans and log records.
const scope = "github.com/zynthara/chok/v3/telemetry"

// Telemetry is the observability battery: a Prometheus registry, an
// OpenTelemetry tracer provider and, with logs.otel, a bridge from the
// App's logs to OpenTelemetry Logs. Register it with app.Use: its
// middleware records every request (chok.Instrumenter), its Mount serves
// the metrics, its Close flushes what is buffered.
type Telemetry struct {
	cfg    Config
	logger *slog.Logger
	reg    *prometheus.Registry
	prop   propagation.TextMapPropagator

	tp     trace.TracerProvider     // the SDK's, or a no-op one when tracing is off
	sdk    *sdktrace.TracerProvider // nil when tracing is off
	tracer trace.Tracer
	logs   *sdklog.LoggerProvider // nil when logs.otel is off
	untap  func()

	http    *httpMetrics // nil when metrics are off
	db      *dbMetrics   // nil when metrics are off
	mu      sync.Mutex
	dbs     map[*db.DB]bool // instrumented handles
	poolIDs map[string]bool // db_name labels of the pool collectors
	closed  bool
}

// Option configures New.
type Option func(*options)

type options struct {
	logger *slog.Logger
	// spans and logs replace the exporters the Config names, and export
	// synchronously; the package's tests set them.
	spans sdktrace.SpanExporter
	logs  sdklog.Exporter
}

// Logger is where telemetry reports its own trouble, such as an export
// that failed; the default is slog.Default().
func Logger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// New builds the battery from cfg. With tracing on it installs its
// tracer provider and the W3C trace-context propagator as the
// OpenTelemetry globals, so instrumented clients join the same traces;
// with tracing or logs.otel on, OpenTelemetry's own errors (a failed
// export) are logged through the Logger option's logger. Spans and log
// records are exported in the background, in batches; Close flushes what
// is buffered and stops the exporters.
func New(cfg Config, opts ...Option) (*Telemetry, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	cfg = cfg.withDefaults()
	if err := conf.Check(&cfg); err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	t := &Telemetry{
		cfg: cfg, logger: o.logger, reg: prometheus.NewRegistry(),
		prop:    propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
		tp:      noop.NewTracerProvider(),
		dbs:     map[*db.DB]bool{},
		poolIDs: map[string]bool{},
	}
	// The exporters first: they are what can fail, and until the
	// providers start there is nothing to undo.
	var spans sdktrace.SpanExporter
	var logs sdklog.Exporter
	var err error
	if cfg.Tracing.Enabled {
		if spans, err = spanExporter(cfg, o.spans); err != nil {
			return nil, err
		}
	}
	if cfg.Logs.OTel {
		if logs, err = logExporter(cfg, o.logs); err != nil {
			return nil, err
		}
	}
	if cfg.Metrics.Enabled {
		if err := t.registerMetrics(); err != nil {
			return nil, err
		}
	}
	res, err := resource.Merge(resource.Default(), serviceResource(cfg.Tracing.ServiceName))
	if err != nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}
	if spans != nil || logs != nil {
		reportOTelErrors(t.logger)
	}
	if spans != nil {
		process := sdktrace.WithBatcher(spans)
		if o.spans != nil {
			process = sdktrace.WithSyncer(spans)
		}
		t.sdk = sdktrace.NewTracerProvider(process, sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Tracing.SampleRatio))))
		t.tp = t.sdk
		otel.SetTracerProvider(t.sdk)
		otel.SetTextMapPropagator(t.prop)
	}
	t.tracer = t.tp.Tracer(scope)
	if logs != nil {
		var processor sdklog.Processor = sdklog.NewBatchProcessor(logs)
		if o.logs != nil {
			processor = sdklog.NewSimpleProcessor(logs)
		}
		t.logs = sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(processor))
		t.untap = logtap.Install(otelslog.NewHandler(scope, otelslog.WithLoggerProvider(t.logs)))
	}
	return t, nil
}

// MustNew is New that panics on error.
func MustNew(cfg Config, opts ...Option) *Telemetry {
	t, err := New(cfg, opts...)
	if err != nil {
		panic(err)
	}
	return t
}

// serviceResource carries service.name when the configuration names it;
// resource.Default supplies OTEL_SERVICE_NAME or the executable's name.
func serviceResource(name string) *resource.Resource {
	if name == "" {
		return resource.Empty()
	}
	return resource.NewSchemaless(semconv.ServiceName(name))
}

// constructionCtx is the context the OTLP exporters are built with: New
// has no caller context, and the exporters keep it only as the template
// of their requests, each export carrying its own.
func constructionCtx() context.Context { return context.Background() }

// spanExporter is the exporter tracing.exporter names, or the override
// the package's tests give.
func spanExporter(cfg Config, override sdktrace.SpanExporter) (sdktrace.SpanExporter, error) {
	if override != nil {
		return override, nil
	}
	if cfg.Tracing.Exporter == "otlp" {
		exp, err := otlptracehttp.New(constructionCtx(), otlptracehttp.WithEndpointURL(signalURL(cfg.Tracing.Endpoint, "traces")))
		if err != nil {
			return nil, fmt.Errorf("telemetry: otlp span exporter: %w", err)
		}
		return exp, nil
	}
	exp, err := stdouttrace.New(stdouttrace.WithWriter(os.Stdout))
	if err != nil {
		return nil, fmt.Errorf("telemetry: stdout span exporter: %w", err)
	}
	return exp, nil
}

// logExporter posts the log records over OTLP to tracing.endpoint, or is
// the override the package's tests give.
func logExporter(cfg Config, override sdklog.Exporter) (sdklog.Exporter, error) {
	if override != nil {
		return override, nil
	}
	exp, err := otlploghttp.New(constructionCtx(), otlploghttp.WithEndpointURL(signalURL(cfg.Tracing.Endpoint, "logs")))
	if err != nil {
		return nil, fmt.Errorf("telemetry: otlp log exporter: %w", err)
	}
	return exp, nil
}

// OpenTelemetry has one global error handler; it logs through the logger
// of the Telemetry built last, or slog.Default once that one is closed.
var (
	errHandlerOnce sync.Once
	errLogger      atomic.Pointer[slog.Logger]
)

func reportOTelErrors(l *slog.Logger) {
	errLogger.Store(l)
	errHandlerOnce.Do(func() {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			l := errLogger.Load()
			if l == nil {
				l = slog.Default()
			}
			l.Warn("telemetry: opentelemetry: "+err.Error(), "error", err)
		}))
	})
}

func (t *Telemetry) registerMetrics() error {
	t.http = newHTTPMetrics()
	t.db = newDBMetrics()
	cs := []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	}
	cs = append(cs, t.http.collectors()...)
	cs = append(cs, t.db.collectors()...)
	for _, c := range cs {
		if err := t.reg.Register(c); err != nil {
			return fmt.Errorf("telemetry: register metrics: %w", err)
		}
	}
	return nil
}

// Registry returns the Prometheus registry served on metrics.path: the
// HTTP, database, Go runtime and process metrics, and whatever the
// application registers next to them. With metrics off it stays empty
// and is not served.
func (t *Telemetry) Registry() *prometheus.Registry { return t.reg }

// TracerProvider returns the tracer provider, for spans of the
// application's own; with tracing off it is a no-op provider, so
// instrumenting code needs no switch.
func (t *Telemetry) TracerProvider() trace.TracerProvider { return t.tp }

// Mount serves the registry on metrics.path (GET), when metrics are on.
func (t *Telemetry) Mount(r chok.Router) {
	if !t.cfg.Metrics.Enabled {
		return
	}
	h := promhttp.HandlerFor(t.reg, promhttp.HandlerOpts{ErrorLog: promLogger{t.logger}})
	r.Handle(http.MethodGet, t.cfg.Metrics.Path, h)
}

// promLogger reports promhttp's collection and encoding errors.
type promLogger struct{ l *slog.Logger }

func (p promLogger) Println(v ...any) { p.l.Warn("telemetry: metrics: " + fmt.Sprint(v...)) }

// Health reports nil: telemetry's trouble, such as an unreachable
// collector, never takes the process out of rotation; failed exports are
// logged.
func (t *Telemetry) Health(context.Context) error { return nil }

// Name is "telemetry", its name in the health report.
func (t *Telemetry) Name() string { return "telemetry" }

// Close stops copying the logs, then flushes the spans and log records
// still buffered and shuts the exporters down, within ctx. The global
// tracer provider goes back to a no-op one if it is still this
// Telemetry's. Close is idempotent.
func (t *Telemetry) Close(ctx context.Context) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	var errs []error
	errLogger.CompareAndSwap(t.logger, nil)
	if t.untap != nil {
		t.untap()
	}
	if t.logs != nil {
		if err := t.logs.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("telemetry: logs: %w", err))
		}
	}
	if t.sdk != nil {
		if otel.GetTracerProvider() == trace.TracerProvider(t.sdk) {
			otel.SetTracerProvider(noop.NewTracerProvider())
		}
		if err := t.sdk.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("telemetry: tracing: %w", err))
		}
	}
	return errors.Join(errs...)
}
