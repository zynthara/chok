package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/internal/apptest"
	"github.com/zynthara/chok/internal/conf"
)

// The package's tests replace the exporters through these options, which
// production code cannot reach, and export synchronously.

func spanExporterOption(e sdktrace.SpanExporter) Option { return func(o *options) { o.spans = e } }
func logExporterOption(e sdklog.Exporter) Option        { return func(o *options) { o.logs = e } }

// memLogs keeps the exported log records.
type memLogs struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

func (m *memLogs) Export(_ context.Context, recs []sdklog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range recs {
		m.recs = append(m.recs, r.Clone())
	}
	return nil
}
func (*memLogs) Shutdown(context.Context) error   { return nil }
func (*memLogs) ForceFlush(context.Context) error { return nil }

func (m *memLogs) records() []sdklog.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sdklog.Record(nil), m.recs...)
}

// metricsOn is a Config with the metrics on, as chok.Load gives it.
func metricsOn() Config {
	var c Config
	c.Metrics.Enabled = true
	return c
}

// quiet keeps telemetry's own reports out of the test output.
func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newTel builds a Telemetry that the test closes.
func newTel(t *testing.T, cfg Config, opts ...Option) *Telemetry {
	t.Helper()
	tel, err := New(cfg, append([]Option{Logger(quiet())}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Close(context.WithoutCancel(t.Context())) })
	return tel
}

// tracing returns a Config with tracing on and an exporter that keeps the
// spans in memory.
func tracing(t *testing.T, cfg Config) (*Telemetry, *tracetest.InMemoryExporter) {
	t.Helper()
	spans := tracetest.NewInMemoryExporter()
	cfg.Tracing.Enabled = true
	return newTel(t, cfg, spanExporterOption(spans)), spans
}

// metric returns the value of a series of family name whose labels
// include want: a counter's or gauge's value, a histogram's sample count.
func metric(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
	series:
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			for k, v := range want {
				if labels[k] != v {
					continue series
				}
			}
			switch {
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount()), true
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue(), true
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// scrape serves a GET of path through h and returns the text.
func scrape(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, rec.Code)
	}
	return rec.Body.String()
}

// testBase is a Base with every default and nothing listening.
func testBase(t *testing.T) chok.Base {
	t.Helper()
	var b chok.Base
	if err := conf.Defaults(&b); err != nil {
		t.Fatal(err)
	}
	b.HTTP.Enabled = false
	b.HTTP.DrainDelay = 0
	return b
}

func TestConfig_Validate(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Config)
		want string
	}{
		"unknown exporter": {func(c *Config) { c.Tracing.Exporter = "jaeger" }, "exporter"},
		"ratio over one":   {func(c *Config) { c.Tracing.Enabled, c.Tracing.SampleRatio = true, 1.5 }, "sample_ratio"},
		"negative ratio":   {func(c *Config) { c.Tracing.Enabled, c.Tracing.SampleRatio = true, -0.1 }, "sample_ratio"},
		"endpoint without http": {func(c *Config) {
			c.Tracing.Enabled, c.Tracing.Exporter, c.Tracing.Endpoint = true, "otlp", "localhost:4318"
		}, "endpoint"},
		"endpoint with a signal": {func(c *Config) { c.Logs.OTel, c.Tracing.Endpoint = true, "http://collector:4318/v1/traces" }, "base URL"},
		"metrics path":           {func(c *Config) { c.Metrics.Enabled, c.Metrics.Path = true, "metrics" }, "metrics.path"},
		"metrics addr":           {func(c *Config) { c.Metrics.Enabled, c.Metrics.Addr = true, "9464" }, "metrics.addr"},
	} {
		c := metricsOn().withDefaults()
		tc.edit(&c)
		err := conf.Check(&c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error naming %q", name, err, tc.want)
		}
		if _, err := New(c); err == nil {
			t.Errorf("%s: New accepts it", name)
		}
	}
	// What is off is not checked: a disabled tracing section may keep a
	// stale endpoint.
	c := Config{}.withDefaults()
	c.Tracing.Endpoint, c.Tracing.SampleRatio = "not a url", 7
	if err := conf.Check(&c); err != nil {
		t.Fatalf("tracing off: %v", err)
	}
}

