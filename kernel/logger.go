package kernel

import "context"

// Logger is the kernel's consumer-side logging contract. The rich
// chok log.Logger satisfies it structurally; keeping the definition
// here (instead of importing the log package) leaves log free to
// import kernel for log.Module — the dependency arrow points one way:
// log → kernel → conf.
type Logger interface {
	Debug(msg string, keysAndValues ...any)
	Info(msg string, keysAndValues ...any)
	Warn(msg string, keysAndValues ...any)
	Error(msg string, keysAndValues ...any)
	DebugContext(ctx context.Context, msg string, keysAndValues ...any)
	InfoContext(ctx context.Context, msg string, keysAndValues ...any)
	WarnContext(ctx context.Context, msg string, keysAndValues ...any)
	ErrorContext(ctx context.Context, msg string, keysAndValues ...any)
}

// nopLogger is the default when no logger is configured.
type nopLogger struct{}

func (nopLogger) Debug(string, ...any)                         {}
func (nopLogger) Info(string, ...any)                          {}
func (nopLogger) Warn(string, ...any)                          {}
func (nopLogger) Error(string, ...any)                         {}
func (nopLogger) DebugContext(context.Context, string, ...any) {}
func (nopLogger) InfoContext(context.Context, string, ...any)  {}
func (nopLogger) WarnContext(context.Context, string, ...any)  {}
func (nopLogger) ErrorContext(context.Context, string, ...any) {}
