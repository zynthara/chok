package log

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/internal/ctxval"
)

// slogLogger wraps *slog.Logger to implement Logger.
type slogLogger struct {
	sl *slog.Logger
}

// NewSlog creates a Logger from SlogOptions.
func NewSlog(opts *config.SlogOptions) Logger {
	level := parseLevel(opts.Level)
	writer := buildWriter(opts.Output)

	var handler slog.Handler
	hopts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(opts.Format) {
	case "text":
		handler = slog.NewTextHandler(writer, hopts)
	default:
		handler = slog.NewJSONHandler(writer, hopts)
	}
	return &slogLogger{sl: slog.New(handler)}
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
	return &slogLogger{sl: l.sl.With(kv...)}
}

func (l *slogLogger) appendCtx(ctx context.Context, kv []any) []any {
	if rid := ctxval.RequestIDFrom(ctx); rid != "" {
		kv = append(kv, "request_id", rid)
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

func buildWriter(outputs []string) io.Writer {
	if len(outputs) == 0 {
		return os.Stdout
	}
	if len(outputs) == 1 {
		return singleWriter(outputs[0])
	}
	writers := make([]io.Writer, 0, len(outputs))
	for _, o := range outputs {
		writers = append(writers, singleWriter(o))
	}
	return io.MultiWriter(writers...)
}

func singleWriter(name string) io.Writer {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "stdout":
		return os.Stdout
	case "stderr":
		return os.Stderr
	default:
		f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			// Fallback to stderr if file can't be opened.
			return os.Stderr
		}
		return f
	}
}
