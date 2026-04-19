package log

import "context"

// Logger is the framework's logging interface.
type Logger interface {
	Debug(msg string, keysAndValues ...any)
	Info(msg string, keysAndValues ...any)
	Warn(msg string, keysAndValues ...any)
	Error(msg string, keysAndValues ...any)
	DebugContext(ctx context.Context, msg string, keysAndValues ...any)
	InfoContext(ctx context.Context, msg string, keysAndValues ...any)
	WarnContext(ctx context.Context, msg string, keysAndValues ...any)
	ErrorContext(ctx context.Context, msg string, keysAndValues ...any)
	With(keysAndValues ...any) Logger

	// SetLevel dynamically changes the minimum log level. Accepts
	// "debug" / "info" / "warn" / "warning" / "error". Returns an error
	// for unsupported values. Loggers that don't support dynamic levels
	// (e.g. Empty) may treat this as a no-op.
	SetLevel(level string) error
}

// --- Context-scoped Logger ---------------------------------------------------

type loggerCtxKey struct{}

// WithContext stores a Logger in ctx. Typically called by HTTP middleware
// to inject a per-request logger enriched with request_id and other
// request-scoped attributes.
func WithContext(ctx context.Context, l Logger) context.Context {
	return context.WithValue(ctx, loggerCtxKey{}, l)
}

// FromContext retrieves the Logger stored in ctx by WithContext. Returns
// nil if no logger is present — callers should fall back to a default
// logger when nil is returned.
func FromContext(ctx context.Context) Logger {
	if ctx == nil {
		return nil
	}
	if l, ok := ctx.Value(loggerCtxKey{}).(Logger); ok {
		return l
	}
	return nil
}
