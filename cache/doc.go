// Package cache is chok's cache battery: byte values under string keys,
// in a memory layer in the process, a Redis layer shared by the replicas,
// or both, with a circuit breaker in front of Redis, under one Config.
//
//	c := cache.MustNew(cfg.Cache)
//	app.Use(c)
//
//	data, err := c.GetOrLoad(ctx, "post:"+id, 10*time.Minute, func(ctx context.Context) ([]byte, error) {
//		p, err := posts.Get(ctx, id)
//		if err != nil {
//			return nil, err
//		}
//		return json.Marshal(p)
//	})
//
// Reads go to the memory layer first and to Redis on a miss, a Redis hit
// filling the memory layer; writes and deletes go to both. GetOrLoad
// makes concurrent misses of one key share a single load. Redis returns
// the layer's client for what a key-value cache does not do.
//
// Each replica has its own memory layer: a Delete clears this replica's
// memory and Redis, the other replicas' memory keeps the value until its
// TTL ends. Keep memory.ttl short for values that change, or use Redis
// alone.
//
// The breaker turns a Redis outage into cache misses: once open it skips
// Redis entirely, so a request costs a database read rather than a
// timeout, and Health stays nil. Without the breaker Health pings Redis,
// and the App waits for it at start.
//
// Importing the package sends go-redis's own log lines (a dial that
// failed, a connection dropped), process-wide, to slog.Default at warn
// instead of to stderr.
package cache
