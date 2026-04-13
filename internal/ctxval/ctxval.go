package ctxval

import "context"

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyLogger
)

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

func RequestIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(keyRequestID).(string); ok {
		return v
	}
	return ""
}

// Logger is stored as an interface{} to avoid import cycles.
// The actual type is log.Logger, but ctxval must not import log.

func WithLogger(ctx context.Context, logger any) context.Context {
	return context.WithValue(ctx, keyLogger, logger)
}

func LoggerFrom(ctx context.Context) any {
	if ctx == nil {
		return nil
	}
	return ctx.Value(keyLogger)
}
