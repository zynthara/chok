package log

import (
	"fmt"
	"strings"
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
// Empty path disables the entry; rotation thresholds are size-based
// with optional age/backup caps.
type FileOptions struct {
	Path       string `mapstructure:"path"`
	MaxSizeMB  int    `mapstructure:"max_size_mb"  default:"100"`
	MaxBackups int    `mapstructure:"max_backups"  default:"7"`
	MaxAgeDays int    `mapstructure:"max_age_days" default:"30"`
	Compress   bool   `mapstructure:"compress"     default:"true"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	switch o.Level {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("log: unsupported level %q (use debug/info/warn/error)", o.Level)
	}
	switch o.Format {
	case "json", "text":
	default:
		return fmt.Errorf("log: unsupported format %q (use json/text)", o.Format)
	}
	// Reject blank Output entries up front. Blank strings would silently
	// drop through buildWriter and could confuse later debugging when an
	// operator stares at a config that "should write to a file".
	for i, name := range o.Output {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("log.output[%d]: must not be empty", i)
		}
	}
	// Determine whether at least one *effective* sink is configured.
	// FileOptions documents a blank Path as "disabled", so a slice of
	// all-blank entries equates to no file sink even though len > 0.
	effectiveFiles := 0
	for i := range o.Files {
		if err := o.Files[i].Validate(); err != nil {
			return fmt.Errorf("log.files[%d]: %w", i, err)
		}
		if o.Files[i].Path != "" {
			effectiveFiles++
		}
	}
	for i := range o.AccessFiles {
		if err := o.AccessFiles[i].Validate(); err != nil {
			return fmt.Errorf("log.access_files[%d]: %w", i, err)
		}
	}
	if len(o.Output) == 0 && effectiveFiles == 0 {
		return fmt.Errorf("log: output or files must not both be empty")
	}
	return nil
}

// Validate implements conf.Validatable for one file entry.
func (f *FileOptions) Validate() error {
	if f.Path == "" {
		return nil // empty path disables the entry
	}
	if f.MaxSizeMB < 0 {
		return fmt.Errorf("max_size_mb must not be negative")
	}
	if f.MaxBackups < 0 {
		return fmt.Errorf("max_backups must not be negative")
	}
	if f.MaxAgeDays < 0 {
		return fmt.Errorf("max_age_days must not be negative")
	}
	return nil
}
