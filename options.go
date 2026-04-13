package chok

import (
	"context"
	"time"

	"github.com/spf13/pflag"

	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/version"
)

// Option configures an App.
type Option func(*App)

func WithVersion(v version.Info) Option {
	return func(a *App) { a.version = v }
}

// WithConfig registers a typed config pointer and optional explicit path.
// The pointer is populated during Run() after config loading.
func WithConfig(cfg any, path ...string) Option {
	return func(a *App) {
		a.configPtr = cfg
		if len(path) > 0 && path[0] != "" {
			a.configPath = path[0]
			a.configExplicit = true
		}
	}
}

func WithEnvPrefix(prefix string) Option {
	return func(a *App) { a.envPrefix = prefix }
}

// WithLogConfig points to the SlogOptions inside the typed config.
// The pointer is dereferenced after config loading, before Setup.
func WithLogConfig(opts *config.SlogOptions) Option {
	return func(a *App) { a.logOpts = opts }
}

// WithCacheConfig points to cache config options inside the typed config.
// After config loading, the framework auto-builds the cache from enabled layers.
// SetCacher() in Setup overrides the auto-built cache.
// Pointers are dereferenced after config loading (same timing as WithLogConfig).
func WithCacheConfig(memory *config.CacheMemoryOptions, file *config.CacheFileOptions) Option {
	return func(a *App) {
		a.cacheMemOpts = memory
		a.cacheFileOpts = file
	}
}

// WithLogger injects a Logger directly (highest priority).
func WithLogger(l log.Logger) Option {
	return func(a *App) { a.logger = l }
}

func WithSetup(f func(context.Context, *App) error) Option {
	return func(a *App) { a.setupFn = f }
}

// WithCleanup registers a cleanup callback, called LIFO when Run ends.
func WithCleanup(f func(context.Context) error) Option {
	return func(a *App) { a.cleanupFns = append(a.cleanupFns, f) }
}

func WithShutdownTimeout(d time.Duration) Option {
	return func(a *App) { a.shutdownTimeout = d }
}

func WithReloadFunc(f func(context.Context) error) Option {
	return func(a *App) { a.reloadFn = f }
}

func WithReloadTimeout(d time.Duration) Option {
	return func(a *App) { a.reloadTimeout = d }
}

// WithFlags registers a parsed pflag.FlagSet.
// CLI flags take highest priority: flags > env > file > default tag.
// pflag is already an indirect dependency via Viper.
func WithFlags(fs *pflag.FlagSet) Option {
	return func(a *App) { a.flagSet = fs }
}

// RunOption configures a Run() call.
type RunOption func(*runConfig)

type runConfig struct {
	signals bool
}

// WithSignals enables OS signal handling (SIGTERM/SIGINT/SIGHUP/SIGQUIT).
func WithSignals() RunOption {
	return func(rc *runConfig) { rc.signals = true }
}
