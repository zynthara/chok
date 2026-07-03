// Package debug is the chok v2 introspection module: /componentz
// renders the assembled component topology straight from Descriptor
// data (single source of truth, SPEC axiom 5) plus a bounded history
// of lifecycle events from the bus.
package debug

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/kernel/event"
)

// Options is the "debug" yaml section. Disabled by default — enable
// in development/staging; the endpoint exposes topology and timing.
type Options struct {
	Enabled bool   `mapstructure:"enabled" default:"false"`
	Path    string `mapstructure:"path"    default:"/componentz" reload:"restart"`
	// EventHistory bounds the retained lifecycle event lines.
	EventHistory int `mapstructure:"event_history" default:"64" reload:"restart"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Path != "" && !strings.HasPrefix(o.Path, "/") {
		return fmt.Errorf("debug: path must start with /, got %q", o.Path)
	}
	if o.EventHistory <= 0 {
		return fmt.Errorf("debug: event_history must be positive, got %d", o.EventHistory)
	}
	return nil
}

// Module returns the debug component for chok.Use.
func Module() kernel.Component { return &component{} }

type component struct {
	k    kernel.Kernel
	opts Options

	mu     sync.Mutex
	events []eventLine
	unsubs []func()
}

type eventLine struct {
	At    time.Time `json:"at"`
	Event string    `json:"event"`
}

func (c *component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "debug",
		ConfigKey: "debug",
		Options:   Options{},
	}
}

func (c *component) Init(ctx context.Context, k kernel.Kernel) error {
	c.k = k
	if err := k.Config().Section("debug", &c.opts); err != nil {
		return fmt.Errorf("debug: decode section: %w", err)
	}

	bus := k.Bus()
	c.unsubs = append(c.unsubs,
		event.Subscribe(bus, func(_ context.Context, e kernel.ComponentInitialized) {
			c.record(fmt.Sprintf("initialized %s in %s", e.Key, e.Duration.Round(time.Microsecond)))
		}),
		event.Subscribe(bus, func(_ context.Context, e kernel.ComponentDegraded) {
			c.record(fmt.Sprintf("degraded %s: %s", e.Key, e.Err))
		}),
		event.Subscribe(bus, func(_ context.Context, e kernel.ComponentClosed) {
			c.record(fmt.Sprintf("closed %s in %s", e.Key, e.Duration.Round(time.Microsecond)))
		}),
		event.Subscribe(bus, func(_ context.Context, e kernel.AppStarted) {
			c.record(fmt.Sprintf("app started in %s", e.Duration.Round(time.Microsecond)))
		}),
		event.Subscribe(bus, func(_ context.Context, e kernel.ReloadApplied) {
			c.record(fmt.Sprintf("reload applied in %s (reloaded: %s)",
				e.Duration.Round(time.Microsecond), strings.Join(e.Reloaded, ",")))
		}),
	)
	return nil
}

func (c *component) record(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, eventLine{At: time.Now(), Event: s})
	if max := c.opts.EventHistory; max > 0 && len(c.events) > max {
		c.events = c.events[len(c.events)-max:]
	}
}

func (c *component) Close(ctx context.Context) error {
	for _, u := range c.unsubs {
		u()
	}
	return nil
}

// Mount implements kernel.Mounter.
func (c *component) Mount(r kernel.Router) error {
	r.Handle(http.MethodGet, c.opts.Path, http.HandlerFunc(c.serveComponentz))
	return nil
}

type componentzResponse struct {
	Components []componentInfo `json:"components"`
	Events     []eventLine     `json:"events"`
}

type componentInfo struct {
	Component  string   `json:"component"`
	State      string   `json:"state"`
	ConfigKey  string   `json:"config_key,omitempty"`
	Needs      []string `json:"needs,omitempty"`
	Optional   bool     `json:"optional,omitempty"`
	MountOrder int      `json:"mount_order,omitempty"`
	Error      string   `json:"error,omitempty"`
}

func (c *component) serveComponentz(w http.ResponseWriter, req *http.Request) {
	statuses := c.k.Components()
	resp := componentzResponse{}
	for _, s := range statuses {
		info := componentInfo{
			Component:  s.Key.String(),
			State:      string(s.State),
			ConfigKey:  s.ConfigKey,
			Optional:   s.Descriptor.Optional,
			MountOrder: s.Descriptor.MountOrder,
			Error:      s.Err,
		}
		for _, d := range s.Descriptor.Needs {
			suffix := ""
			if d.Optional {
				suffix = "?"
			}
			info.Needs = append(info.Needs, kernel.Key{Kind: d.Kind, Instance: d.Instance}.String()+suffix)
		}
		resp.Components = append(resp.Components, info)
	}
	c.mu.Lock()
	resp.Events = append(resp.Events, c.events...)
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