func TestConfig_LoadsWithDefaults(t *testing.T) {
	type appConfig struct {
		chok.Base
		Telemetry Config `yaml:"telemetry"`
	}
	t.Setenv("TLT_TELEMETRY_TRACING_ENABLED", "true")
	cfg, err := chok.Load[appConfig]("tlt", chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := metricsOn().withDefaults()
	want.Tracing.Enabled = true
	if cfg.Telemetry != want {
		t.Fatalf("loaded %+v, want %+v", cfg.Telemetry, want)
	}
	// An unknown exporter fails the load and names its key.
	t.Setenv("TLT_TELEMETRY_TRACING_EXPORTER", "jaeger")
	if _, err := chok.Load[appConfig]("tlt", chok.Args(nil)); err == nil || !strings.Contains(err.Error(), "telemetry.tracing.exporter") {
		t.Fatalf("exporter jaeger: %v", err)
	}
}

func TestNew_DefaultsZeroFields(t *testing.T) {
	tel := newTel(t, Config{})
	c := tel.cfg
	if c.Metrics.Enabled || c.Metrics.Addr != "127.0.0.1:9464" || c.Metrics.Path != "/metrics" || c.Tracing.Exporter != "stdout" ||
		c.Tracing.Endpoint != "http://localhost:4318" || c.Tracing.SampleRatio != 1 {
		t.Fatalf("defaults in code: %+v", c)
	}
	if tel.Name() != "telemetry" || tel.Health(t.Context()) != nil {
		t.Fatal("Name is telemetry and Health is nil")
	}
	if fams, err := tel.Registry().Gather(); err != nil || len(fams) != 0 {
		t.Fatalf("with metrics off the registry stays empty: %d families, %v", len(fams), err)
	}
}

func TestMetrics_ExposesRuntimeAndProcess(t *testing.T) {
	tel := newTel(t, metricsOn())
	body := scrape(t, tel.metricsHandler(), "/metrics")
	for _, want := range []string{"go_goroutines ", "go_memstats_alloc_bytes ", "process_start_time_seconds "} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics lacks %q:\n%s", want, body)
		}
	}
}

func TestMetricsHandler_ServesTheConfiguredPath(t *testing.T) {
	cfg := metricsOn()
	cfg.Metrics.Path = "/internal/metrics"
	h := newTel(t, cfg).metricsHandler()
	if body := scrape(t, h, "/internal/metrics"); !strings.Contains(body, "go_goroutines") {
		t.Fatalf("the configured path serves the registry:\n%s", body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/metrics is not served when the path is elsewhere: %d", rec.Code)
	}
}

// running runs an App with tel until the test ends and returns it once
// its start phase is over.
func running(t *testing.T, base chok.Base, tel *Telemetry) *chok.App {
	t.Helper()
	app := chok.New(base, chok.Logger(quiet()))
	app.Use(tel)
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	for deadline := time.Now().Add(10 * time.Second); !apptest.Ready(app); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the App did not start")
		}
	}
	return app
}

// get fetches url and returns the status and the body.
func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// TestStart_ServesOnItsOwnListener: with the App serving HTTP, the
// metrics answer on their own address and path only; the App's port does
// not serve them.
func TestStart_ServesOnItsOwnListener(t *testing.T) {
	base := testBase(t)
	base.HTTP.Enabled, base.HTTP.Addr = true, "127.0.0.1:0"
	cfg := metricsOn()
	cfg.Metrics.Addr = "127.0.0.1:0"
	tel := newTel(t, cfg)
	app := running(t, base, tel)

	addr := tel.addr.Load()
	if addr == nil {
		t.Fatal("no metrics listener")
	}
	if code, body := get(t, "http://"+*addr+"/metrics"); code != http.StatusOK || !strings.Contains(body, "go_goroutines ") {
		t.Fatalf("metrics listener: GET /metrics = %d:\n%s", code, body)
	}
	if code, _ := get(t, "http://"+*addr+"/healthz"); code != http.StatusNotFound {
		t.Fatalf("metrics listener: GET /healthz = %d, want 404", code)
	}
	if code, _ := get(t, "http://"+app.Addr()+"/metrics"); code != http.StatusNotFound {
		t.Fatalf("the App's port: GET /metrics = %d, want 404", code)
	}
}

