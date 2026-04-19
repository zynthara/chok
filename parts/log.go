package parts

import (
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/log"
)

// LoggerResolver extracts the logger's SlogOptions from the app config.
// Returning nil is valid and yields an Empty logger, matching chok's
// existing "no config → no-op logger" behaviour.
type LoggerResolver func(appConfig any) *config.SlogOptions

// LoggerComponent is the Component wrapper around chok/log. It owns the
// framework logger and (when configured) a separate access logger. The
// level can be hot-swapped via Reload — file rotation / output routing
// are NOT reloadable and are locked in at Init.
//
// Two modes of operation:
//
//   - Standard: Init uses the resolver to get SlogOptions and calls
//     log.NewSlog to build the logger. Used when parts wires up the
//     logger itself (e.g. in a clean "all Components" app).
//   - Pre-built (WithPreBuilt): Init skips construction and uses the
//     supplied log.Logger directly. Used by chok.App which still
//     builds the logger through its legacy initLogger path but wants
//     the registry to own Reload/Close dispatch.
type LoggerComponent struct {
	resolve LoggerResolver

	kernel component.Kernel
	opts   atomic.Pointer[config.SlogOptions]

	// preBuiltLogger / preBuiltAccess are set by WithPreBuilt. When
	// preBuiltLogger is non-nil, Init adopts it instead of calling
	// log.NewSlog. Reload still dispatches SetLevel to the live logger.
	preBuiltLogger log.Logger
	preBuiltAccess log.Logger

	logger       log.Logger
	accessLogger log.Logger

	// closeOnce guards Close so repeat calls (e.g. idempotent shutdown
	// paths) don't attempt to close already-released lumberjack handles.
	closeOnce sync.Once
}

// NewLoggerComponent returns a LoggerComponent that reads its configuration
// via resolve on every Init/Reload. resolve is responsible for cast and
// nil-safety.
func NewLoggerComponent(resolve LoggerResolver) *LoggerComponent {
	return &LoggerComponent{resolve: resolve}
}

// WithPreBuilt binds a logger (and optional access logger) that was
// constructed externally. Init will adopt the given instances instead
// of building its own.
//
// Typical caller: chok.App, which retains its legacy initLogger code
// path so setupFn can use app.Logger() before the registry starts.
// Passing access == nil is fine — AccessLogger() falls back to logger.
// The resolver is still consulted on Reload to apply level changes
// against the pre-built logger, so log.level hot-swap keeps working.
func (l *LoggerComponent) WithPreBuilt(logger, access log.Logger) *LoggerComponent {
	l.preBuiltLogger = logger
	l.preBuiltAccess = access
	return l
}

// Name implements component.Component.
func (l *LoggerComponent) Name() string { return "log" }

// ConfigKey implements component.Component.
func (l *LoggerComponent) ConfigKey() string { return "log" }

// Init constructs the logger(s) from the resolved options. When resolve
// returns nil (no log configuration), Init installs an Empty logger so
// downstream code can call logger methods unconditionally.
//
// When WithPreBuilt was set, Init adopts the supplied logger instances
// and only caches the options pointer for Reload to later use.
func (l *LoggerComponent) Init(ctx context.Context, k component.Kernel) error {
	l.kernel = k

	if l.preBuiltLogger != nil {
		l.logger = l.preBuiltLogger
		if l.preBuiltAccess != nil {
			l.accessLogger = l.preBuiltAccess
		} else {
			l.accessLogger = l.preBuiltLogger
		}
		if opts := l.resolve(k.ConfigSnapshot()); opts != nil {
			l.opts.Store(opts)
		}
		return nil
	}

	opts := l.resolve(k.ConfigSnapshot())
	if opts == nil {
		l.logger = log.Empty()
		l.accessLogger = log.Empty()
		return nil
	}
	l.opts.Store(opts)

	l.logger = log.NewSlog(opts)
	l.accessLogger = buildAccessLogger(opts, l.logger)
	return nil
}

