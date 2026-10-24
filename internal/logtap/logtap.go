// Package logtap copies the records an App's loggers write to one more
// slog.Handler, the OpenTelemetry Logs bridge of telemetry, without a
// public API: the root package wraps the handlers it builds with Wrap,
// and telemetry installs the bridge with Install when logs.otel is on.
// Until then a wrapped handler costs an atomic load per record.
package logtap

import (
	"context"
	"log/slog"
	"slices"
	"sync/atomic"
)

// tap is the installed handler; a fresh *tap per Install tells the
// handlers derived for an earlier one to derive again.
type tap struct{ h slog.Handler }

var current atomic.Pointer[tap]

// Install makes every wrapped handler pass its records to h as well, in
// place of the handler installed before. The returned function removes h
// unless another Install has replaced it since.
func Install(h slog.Handler) (uninstall func()) {
	t := &tap{h: h}
	current.Store(t)
	return func() { current.CompareAndSwap(t, nil) }
}

// Wrap returns a handler that writes through h and, while a handler is
// installed, passes the records h is enabled for to that one too, with
// the attributes and groups given to WithAttrs and WithGroup on the way.
func Wrap(h slog.Handler) slog.Handler { return &handler{h: h} }

type handler struct {
	h   slog.Handler
	ops []op // WithAttrs and WithGroup calls, replayed on the tap
	// derived caches the tap with ops applied, for the tap it was built on.
	derived atomic.Pointer[derived]
}

type op struct {
	group string // a WithGroup when attrs is nil
	attrs []slog.Attr
}

type derived struct {
	from *tap
	h    slog.Handler
}

func (w *handler) Enabled(ctx context.Context, l slog.Level) bool { return w.h.Enabled(ctx, l) }

func (w *handler) Handle(ctx context.Context, r slog.Record) error {
	err := w.h.Handle(ctx, r)
	if t := current.Load(); t != nil {
		if h := w.tapped(t); h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone()) // the tap's trouble is not the caller's
		}
	}
	return err
}

func (w *handler) tapped(t *tap) slog.Handler {
	if d := w.derived.Load(); d != nil && d.from == t {
		return d.h
	}
	h := t.h
	for _, o := range w.ops {
		if o.attrs == nil {
			h = h.WithGroup(o.group)
		} else {
			h = h.WithAttrs(o.attrs)
		}
	}
	w.derived.Store(&derived{from: t, h: h})
	return h
}

func (w *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return w
	}
	return &handler{h: w.h.WithAttrs(attrs), ops: append(slices.Clip(w.ops), op{attrs: slices.Clone(attrs)})}
}

func (w *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return w
	}
	return &handler{h: w.h.WithGroup(name), ops: append(slices.Clip(w.ops), op{group: name})}
}
