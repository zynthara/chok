package ctxval

import "context"

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyLogger
	keyClientIP
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

// WithClientIP stores the resolved client IP in ctx. Called by HTTP
// middleware so handlers can key rate limiters on source address in
// addition to user-supplied identifiers.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, keyClientIP, ip)
}

// ClientIPFrom retrieves the client IP stored by WithClientIP. Returns
// "" when absent; callers should skip IP-based rate limiting in that case
// rather than keying on the empty string (which would be a global bucket).
func ClientIPFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(keyClientIP).(string); ok {
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
