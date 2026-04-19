package parts

import (
	"context"
	"testing"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/log"
)

// mockKernel is a minimal Kernel for testing Components in isolation.
type mockKernel struct {
	cfg    any
	logger log.Logger
	store  map[string]component.Component
	hooks  map[component.Event][]component.Hook
}

func newMockKernel(cfg any) *mockKernel {
	return &mockKernel{
		cfg:    cfg,
		logger: log.Empty(),
		store:  map[string]component.Component{},
		hooks:  map[component.Event][]component.Hook{},
	}
}

func (m *mockKernel) Config() any         { return m.cfg }
func (m *mockKernel) ConfigSnapshot() any { return m.cfg }
func (m *mockKernel) Logger() log.Logger  { return m.logger }
func (m *mockKernel) Get(name string) component.Component {
	return m.store[name]
}
func (m *mockKernel) On(e component.Event, h component.Hook) {
	m.hooks[e] = append(m.hooks[e], h)
}
func (m *mockKernel) Health(_ context.Context) component.HealthReport {
	return component.HealthReport{Status: component.HealthOK}
}
func (m *mockKernel) ReadyCheck(_ context.Context) error { return nil }

type testCfg struct {
	Log *config.SlogOptions
}

func TestLoggerComponent_Init_NoConfig_UsesEmpty(t *testing.T) {
	c := NewLoggerComponent(func(any) *config.SlogOptions { return nil })
	k := newMockKernel(&testCfg{})

	if err := c.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	if c.Logger() == nil {
		t.Fatal("Logger() should never be nil after Init")
	}
	// Empty logger accepts all calls without panic.
	c.Logger().Info("hello")
}

func TestLoggerComponent_Init_WithConfig(t *testing.T) {
	cfg := &testCfg{Log: &config.SlogOptions{
		Level:  "info",
		Format: "json",
		Output: []string{"stdout"},
	}}
	c := NewLoggerComponent(func(a any) *config.SlogOptions {
		return a.(*testCfg).Log
	})
	k := newMockKernel(cfg)

	if err := c.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	c.Logger().Info("hello") // should not panic
}

func TestLoggerComponent_Reload_ChangesLevel(t *testing.T) {
	cfg := &testCfg{Log: &config.SlogOptions{
		Level:  "info",
		Format: "json",
		Output: []string{"stdout"},
	}}
	c := NewLoggerComponent(func(a any) *config.SlogOptions {
		return a.(*testCfg).Log
	})
	k := newMockKernel(cfg)

	if err := c.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	// Mutate the shared config and Reload.
	cfg.Log.Level = "debug"
	if err := c.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	// We can't introspect slog's LevelVar through the interface, but the
	// call must complete without error to confirm SetLevel was dispatched.
}

func TestLoggerComponent_Reload_NilConfigKeepsOldLogger(t *testing.T) {
	cfg := &testCfg{Log: &config.SlogOptions{
		Level: "info", Format: "json", Output: []string{"stdout"},
	}}
	c := NewLoggerComponent(func(a any) *config.SlogOptions {
		return a.(*testCfg).Log
	})
	k := newMockKernel(cfg)
	if err := c.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	// Simulate config removal: resolver now returns nil.
	c.resolve = func(any) *config.SlogOptions { return nil }

	before := c.Logger()
	if err := c.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Logger() != before {
		t.Fatal("logger replaced despite nil reload config; should keep old instance")
	}
}

func TestLoggerComponent_Reload_UnknownLevel_Errors(t *testing.T) {
	cfg := &testCfg{Log: &config.SlogOptions{
		Level: "info", Format: "json", Output: []string{"stdout"},
	}}
	c := NewLoggerComponent(func(a any) *config.SlogOptions {
		return a.(*testCfg).Log
	})
	k := newMockKernel(cfg)
	if err := c.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	cfg.Log.Level = "chatty"
	if err := c.Reload(context.Background()); err == nil {
		t.Fatal("reload with unknown level should surface SetLevel error")
	}
}

func TestLoggerComponent_Health(t *testing.T) {
	c := NewLoggerComponent(func(any) *config.SlogOptions { return nil })
	k := newMockKernel(&testCfg{})
	if err := c.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	s := c.Health(context.Background())
	if s.Status != component.HealthOK {
		t.Fatalf("logger health should always be OK, got %q", s.Status)
	}
}

func TestLoggerComponent_AccessLogger_NoFiles_FallsBackToMain(t *testing.T) {
	cfg := &testCfg{Log: &config.SlogOptions{
		Level: "info", Format: "json", Output: []string{"stdout"},
	}}
	c := NewLoggerComponent(func(a any) *config.SlogOptions {
		return a.(*testCfg).Log
	})
	if err := c.Init(context.Background(), newMockKernel(cfg)); err != nil {
		t.Fatal(err)
	}
	if c.AccessLogger() != c.Logger() {
		t.Fatal("access logger should equal main logger when no access_files set")
	}
}
