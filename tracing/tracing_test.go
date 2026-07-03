package tracing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/kernel/event"
	"github.com/zynthara/chok/v2/log"
)

type stubKernel struct {
	store *conf.Store
}

func (k *stubKernel) Config() *conf.Snapshot { return k.store.Snapshot() }
func (k *stubKernel) Logger() kernel.Logger  { return log.Empty() }
func (k *stubKernel) Bus() *event.Bus        { return event.NewBus() }
func (k *stubKernel) Lookup(string, ...string) (kernel.Component, bool) {
	return nil, false
}
func (k *stubKernel) Health(context.Context) kernel.HealthReport {
	return kernel.HealthReport{}
}
func (k *stubKernel) Ready(context.Context) error          { return nil }
func (k *stubKernel) Components() []kernel.ComponentStatus { return nil }

func kernelWith(t *testing.T, yaml string) *stubKernel {
	t.Helper()
	loader := conf.NewLoader("tracingtest", "TRACINGTEST")
	if err := loader.Register("tracing", Options{}); err != nil {
		t.Fatal(err)
	}
	if yaml != "" {
		dir := t.TempDir()
		p := filepath.Join(dir, "tracingtest.yaml")
		if err := os.WriteFile(p, []byte(strings.TrimSpace(yaml)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		loader.SetPath(p)
	}
	store, err := conf.NewStore(loader)
	if err != nil {
		t.Fatal(err)
	}
	return &stubKernel{store: store}
}

func TestTracing_Disabled_ReturnsNoopProvider(t *testing.T) {
	c := Module().(*Component)
	if err := c.Init(context.Background(), kernelWith(t, "")); err != nil {
		t.Fatal(err)
	}
	if c.Enabled() {
		t.Fatal("default (enabled:false) must not build a provider")
	}
	if _, ok := c.TracerProvider().(noop.TracerProvider); !ok {
		t.Fatalf("disabled tracing must hand out the noop provider, got %T", c.TracerProvider())
	}
}

func TestTracing_StdoutExporter_Builds(t *testing.T) {
	c := Module().(*Component)
	err := c.Init(context.Background(), kernelWith(t, `
tracing:
  enabled: true
  exporter: stdout
  service_name: web-test
`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	if !c.Enabled() {
		t.Fatal("enabled stdout tracing must build a provider")
	}
	if c.ServiceName() != "web-test" {
		t.Fatalf("service name = %q", c.ServiceName())
	}
	if _, ok := c.TracerProvider().(noop.TracerProvider); ok {
		t.Fatal("live provider expected, got noop")
	}
}

func TestTracing_UnknownExporter_FailsValidation(t *testing.T) {
	loader := conf.NewLoader("tracingtest", "TRACINGTEST")
	if err := loader.Register("tracing", Options{}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "tracingtest.yaml")
	os.WriteFile(p, []byte("tracing:\n  enabled: true\n  exporter: jaeger\n"), 0o644)
	loader.SetPath(p)

	if _, err := conf.NewStore(loader); err == nil {
		t.Fatal("unknown exporter must fail config validation")
	} else if !strings.Contains(err.Error(), "unknown exporter") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTracing_Close_WhenDisabled_NoOp(t *testing.T) {
	c := Module().(*Component)
	if err := c.Init(context.Background(), kernelWith(t, "")); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("disabled close must be a no-op, got %v", err)
	}
}

func TestTracing_CloseSwapsToNoopAndIsIdempotent(t *testing.T) {
	c := Module().(*Component)
	err := c.Init(context.Background(), kernelWith(t, `
tracing:
  enabled: true
  exporter: stdout
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Enabled() {
		t.Fatal("closed component must report disabled")
	}
	if _, ok := c.TracerProvider().(noop.TracerProvider); !ok {
		t.Fatal("closed component must hand out the noop provider")
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("second close must be a no-op, got %v", err)
	}
}

func TestTracing_DefaultServiceName(t *testing.T) {
	c := Module().(*Component)
	if err := c.Init(context.Background(), kernelWith(t, "")); err != nil {
		t.Fatal(err)
	}
	if c.ServiceName() != "chok" {
		t.Fatalf("default service name = %q, want chok", c.ServiceName())
	}
}