// Close releases any file handles held by the constructed loggers. It is
// a no-op when WithPreBuilt supplied the loggers (the caller owns their
// lifecycle) and idempotent via closeOnce.
func (l *LoggerComponent) Close(ctx context.Context) error {
	if l.preBuiltLogger != nil {
		return nil
	}
	var firstErr error
	l.closeOnce.Do(func() {
		if c, ok := l.logger.(io.Closer); ok {
			if err := c.Close(); err != nil {
				firstErr = err
			}
		}
		// Access logger is only separately constructed when access_files
		// differs from main files; otherwise it aliases l.logger and
		// Closing it again is unsafe. Check identity via underlying
		// slogLogger pointer equality.
		if l.accessLogger != nil && l.accessLogger != l.logger {
			if c, ok := l.accessLogger.(io.Closer); ok {
				if err := c.Close(); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
	})
	return firstErr
}

// Reload re-reads the logger options via the resolver and applies the
// level change unconditionally (SetLevel is cheap and idempotent).
//
// Output routing, file rotation and format are NOT reloadable. When
// those fields change, a warning is emitted so operators know a restart
// is required. This simplification exists because replacing log writers
// at runtime risks losing in-flight entries and complicates lumberjack
// file-handle management. Documented in docs/design.md (§6.3).
func (l *LoggerComponent) Reload(ctx context.Context) error {
	next := l.resolve(l.kernel.ConfigSnapshot())
	if next == nil {
		// Config was removed entirely — keep current logger running so
		// callers don't suddenly lose output.
		return nil
	}

	prev := l.opts.Load()
	l.opts.Store(next)

	// Warn about non-reloadable field changes so operators know a restart
	// is required. Level changes are applied immediately below.
	if prev != nil {
		if prev.Format != next.Format {
			l.kernel.Logger().Warn("log config changed but requires restart: format",
				"old", prev.Format, "new", next.Format)
		}
		if !strSliceEqual(prev.Output, next.Output) {
			l.kernel.Logger().Warn("log config changed but requires restart: output",
				"old", prev.Output, "new", next.Output)
		}
		if !logFileSliceEqual(prev.Files, next.Files) {
			l.kernel.Logger().Warn("log config changed but requires restart: files")
		}
		if !logFileSliceEqual(prev.AccessFiles, next.AccessFiles) {
			l.kernel.Logger().Warn("log config changed but requires restart: access_files")
		}
	}

	return l.logger.SetLevel(next.Level)
}

func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func logFileSliceEqual(a, b []config.LogFileOptions) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Health always returns OK. A degraded logger would mean the process
// has far bigger problems than /healthz can report, and any probe
// failure here would create a circular dependency (logging the
// probe's own failure).
func (l *LoggerComponent) Health(ctx context.Context) component.HealthStatus {
	return component.HealthStatus{Status: component.HealthOK}
}

// Logger returns the main application logger. Safe to call after Init.
func (l *LoggerComponent) Logger() log.Logger { return l.logger }

// AccessLogger returns the logger dedicated to HTTP access entries.
// Falls back to the main logger when access_files is empty.
func (l *LoggerComponent) AccessLogger() log.Logger { return l.accessLogger }

// buildAccessLogger mirrors the semantics of chok.App.AccessLogger():
// if access_files is configured, route access entries to a dedicated
// file set; otherwise reuse the main logger.
func buildAccessLogger(opts *config.SlogOptions, main log.Logger) log.Logger {
	if len(opts.AccessFiles) == 0 {
		return main
	}
	// Construct a standalone logger for access output. Format/level match
	// the main logger so operators can grep across both streams uniformly.
	accessOpts := &config.SlogOptions{
		Level:  opts.Level,
		Format: opts.Format,
		Output: nil,
		Files:  opts.AccessFiles,
	}
	return log.NewSlog(accessOpts)
}
