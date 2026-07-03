package log

import (
	"context"
	"fmt"

	"github.com/zynthara/chok/v2/kernel"
)

// Module returns the log component for chok.Use. The App owns the
// root logger's construction and file-handle lifecycle (SPEC §3.5);
// this module carries only the hot-reload semantics: when the "log"
// section's hot fields change (level), Reload applies them to the
// live logger. Restart-only fields (format / outputs / files) are
// warn-reported by the framework diff.
func Module() kernel.Component { return &component{} }

type component struct {
	k kernel.Kernel
}

// levelSetter is the slice of the logger contract this module needs;
// the chok Logger implements it.
type levelSetter interface {
	SetLevel(level string) error
}

func (c *component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "log",
		ConfigKey: "log",
		Options:   Options{},
	}
}

func (c *component) Init(ctx context.Context, k kernel.Kernel) error {
	c.k = k
	return nil
}

// Close is a no-op: the App owns the root logger's file handles and
// flushes them after the control plane stops.
func (c *component) Close(ctx context.Context) error { return nil }

// Reload applies hot log-section fields to the live root logger.
func (c *component) Reload(ctx context.Context) error {
	var o Options
	if err := c.k.Config().Section("log", &o); err != nil {
		return fmt.Errorf("log: decode section: %w", err)
	}
	ls, ok := c.k.Logger().(levelSetter)
	if !ok {
		c.k.Logger().Warn("log: root logger does not support dynamic levels; restart to apply", "level", o.Level)
		return nil
	}
	if err := ls.SetLevel(o.Level); err != nil {
		return fmt.Errorf("log: set level: %w", err)
	}
	c.k.Logger().Info("log: level applied", "level", o.Level)
	return nil
}
