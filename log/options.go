package log

import (
	"github.com/zynthara/chok/v2/config"
)

// Options is the v2 "log" section. The App owns the root logger built
// from it (constructed before the control plane, flushed/closed after
// it — SPEC §3.5); log.Module only carries the hot-reload semantics.
//
// reload tags: level is hot (SetLevel on the live logger); everything
// that changes writers or formats requires a restart.
type Options struct {
	Level  string        `mapstructure:"level"  default:"info"   reload:"hot"`
	Format string        `mapstructure:"format" default:"json"   reload:"restart"`
	Output []string      `mapstructure:"output" default:"stdout" reload:"restart"`
	Files  []FileOptions `mapstructure:"files"                   reload:"restart"`

	// AccessFiles / AccessEnabled keep the v1 yaml shape; web.Module
	// consumes them when building the access-log middleware (dedicated
	// rotating files when set, the root logger otherwise).
	AccessFiles   []FileOptions `mapstructure:"access_files"   reload:"restart"`
	AccessEnabled bool          `mapstructure:"access_enabled" default:"true" reload:"restart"`
}

// FileOptions configures one rotating file output (lumberjack-backed).
type FileOptions struct {
	Path       string `mapstructure:"path"`
	MaxSizeMB  int    `mapstructure:"max_size_mb"  default:"100"`
	MaxBackups int    `mapstructure:"max_backups"  default:"7"`
	MaxAgeDays int    `mapstructure:"max_age_days" default:"30"`
	Compress   bool   `mapstructure:"compress"     default:"true"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	return o.slog().Validate()
}

// slog converts to the v1 construction options — the slog/lumberjack
// build path is shared during the transition (config package retires
// with the last v1 battery, M5).
func (o *Options) slog() *config.SlogOptions {
	files := make([]config.LogFileOptions, 0, len(o.Files))
	for _, f := range o.Files {
		files = append(files, config.LogFileOptions(f))
	}
	access := make([]config.LogFileOptions, 0, len(o.AccessFiles))
	for _, f := range o.AccessFiles {
		access = append(access, config.LogFileOptions(f))
	}
	return &config.SlogOptions{
		Level:         o.Level,
		Format:        o.Format,
		Output:        o.Output,
		Files:         files,
		AccessFiles:   access,
		AccessEnabled: o.AccessEnabled,
	}
}

// New builds a Logger from Options. The returned logger implements
// io.Closer when file outputs are configured; the owner (the App for
// the root logger) must Close it after the control plane stops.
func New(o Options) Logger {
	return NewSlog(o.slog())
}
