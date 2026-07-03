package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/kernel/event"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/metrics"
)

// testKernel is a minimal kernel.Kernel double for exercising
// Component.Init without a full registry: config from a real conf
// store, role lookups from a map. (choktest.NewTestKernel always
// injects its own RouterProvider, which would collide with the web
// module's — hence the local stub.)
type testKernel struct {
	store *conf.Store
	bus   *event.Bus
	comps map[kernel.Key]kernel.Component
}

func (k *testKernel) Config() *conf.Snapshot { return k.store.Snapshot() }
func (k *testKernel) Logger() kernel.Logger  { return log.Empty() }
func (k *testKernel) Bus() *event.Bus        { return k.bus }

func (k *testKernel) Lookup(kind string, instance ...string) (kernel.Component, bool) {
	inst := kernel.DefaultInstance
	if len(instance) > 0 && instance[0] != "" {
		inst = instance[0]
	}
	c, ok := k.comps[kernel.Key{Kind: kind, Instance: inst}]
	return c, ok
}

func (k *testKernel) Health(context.Context) kernel.HealthReport {
	return kernel.HealthReport{Status: kernel.HealthUp}
}
func (k *testKernel) Ready(context.Context) error            { return nil }
func (k *testKernel) Components() []kernel.ComponentStatus   { return nil }

// newTestKernel builds the stub from a yaml literal, registering the
// web Options plus every peer component's section, then running the
// peers' Init so their capabilities (metrics registry, ...) are live.
func newTestKernel(t *testing.T, yaml string, peers ...kernel.Component) *testKernel {
	t.Helper()
	loader := conf.NewLoader("webtest", "WEBTEST")
	if yaml != "" {
		dir := t.TempDir()
		path := filepath.Join(dir, "webtest.yaml")
		if err := os.WriteFile(path, []byte(strings.TrimSpace(yaml)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		loader.SetPath(path)
	}
	if err := loader.Register("http", Options{}); err != nil {
		t.Fatal(err)
	}
	if err := loader.Register("log", log.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range peers {
		d := p.Describe()
		if key := kernel.SectionKeyOf(d); key != "" && d.Options != nil {
			if err := loader.Register(key, d.Options); err != nil {
				t.Fatal(err)
			}
		}
	}
	store, err := conf.NewStore(loader)
	if err != nil {
		t.Fatal(err)
	}
	tk := &testKernel{
		store: store,
		bus:   event.NewBus(),
		comps: make(map[kernel.Key]kernel.Component),
	}
	for _, p := range peers {
		if err := p.Init(context.Background(), tk); err != nil {
			t.Fatalf("peer %s init: %v", kernel.KeyOf(p.Describe()), err)
		}
		tk.comps[kernel.KeyOf(p.Describe())] = p
		t.Cleanup(func() { _ = p.Close(context.Background()) })
	}
	return tk
}

// newWebComponent assembles and Inits a web Component against the stub.
func newWebComponent(t *testing.T, yaml string, peers []kernel.Component, opts ...ModOpt) *Component {
	t.Helper()
	tk := newTestKernel(t, yaml, peers...)
	c := Module(opts...).(*Component)
	if err := c.Init(context.Background(), tk); err != nil {
		t.Fatalf("web init: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// metricsPeer builds an initialized metrics module for RED assertions.
func metricsPeer() kernel.Component { return metrics.Module() }