// TestStart_NoListenerWithoutHTTP: a worker process, or an App under
// choktest, opens no port for the metrics; they are still recorded.
func TestStart_NoListenerWithoutHTTP(t *testing.T) {
	cfg := metricsOn()
	cfg.Metrics.Addr = "127.0.0.1:0"
	tel := newTel(t, cfg)
	running(t, testBase(t), tel)
	if addr := tel.addr.Load(); addr != nil {
		t.Fatalf("http.enabled false, yet the metrics listen on %s", *addr)
	}
	if fams, err := tel.Registry().Gather(); err != nil || len(fams) == 0 {
		t.Fatalf("the metrics are still recorded: %d families, %v", len(fams), err)
	}
}

// TestStart_AddressInUse fails and names the key that picks another.
func TestStart_AddressInUse(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	cfg := metricsOn()
	cfg.Metrics.Addr = taken.Addr().String()
	err = newTel(t, cfg).Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "telemetry.metrics.addr") {
		t.Fatalf("Start on a taken address: %v", err)
	}
}

func TestRegistry_TakesCustomCollectors(t *testing.T) {
	tel := newTel(t, metricsOn())
	orders := prometheus.NewCounter(prometheus.CounterOpts{Name: "shop_orders_total", Help: "Orders placed."})
	tel.Registry().MustRegister(orders)
	orders.Add(3)
	if body := scrape(t, tel.metricsHandler(), "/metrics"); !strings.Contains(body, "shop_orders_total 3") {
		t.Fatalf("the application's collector is served:\n%s", body)
	}
}

func TestTracerProvider_NoopWhenTracingIsOff(t *testing.T) {
	tel := newTel(t, metricsOn())
	_, span := tel.TracerProvider().Tracer("t").Start(t.Context(), "x")
	defer span.End()
	if span.IsRecording() || span.SpanContext().IsValid() {
		t.Fatal("tracing off hands out a no-op provider")
	}
	if tel.sdk != nil {
		t.Fatal("no SDK provider is built")
	}
}

