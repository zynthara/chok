package cache

import (
	"errors"
	"fmt"
	"time"
)

// Config is the cache section of an application's configuration: a
// memory layer in the process, a Redis layer shared by the replicas, or
// both (memory first), and a circuit breaker in front of Redis.
//
//	cache:
//	  memory:
//	    enabled: true
//	    capacity: 10000
//	    ttl: 5m
//	  redis:
//	    enabled: true
//	    addr: redis.internal:6380
//	    tls: true
//	    ttl: 10m
//	  breaker:
//	    enabled: true
//
// The password is a secret: set it through the environment
// ({APP}_CACHE_REDIS_PASSWORD) rather than in the file. New gives the zero
// fields of a Config built in code the defaults below; the switches have
// none.
type Config struct {
	Memory struct {
		// Enabled keeps entries in this process. Each replica has its own:
		// a Delete reaches this replica's memory and Redis, not the other
		// replicas' memory, so keep TTL short for data that changes.
		Enabled bool `yaml:"enabled"`
		// Capacity is the most entries kept; the least likely to be used
		// again go first.
		Capacity int `yaml:"capacity" default:"10000"`
		// TTL is how long an entry lives when Set is given no TTL.
		TTL time.Duration `yaml:"ttl" default:"5m"`
	} `yaml:"memory"`
	Redis struct {
		// Enabled keeps entries in Redis, shared by every replica.
		Enabled  bool   `yaml:"enabled"`
		Addr     string `yaml:"addr" default:"127.0.0.1:6379"`
		Username string `yaml:"username"` // a Redis 6 ACL user; empty is the default user
		Password string `yaml:"password" secret:"true"`
		DB       int    `yaml:"db"`
		// TLS encrypts the connection, verifying the server against the
		// system roots, or against CACert when it is set (CACert alone
		// turns TLS on), as managed Redis services require.
		TLS    bool   `yaml:"tls"`
		CACert string `yaml:"ca_cert"`
		// The timeouts are short on purpose: a cache that answers late
		// is worse than a miss.
		DialTimeout  time.Duration `yaml:"dial_timeout" default:"1s"`
		ReadTimeout  time.Duration `yaml:"read_timeout" default:"500ms"`
		WriteTimeout time.Duration `yaml:"write_timeout" default:"500ms"`
		PoolSize     int           `yaml:"pool_size" default:"10"`
		// TTL is how long an entry lives when Set is given no TTL.
		TTL time.Duration `yaml:"ttl" default:"10m"`
	} `yaml:"redis"`
	Breaker struct {
		// Enabled puts a circuit breaker in front of Redis: after
		// Threshold failures in a row Redis is left alone for
		// ResetTimeout, its reads are misses and its writes are skipped;
		// then one probe at a time goes through, and HalfOpenSuccesses
		// successes in a row close the breaker again. A Redis that is
		// down then costs a miss instead of a timeout per call, and the
		// Cache reports healthy: the application serves from its
		// database meanwhile.
		Enabled           bool          `yaml:"enabled"`
		Threshold         int           `yaml:"threshold" default:"5"`
		ResetTimeout      time.Duration `yaml:"reset_timeout" default:"30s"`
		HalfOpenSuccesses int           `yaml:"half_open_successes" default:"3"`
		// ProbeTimeout bounds a probe on its own, so a caller's nearly
		// spent deadline cannot fail it; it is at most ResetTimeout.
		ProbeTimeout time.Duration `yaml:"probe_timeout" default:"2s"`
	} `yaml:"breaker"`
}

// The defaults New gives the zero fields of a Config built in code.
const (
	defaultCapacity          = 10_000
	defaultMemoryTTL         = 5 * time.Minute
	defaultAddr              = "127.0.0.1:6379"
	defaultDialTimeout       = time.Second
	defaultIOTimeout         = 500 * time.Millisecond
	defaultPoolSize          = 10
	defaultRedisTTL          = 10 * time.Minute
	defaultThreshold         = 5
	defaultResetTimeout      = 30 * time.Second
	defaultHalfOpenSuccesses = 3
	defaultProbeTimeout      = 2 * time.Second
)

func (c Config) withDefaults() Config {
	m, r, b := &c.Memory, &c.Redis, &c.Breaker
	setInt(&m.Capacity, defaultCapacity)
	setDuration(&m.TTL, defaultMemoryTTL)
	if r.Addr == "" {
		r.Addr = defaultAddr
	}
	setDuration(&r.DialTimeout, defaultDialTimeout)
	setDuration(&r.ReadTimeout, defaultIOTimeout)
	setDuration(&r.WriteTimeout, defaultIOTimeout)
	setInt(&r.PoolSize, defaultPoolSize)
	setDuration(&r.TTL, defaultRedisTTL)
	setInt(&b.Threshold, defaultThreshold)
	setDuration(&b.ResetTimeout, defaultResetTimeout)
	setInt(&b.HalfOpenSuccesses, defaultHalfOpenSuccesses)
	setDuration(&b.ProbeTimeout, defaultProbeTimeout)
	return c
}

func setInt(v *int, def int) {
	if *v == 0 {
		*v = def
	}
}

func setDuration(v *time.Duration, def time.Duration) {
	if *v == 0 {
		*v = def
	}
}

// Validate checks the layers that are switched on. It runs when
// chok.Load reads the configuration and again in New; that no layer is
// on at all is New's error, not the configuration's.
func (c *Config) Validate() error {
	var errs []error
	positive := func(key string, ok bool, v any) {
		if !ok {
			errs = append(errs, fmt.Errorf("%s must be positive, got %v", key, v))
		}
	}
	if m := c.Memory; m.Enabled {
		positive("memory.capacity", m.Capacity > 0, m.Capacity)
		positive("memory.ttl", m.TTL > 0, m.TTL)
	}
	if r := c.Redis; r.Enabled {
		if r.Addr == "" {
			errs = append(errs, errors.New("redis.addr must not be empty"))
		}
		if r.DB < 0 {
			errs = append(errs, fmt.Errorf("redis.db must not be negative, got %d", r.DB))
		}
		positive("redis.dial_timeout", r.DialTimeout > 0, r.DialTimeout)
		positive("redis.read_timeout", r.ReadTimeout > 0, r.ReadTimeout)
		positive("redis.write_timeout", r.WriteTimeout > 0, r.WriteTimeout)
		positive("redis.pool_size", r.PoolSize > 0, r.PoolSize)
		positive("redis.ttl", r.TTL > 0, r.TTL)
	}
	if b := c.Breaker; b.Enabled {
		if !c.Redis.Enabled {
			errs = append(errs, errors.New("breaker guards the redis layer: enable redis or disable breaker"))
		}
		positive("breaker.threshold", b.Threshold > 0, b.Threshold)
		positive("breaker.reset_timeout", b.ResetTimeout > 0, b.ResetTimeout)
		positive("breaker.half_open_successes", b.HalfOpenSuccesses > 0, b.HalfOpenSuccesses)
		positive("breaker.probe_timeout", b.ProbeTimeout > 0, b.ProbeTimeout)
	}
	return errors.Join(errs...)
}

// String keeps the Redis password out of logs.
func (c Config) String() string {
	if c.Redis.Password != "" {
		c.Redis.Password = "***"
	}
	type plain Config
	return fmt.Sprintf("%+v", plain(c))
}

// GoString keeps the Redis password out of %#v.
func (c Config) GoString() string {
	if c.Redis.Password != "" {
		c.Redis.Password = "***"
	}
	type plain Config
	return fmt.Sprintf("%#v", plain(c))
}
