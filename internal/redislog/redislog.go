// Package redislog routes go-redis's own log lines (a dial that failed, a
// connection it dropped) to slog. go-redis writes them through one
// process-wide logger that defaults to the standard library's log on
// stderr, which no chok output captures; every chok package that links
// go-redis imports this one for its side effect, so an application gets
// the bridge whether it uses cache, identity/rbac or both.
package redislog

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"
)

// The logger is set once, when the package initializes and before any
// client exists: go-redis reads it without a lock.
func init() { redis.SetLogger(logger{}) }

// logger passes go-redis's messages to slog.Default at warn; slog.Default
// is read per message, so the logger chok.New installs is picked up.
type logger struct{}

func (logger) Printf(ctx context.Context, format string, v ...any) {
	slog.Default().WarnContext(ctx, fmt.Sprintf(format, v...), "logger", "go-redis")
}
