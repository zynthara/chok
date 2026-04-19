package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/internal/ctxval"
	"gopkg.in/natefinch/lumberjack.v2"
)

func errUnknownLevel(s string) error {
	return fmt.Errorf("log: unknown level %q (use debug/info/warn/error)", s)
}

// slogLogger wraps *slog.Logger to implement Logger.
// Holds a reference to the shared slog.LevelVar so SetLevel can mutate it
// and every derived sub-logger (from With) reflects the change immediately.
//
// closers holds the underlying io.Closer writers (lumberjack file handles).
// Sub-loggers from With() share the same slice header — Close is intended
// to be called on the root logger only.
type slogLogger struct {
	sl       *slog.Logger
	levelVar *slog.LevelVar
	closers  []io.Closer
}

// NewSlog creates a Logger from SlogOptions.
func NewSlog(opts *config.SlogOptions) Logger {
	lv := new(slog.LevelVar)
	lv.Set(parseLevel(opts.Level))
	writer, closers := buildWriter(opts.Output, opts.Files)

	var handler slog.Handler
	hopts := &slog.HandlerOptions{Level: lv}
	switch strings.ToLower(opts.Format) {
	case "text":
		handler = slog.NewTextHandler(writer, hopts)
	default:
		handler = slog.NewJSONHandler(writer, hopts)
	}
	return &slogLogger{sl: slog.New(handler), levelVar: lv, closers: closers}
}

// NewDefaultSlog creates a JSON/info Logger writing to stdout.
func NewDefaultSlog() Logger {
	return NewSlog(&config.SlogOptions{
		Level:  "info",
		Format: "json",
		Output: []string{"stdout"},
	})
}

func (l *slogLogger) Debug(msg string, kv ...any) { l.sl.Debug(msg, kv...) }
func (l *slogLogger) Info(msg string, kv ...any)  { l.sl.Info(msg, kv...) }
func (l *slogLogger) Warn(msg string, kv ...any)  { l.sl.Warn(msg, kv...) }
func (l *slogLogger) Error(msg string, kv ...any) { l.sl.Error(msg, kv...) }

func (l *slogLogger) DebugContext(ctx context.Context, msg string, kv ...any) {
	l.sl.DebugContext(ctx, msg, l.appendCtx(ctx, kv)...)
}
func (l *slogLogger) InfoContext(ctx context.Context, msg string, kv ...any) {
	l.sl.InfoContext(ctx, msg, l.appendCtx(ctx, kv)...)
}
func (l *slogLogger) WarnContext(ctx context.Context, msg string, kv ...any) {
	l.sl.WarnContext(ctx, msg, l.appendCtx(ctx, kv)...)
}
func (l *slogLogger) ErrorContext(ctx context.Context, msg string, kv ...any) {
	l.sl.ErrorContext(ctx, msg, l.appendCtx(ctx, kv)...)
}

func (l *slogLogger) With(kv ...any) Logger {
	// 保留对同一 LevelVar 的引用：子 logger 的 level 变化跟随父 logger 的 SetLevel。
	// closers intentionally nil on derived loggers — Close is a root-only operation.
	return &slogLogger{sl: l.sl.With(kv...), levelVar: l.levelVar}
}

// Close releases any file-backed writers held by this logger. Safe to
// call multiple times; returns the first error encountered. Stdout/stderr
// writers are not closed. Derived loggers (from With) are no-ops.
func (l *slogLogger) Close() error {
	var firstErr error
	for _, c := range l.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	l.closers = nil
	return firstErr
}

// SetLevel 切换当前 logger（及其所有 With 派生子 logger）的最小输出级别。
// 底层靠 slog.LevelVar 实现，零分配且线程安全。
func (l *slogLogger) SetLevel(level string) error {
	switch strings.ToLower(level) {
	case "debug":
		l.levelVar.Set(slog.LevelDebug)
	case "info":
		l.levelVar.Set(slog.LevelInfo)
	case "warn", "warning":
		l.levelVar.Set(slog.LevelWarn)
	case "error":
		l.levelVar.Set(slog.LevelError)
	default:
		return errUnknownLevel(level)
	}
	return nil
}

func (l *slogLogger) appendCtx(ctx context.Context, kv []any) []any {
	if rid := ctxval.RequestIDFrom(ctx); rid != "" {
		// Copy to avoid mutating the caller's backing array when it has
		// spare capacity — a classic variadic slice aliasing data race.
		out := make([]any, len(kv), len(kv)+2)
		copy(out, kv)
		return append(out, "request_id", rid)
	}
	return kv
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// buildWriter returns the aggregated writer plus a list of closers for
// lumberjack-backed file writers so the owning Logger can release handles
// on Close. Stdout/stderr are never added to the closer list.
//
// Empty entries (blank string in `outputs`, blank Path in `files`) are
// silently skipped: LogFileOptions documents a blank path as "disabled"
// and SlogOptions.Validate now rejects blank Output strings up front,
// so a blank entry reaching this function would be a defence-in-depth
// case (e.g. an unvalidated programmatic build). Skipping prevents
// lumberjack from defaulting an empty Filename to a temp-dir log file
// the operator never asked for.
func buildWriter(outputs []string, files []config.LogFileOptions) (io.Writer, []io.Closer) {
	writers := make([]io.Writer, 0, len(outputs)+len(files))
	var closers []io.Closer
	for _, o := range outputs {
		if strings.TrimSpace(o) == "" {
			continue
		}
		w, c := singleWriter(o)
		writers = append(writers, w)
		if c != nil {
			closers = append(closers, c)
		}
	}
	for i := range files {
		if files[i].Path == "" {
			continue
		}
		lj := fileWriter(&files[i])
		writers = append(writers, lj)
		closers = append(closers, lj)
	}
	switch len(writers) {
	case 0:
		return os.Stdout, nil
	case 1:
		return writers[0], closers
	default:
		return io.MultiWriter(writers...), closers
	}
}

// singleWriter returns the writer for a bare output name and, when the
// writer owns a file handle, the closer to release it. Stdout/stderr
// return a nil closer.
func singleWriter(name string) (io.Writer, io.Closer) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "stdout":
		return os.Stdout, nil
	case "stderr":
		return os.Stderr, nil
	default:
		// Bare-path output: wrap in lumberjack for lifecycle management
		// (flush/close on process exit) and sensible default rotation.
		lj := &lumberjack.Logger{
			Filename:   name,
			MaxSize:    100, // MB
			MaxBackups: 3,
			MaxAge:     28, // days
			Compress:   false,
			LocalTime:  true,
		}
		return lj, lj
	}
}

func fileWriter(f *config.LogFileOptions) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   f.Path,
		MaxSize:    f.MaxSizeMB,
		MaxBackups: f.MaxBackups,
		MaxAge:     f.MaxAgeDays,
		Compress:   f.Compress,
		LocalTime:  true,
	}
}
