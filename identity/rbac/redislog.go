package rbac

// The watcher's Redis client reports its own trouble through go-redis's
// process-wide logger; internal/redislog routes it to slog.
import _ "github.com/zynthara/chok/internal/redislog"