func TestTracing_StdoutExporterWritesSpans(t *testing.T) {
	// The stdout exporter writes to os.Stdout as it is when New runs.
	out, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = out
	var cfg Config
	cfg.Tracing.Enabled = true
	tel, err := New(cfg, Logger(quiet()))
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tel.TracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("tracing on builds the SDK provider, got %T", tel.TracerProvider())
	}
	_, span := tel.TracerProvider().Tracer("t").Start(t.Context(), "stdout-span")
	span.End()
	if err := tel.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	data, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"Name":"stdout-span"`) {
		t.Fatalf("Close flushes the span to stdout, one JSON object per span:\n%s", data)
	}
}

func TestTracing_ServiceName(t *testing.T) {
	cfg := Config{}
	cfg.Tracing.ServiceName = "blog"
	named, spans := tracing(t, cfg)
	unnamed, spans2 := tracing(t, Config{})
	for _, tel := range []*Telemetry{named, unnamed} {
		_, span := tel.TracerProvider().Tracer("t").Start(t.Context(), "x")
		span.End()
	}
	// Without service_name the resource keeps OpenTelemetry's default:
	// OTEL_SERVICE_NAME when the process starts with it, else
	// unknown_service:<executable>.
	fallback, _ := resource.Default().Set().Value(semconv.ServiceNameKey)
	if v := fallback.AsString(); os.Getenv("OTEL_SERVICE_NAME") == "" && v != "unknown_service:"+filepath.Base(os.Args[0]) {
		t.Fatalf("the SDK's default service.name is %q", v)
	}
	for _, tc := range []struct {
		spans *tracetest.InMemoryExporter
		want  string
	}{{spans, "blog"}, {spans2, fallback.AsString()}} {
		got := tc.spans.GetSpans()
		if len(got) != 1 {
			t.Fatalf("%d spans", len(got))
		}
		name, _ := got[0].Resource.Set().Value(semconv.ServiceNameKey)
		if name.AsString() != tc.want {
			t.Fatalf("service.name = %q, want %q", name.AsString(), tc.want)
		}
	}
}

func TestTracing_SampleRatioDefersToTheParent(t *testing.T) {
	cfg := Config{}
	cfg.Tracing.SampleRatio = 1e-12 // practically no new trace is recorded
	tel, spans := tracing(t, cfg)
	h := tel.Middleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/a", nil))
	if n := len(spans.GetSpans()); n != 0 {
		t.Fatalf("a new trace is sampled at sample_ratio: %d spans", n)
	}
	req := httptest.NewRequest(http.MethodGet, "/b", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if n := len(spans.GetSpans()); n != 1 {
		t.Fatalf("a request whose parent was sampled is recorded: %d spans", n)
	}
}

func TestClose_FlushesAndRestoresTheGlobalProvider(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	var cfg Config
	cfg.Tracing.Enabled = true
	tel, err := New(cfg, Logger(quiet()), spanExporterOption(spans))
	if err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != trace.TracerProvider(tel.sdk) {
		t.Fatal("tracing on installs the provider as the OpenTelemetry global")
	}
	if err := tel.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() == trace.TracerProvider(tel.sdk) {
		t.Fatal("Close puts a no-op provider back")
	}
	_, span := tel.TracerProvider().Tracer("t").Start(t.Context(), "after")
	span.End()
	if span.IsRecording() || len(spans.GetSpans()) != 0 {
		t.Fatal("a closed Telemetry records nothing")
	}
	if err := tel.Close(t.Context()); err != nil {
		t.Fatalf("Close is idempotent: %v", err)
	}
}

func TestClose_WhenOffIsANoOp(t *testing.T) {
	tel, err := New(Config{}, Logger(quiet()))
	if err != nil {
		t.Fatal(err)
	}
	before := otel.GetTracerProvider()
	if err := tel.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != before {
		t.Fatal("with tracing off the global provider is left alone")
	}
	if err := tel.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestLogs_OTelCarriesTheAppLogs(t *testing.T) {
	// The App builds its loggers from the log section (an injected one is
	// not copied); they write to a file here.
	base := testBase(t)
	base.Log.Output = []string{filepath.Join(t.TempDir(), "app.log")}
	before := slog.Default()
	app := chok.New(base)
	t.Cleanup(func() { slog.SetDefault(before) })

	spans := tracetest.NewInMemoryExporter()
	logs := &memLogs{}
	cfg := metricsOn()
	cfg.Tracing.Enabled = true
	cfg.Logs.OTel = true
	tel := newTel(t, cfg, spanExporterOption(spans), logExporterOption(logs))
	app.Use(tel)
	chok.GET(app.Router(), "/hello", func(ctx context.Context, _ *chok.None) (chok.None, error) {
		chok.LoggerFrom(ctx).InfoContext(ctx, "inside the handler", "user", "alice")
		return chok.None{}, nil
	})
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.Header.Set("X-Request-Id", "req-7")
	app.Handler().ServeHTTP(httptest.NewRecorder(), req)

	found := map[string]sdklog.Record{}
	for _, r := range logs.records() {
		found[r.Body().AsString()] = r
	}
	inside, ok := found["inside the handler"]
	if !ok {
		t.Fatalf("the handler's record is exported; got %d records", len(found))
	}
	if inside.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || !inside.SpanID().IsValid() {
		t.Fatalf("the record carries the request's trace: %s %s", inside.TraceID(), inside.SpanID())
	}
	attrs := map[string]string{}
	inside.WalkAttributes(func(kv attribute.KeyValue) bool { attrs[string(kv.Key)] = kv.Value.String(); return true })
	if attrs["request_id"] != "req-7" || attrs["user"] != "alice" {
		t.Fatalf("the record keeps the logger's and the call's attributes: %v", attrs)
	}
	if _, ok := found["access"]; !ok {
		t.Fatal("the access log is exported too")
	}
	// After Close the App's records stay local.
	if err := tel.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	n := len(logs.records())
	app.Logger().Info("after close")
	if len(logs.records()) != n {
		t.Fatal("Close stops copying the logs")
	}
}
