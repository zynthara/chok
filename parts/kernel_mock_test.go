package parts

import (
	"context"

	"github.com/zynthara/chok/v2/component"
	"github.com/zynthara/chok/v2/log"
)

// mockKernel is a minimal v1 Kernel for testing transition-period
// Components in isolation. It used to live in log_test.go and moved
// here when the log/health/metrics/debug parts migrated to their v2
// modules (M1) — the remaining v1 components keep using it until
// their own M2-M4 migrations.
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

// stubLogComponent satisfies the "log" dependency that transition
// components (account, ...) declare by name, now that LoggerComponent
// migrated to the v2 log module.
type stubLogComponent struct{}

func (stubLogComponent) Name() string                                 { return "log" }
func (stubLogComponent) Init(context.Context, component.Kernel) error { return nil }
func (stubLogComponent) Close(context.Context) error                  { return nil }
