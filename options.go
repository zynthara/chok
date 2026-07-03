package chok

import (
	"context"
	"time"

	"github.com/spf13/pflag"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/version"
)

// Option configures an App at construction time.
type Option func(*App)

// Use assembles modules. The one registration surface: duplicate
// (kind, instance) keys fail startup — intentional replacement is
// spelled chok.Override (SPEC §3.1).
func Use(modules ...kernel.Component) Option {
	return func(a *App) { a.modules = append(a.modules, modules...) }
}

// Override replaces a previously Use'd module with the same
// (kind, instance) key. A key that matches nothing fails startup —
// silent-miss typos are the failure mode this guards.
func Override(c kernel.Component) Option {
	return func(a *App) { a.overrides = append(a.overrides, c) }
}

// Routes registers the business route callback, invoked during the
// mount phase between MountOrder ≤ 0 and > 0 mounters.
func Routes(f func(r kernel.Router, k kernel.Kernel) error) Option {
	return func(a *App) { a.routes = f }
}

// WithConfigFile pins an explicit config path (a missing file is then
// an error). Without it: {PREFIX}_CONFIG → ./{name}.yaml →
// ./configs/{name}.yaml, all optional.
func WithConfigFile(path string) Option {
	return func(a *App) { a.configFile = path }
}

// WithEnvPrefix overrides the env prefix (default: upper-cased app
// name with -/. mapped to _).
func WithEnvPrefix(prefix string) Option {
	return func(a *App) { a.envPrefix = prefix }
}

// WithFlags binds a parsed pflag set as the highest-priority config
// source (flags > env > file > defaults).
func WithFlags(fs *pflag.FlagSet) Option {
	return func(a *App) { a.flags = fs }
}

func (a *App) pflags() *pflag.FlagSet {
	fs, _ := a.flags.(*pflag.FlagSet)
	return fs
}

// WithVersion attaches build/version metadata.
func WithVersion(v version.Info) Option {
	return func(a *App) { a.version = v }
}

// WithLogger injects a prebuilt root logger. Its lifecycle belongs to
// the caller — the App only closes loggers it constructed itself.
func WithLogger(l log.Logger) Option {
	return func(a *App) { a.logger = l }
}

// WithReloadFunc registers the synchronous post-reload callback: the
// last stage of the reload pipeline. It runs only when config swap and
// component dispatch both succeeded, and its error fails the whole
// reload (v1 gating contract, SPEC §9).
func WithReloadFunc(f func(context.Context) error) Option {
	return func(a *App) { a.reloadFn = f }
}

// WithErrorMapper registers a per-App error mapper (isolated from
// other App instances). The web middleware stack consumes the
// registry from M2 on.
func WithErrorMapper(m apierr.ErrorMapper) Option {
	return func(a *App) {
		if a.errorMappers == nil {
			a.errorMappers = apierr.NewMapperRegistry()
		}
		a.errorMappers.Register(m)
	}
}

// WithDrainDelay pauses between readiness flipping to 503 (draining
// phase begins) and Serve contexts being cancelled — load balancers
// deregister the pod before in-flight work is cut.
func WithDrainDelay(d time.Duration) Option {
	return func(a *App) { a.drainDelay = d }
}

// WithInitTimeout sets the default per-component Init/Migrate budget
// (Descriptor.Timeouts overrides per component; default 30s).
func WithInitTimeout(d time.Duration) Option {
	return func(a *App) { a.timeouts.Init = d }
}

// WithCloseTimeout sets the default per-component Close budget
// (default 15s).
func WithCloseTimeout(d time.Duration) Option {
	return func(a *App) { a.timeouts.Close = d }
}

// WithComponentReloadTimeout sets the default per-component Reload
// budget (default 10s).
func WithComponentReloadTimeout(d time.Duration) Option {
	return func(a *App) { a.timeouts.Reload = d }
}

// WithShutdownTimeout bounds the whole stop sequence (drain + close;
// default 30s). SIGQUIT further caps it at 5s for fast shutdown.
func WithShutdownTimeout(d time.Duration) Option {
	return func(a *App) { a.shutdownTimeout = d }
}

// WithReloadTimeout bounds one whole reload pipeline (default 30s).
func WithReloadTimeout(d time.Duration) Option {
	return func(a *App) { a.reloadTimeout = d }
}

// RunOption configures a single Run call.
type RunOption func(*runConfig)

type runConfig struct {
	signals     bool
	watchConfig bool
}

// WithSignals enables OS signal handling: SIGTERM/SIGINT graceful
// stop, SIGQUIT fast stop, SIGHUP reload.
func WithSignals() RunOption {
	return func(rc *runConfig) { rc.signals = true }
}

// WithConfigWatch reloads on config file changes (fsnotify, 100ms
// debounce, content-hash suppression for k8s ConfigMap rotations).
// No-op with a warning when no config file was loaded.
func WithConfigWatch() RunOption {
	return func(rc *runConfig) { rc.watchConfig = true }
}
