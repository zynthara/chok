// Package parts contains concrete Component implementations for chok's
// built-in subsystems (logger, redis, db, cache, scheduler, ...).
//
// Each Component wraps an existing chok subsystem (log, redis, db, ...)
// and plugs it into a component.Registry so its lifecycle, config
// reload, and health are coordinated with the rest of the application.
//
// This package is the bridge between "raw subsystem" (e.g. *redis.Client)
// and "Component" (managed by Registry). User code constructs Components
// via the New* functions in this package, registers them, and accesses
// the underlying resource via the component's accessor:
//
//	reg := component.New(cfg, logger)
//	reg.Register(parts.NewRedisComponent(func(c any) *config.RedisOptions {
//	    return &c.(*MyAppConfig).Redis
//	}))
//	reg.Start(ctx)
//	client := reg.Get("redis").(*parts.RedisComponent).Client()
//
// Design rationale: docs/design.md (§9 parts 包).
package parts
