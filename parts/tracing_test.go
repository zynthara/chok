package parts

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
)

func TestTracingComponent_Disabled_ReturnsNoopProvider(t *testing.T) {
	c := NewTracingComponent(func(any) *TracingSettings {
		return &TracingSettings{Enabled: false}
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}

	tp := c.TracerProvider()
	if tp == nil {
		t.Fatal("TracerProvider() should never be nil")
	}
	// When disabled, the provider should be the noop singleton —
	// instrumentation code can create spans safely.
	if _, ok := tp.(noop.TracerProvider); !ok {
		t.Fatalf("expected noop provider when disabled, got %T", tp)
	}
}

func TestTracingComponent_NilResolver_Disabled(t *testing.T) {
	c := NewTracingComponent(nil)
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.TracerProvider().(noop.TracerProvider); !ok {
		t.Fatal("nil resolver should yield disabled (noop) provider")
	}
}

func TestTracingComponent_StdoutExporter_Builds(t *testing.T) {
	c := NewTracingComponent(func(any) *TracingSettings {
		return &TracingSettings{
			Enabled:     true,
			ServiceName: "unit-test",
			Exporter:    "stdout",
		}
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatalf("stdout exporter should build cleanly, got %v", err)
	}

	// TracerProvider must be the real SDK instance, not the noop
	// fallback.
	tp := c.TracerProvider()
	if _, ok := tp.(noop.TracerProvider); ok {
		t.Fatal("expected SDK TracerProvider for enabled tracing")
	}

	// Emitting a span exercises the whole pipeline without asserting
	// on stdout contents (parallel tests would race on stdout).
	_, span := tp.Tracer("test").Start(context.Background(), "op")
	span.End()

	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close should flush cleanly, got %v", err)
	}
}

func TestTracingComponent_UnknownExporter_Errors(t *testing.T) {
	c := NewTracingComponent(func(any) *TracingSettings {
		return &TracingSettings{
			Enabled:  true,
			Exporter: "chatty",
		}
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err == nil {
		t.Fatal("unknown exporter should surface error from Init")
	}
}

func TestTracingComponent_Close_WhenDisabled_NoOp(t *testing.T) {
	c := NewTracingComponent(func(any) *TracingSettings {
		return &TracingSettings{Enabled: false}
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("disabled Close should be nil, got %v", err)
	}
}

func TestTracingComponent_DefaultServiceName(t *testing.T) {
	c := NewTracingComponent(func(any) *TracingSettings {
		return &TracingSettings{
			Enabled:  true,
			Exporter: "stdout",
		}
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatalf("default service name should be allowed, got %v", err)
	}
	_ = c.Close(context.Background())
}
