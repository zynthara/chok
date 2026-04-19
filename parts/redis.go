package parts

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/redis"
)

// RedisResolver extracts the Redis configuration from the app config.
// Returning nil disables the component: Init is a no-op, Client() is
// nil, and Health reports OK (not Down) because an absent Redis is a
// valid operating mode.
type RedisResolver func(appConfig any) *config.RedisOptions

// RedisComponent owns the shared *redis.Client for the application.
// Other Components depending on Redis declare "redis" in Dependencies()
// and retrieve the client via Kernel.Get("redis").(*RedisComponent).Client().
//
// Reconnection / cluster topology updates are the client's responsibility;
// the component does not implement Reloadable since connection parameters
// (Addr, Password, DB) are not safely hot-swappable without dropping
// in-flight commands.
type RedisComponent struct {
	resolve RedisResolver
	client  *goredis.Client

	// pingTimeout caps the Health probe; default 200ms is enough even
	// for cross-region links and keeps /healthz snappy.
	pingTimeout time.Duration
}

// NewRedisComponent builds a RedisComponent. The resolver may return nil
// to signal "no Redis configured", in which case Init skips connection
// setup and Client() returns nil.
func NewRedisComponent(resolve RedisResolver) *RedisComponent {
	return &RedisComponent{
		resolve:     resolve,
		pingTimeout: 200 * time.Millisecond,
	}
}

// Name implements component.Component.
func (r *RedisComponent) Name() string { return "redis" }

// ConfigKey implements component.Component.
func (r *RedisComponent) ConfigKey() string { return "redis" }

// Init opens the Redis connection by delegating to redis.New. A nil
// resolver result is valid and leaves Client() returning nil.
//
// go-redis dials lazily on first command; without an initial PING a
// misconfigured Addr would only surface at the first user request.
// Init issues a bounded PING and logs a warning on failure, so config
// errors are visible in the boot log — but does not fail startup, since
// transient reachability issues at boot are common in orchestrated
// environments (Redis sidecar not yet ready). The Healther probe will
// continue reporting reachability thereafter.
func (r *RedisComponent) Init(ctx context.Context, k component.Kernel) error {
	opts := r.resolve(k.ConfigSnapshot())
	if opts == nil {
		return nil
	}
	client, err := redis.New(opts)
	if err != nil {
		return fmt.Errorf("redis init: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, r.pingTimeout)
	pingErr := client.Ping(pingCtx).Err()
	cancel()
	if pingErr != nil {
		k.Logger().Warn("redis: initial ping failed, client will retry on demand",
			"error", pingErr.Error(),
		)
	}
	r.client = client
	return nil
}

// Close terminates the Redis connection. Safe to call when Init skipped
// the connection (client is nil).
func (r *RedisComponent) Close(ctx context.Context) error {
	if r.client == nil {
		return nil
	}
	return r.client.Close()
}

// Health probes Redis with PING and reports latency in the details map.
// Timeout failures surface as HealthDown; slow-but-successful probes
// still return HealthOK — operators looking for degraded performance
// should graph the latency metric rather than rely on the binary probe.
func (r *RedisComponent) Health(ctx context.Context) component.HealthStatus {
	if r.client == nil {
		// Disabled Redis is an intentional config, not a failure state.
		return component.HealthStatus{Status: component.HealthOK}
	}
	pctx, cancel := context.WithTimeout(ctx, r.pingTimeout)
	defer cancel()

	start := time.Now()
	if err := r.client.Ping(pctx).Err(); err != nil {
		return component.HealthStatus{
			Status: component.HealthDown,
			Error:  err.Error(),
		}
	}
	return component.HealthStatus{
		Status: component.HealthOK,
		Details: map[string]any{
			"latency_ms": time.Since(start).Milliseconds(),
		},
	}
}

// Client returns the underlying Redis client. May be nil if Init was
// called with a nil resolver result (Redis disabled).
func (r *RedisComponent) Client() *goredis.Client { return r.client }

// SetPingTimeout overrides the Health probe timeout. Primarily for tests
// that want to exercise timeout behaviour without waiting 200ms.
func (r *RedisComponent) SetPingTimeout(d time.Duration) {
	r.pingTimeout = d
}
