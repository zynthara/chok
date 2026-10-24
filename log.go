package chok

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"
)

// loggers is what New builds from LogConfig.
type loggers struct {
	root    *slog.Logger
	level   *slog.LevelVar
	access  *slog.Logger // nil when log.access is false
	closers []io.Closer
}

func buildLoggers(c LogConfig) loggers {
	var l loggers
	l.level = new(slog.LevelVar)
	if lv, ok := parseLevel(c.Level); ok {
		l.level.Set(lv)
	}
	w, closers := logWriter(c.Output, c.Files)
	l.closers = closers
	l.root = slog.New(logHandler(c.Format, w, l.level))
	if c.Access {
		l.access = l.root
		if hasPath(c.AccessFiles) {
			aw, ac := logWriter(nil, c.AccessFiles)
			l.closers = append(l.closers, ac...)
			l.access = slog.New(logHandler(c.Format, aw, slog.LevelInfo))
		}
	}
	return l
}

func logHandler(format string, w io.Writer, level slog.Leveler) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(format, "text") {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

func parseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, true
	case "info", "":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

func hasPath(files []FileConfig) bool {
	for _, f := range files {
		if f.Path != "" {
			return true
		}
	}
	return false
}

// logWriter fans out to every output (stdout, stderr or a path) and every
// rotating file; the closers release the files.
func logWriter(outputs []string, files []FileConfig) (io.Writer, []io.Closer) {
	var ws []io.Writer
	var closers []io.Closer
	for _, o := range outputs {
		switch name := strings.TrimSpace(o); strings.ToLower(name) {
		case "":
		case "stdout":
			ws = append(ws, os.Stdout)
		case "stderr":
			ws = append(ws, os.Stderr)
		default:
			lj := &lumberjack.Logger{Filename: name, MaxSize: 100, MaxBackups: 3, MaxAge: 28, LocalTime: true}
			ws = append(ws, lj)
			closers = append(closers, lj)
		}
	}
	for _, f := range files {
		if f.Path == "" {
			continue
		}
		lj := &lumberjack.Logger{
			Filename:   f.Path,
			MaxSize:    f.MaxSizeMB,
			MaxBackups: f.MaxBackups,
			MaxAge:     f.MaxAgeDays,
			Compress:   f.Compress,
			LocalTime:  true,
		}
		ws = append(ws, lj)
		closers = append(closers, lj)
	}
	switch len(ws) {
	case 0:
		return os.Stdout, closers
	case 1:
		return ws[0], closers
	}
	return io.MultiWriter(ws...), closers
}

type loggerKey struct{}

// LoggerFrom returns the request's logger, which carries its request_id;
// outside a request it returns slog.Default().
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

func withLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}
