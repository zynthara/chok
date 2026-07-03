package redis

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/v2/kernel"
)

// pingTimeout caps the Init reachability check and the Health probe;
// 200ms is enough even for cross-region links and keeps /healthz
// snappy.
const pingTimeout = 200 * time.Millisecond

// Module returns the redis component for chok.Use. Peers consume the
// shared client through the Client() role accessor:
//
//	rc, ok := chok.Get[interface{ Client() *goredis.Client }](k, "redis")
//
// Reconnection / topology updates are the client's responsibility; the
// component does not implement Reloader since connection parameters
// (addr, credentials, TLS) are not safely hot-swappable without
// dropping in-flight commands.
func Module() kernel.Component { return &Component{} }

// Component owns the application-wide *redis.Client.
type Component struct {
	opts   Options
	logger kernel.Logger

	// client is published atomically so Health probes racing with
	// Close observe either a valid handle or nil, never a half-closed
	// one.
	client atomic.Pointer[goredis.Client]
}

// Describe implements kernel.Component.
func (c *Component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "redis",
		ConfigKey: "redis",
		Options:   Options{},
		Needs: []kernel.Dep{
			{Kind: "log", Optional: true},
		},
	}
}

// Init opens the Redis client. go-redis dials lazily on first command;
// without an initial PING a misconfigured addr would only surface at
// the first user request. Init issues a bounded PING and logs a
// warning on failure — but does not fail startup, since transient
// reachability issues at boot are common in orchestrated environments
// (Redis sidecar not yet ready). The Health probe keeps reporting
// reachability thereafter.
func (c *Component) Init(ctx context.Context, k kernel.Kernel) error {
	c.logger = k.Logger()
	if err := k.Config().Section("redis", &c.opts); err != nil {
		return fmt.Errorf("redis: decode section: %w", err)
	}
	client, err := New(c.opts)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	pingErr := client.Ping(pingCtx).Err()
	cancel()
	if pingErr != nil {
		c.logger.Warn("redis: initial ping failed, client will retry on demand",
			"addr", c.opts.Addr,
			"error", pingErr.Error(),
		)
	}
	c.client.Store(client)
	return nil
}

// Close terminates the Redis connection. Idempotent: concurrent or
// repeat calls only close the client once.
func (c *Component) Close(ctx context.Context) error {
	client := c.client.Swap(nil)
	if client == nil {
		return nil
	}
	return client.Close()
}

// Health probes Redis with a bounded PING. Timeout failures surface as
// errors; operators looking for degraded latency should graph metrics
// rather than rely on the binary probe (the v1 latency detail has no
// slot in the error-shaped Healther contract).
func (c *Component) Health(ctx context.Context) error {
	client := c.client.Load()
	if client == nil {
		return fmt.Errorf("redis: client not initialised")
	}
	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := client.Ping(pctx).Err(); err != nil {
		return fmt.Errorf("redis: ping: %w", err)
	}
	return nil
}

// Client returns the shared Redis client. nil before Init and after
// Close.
func (c *Component) Client() *goredis.Client { return c.client.Load() }
