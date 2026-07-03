package redis

import (
	"fmt"
	"time"

	"github.com/zynthara/chok/v2/conf"
)

// Options is the "redis" yaml section. Connection parameters cannot be
// swapped under a live client, so every field is restart-only
// (untagged = restart, the conservative conf default).
//
// TLS / CACert / Username are the toffs v0.4.2 back-port: TLS turns on
// in-transit encryption (managed Redis — DigitalOcean Valkey, AWS
// ElastiCache — requires it); a non-empty CACert implies TLS and
// verifies the server certificate against that CA instead of the
// system roots (managed providers present a private-CA certificate).
type Options struct {
	Enabled bool   `mapstructure:"enabled" default:"true"`
	Addr    string `mapstructure:"addr"    default:"127.0.0.1:6379"`

	// Username selects a Redis 6+ ACL user; empty uses the default
	// user (AUTH <password> semantics).
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password" sensitive:"true"`
	DB       int    `mapstructure:"db"       default:"0"`

	// Network timeouts. Defaults are tighter than go-redis' library
	// defaults (DialTimeout 5s, ReadTimeout 3s) because Redis on the hot
	// path of a web request should fail fast and let the caller fall back
	// (cache miss, circuit breaker) instead of stretching every request
	// to the library timeout.
	DialTimeout  time.Duration `mapstructure:"dial_timeout"  default:"1s"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"  default:"500ms"`
	WriteTimeout time.Duration `mapstructure:"write_timeout" default:"500ms"`
	PoolTimeout  time.Duration `mapstructure:"pool_timeout"  default:"1s"`
	PoolSize     int           `mapstructure:"pool_size"     default:"10"`

	// TLS enables in-transit encryption (rediss). CACert existence
	// checks happen lazily in TLSConfigFor, not in Validate — the file
	// only has to exist on the machine that opens the connection.
	TLS    bool   `mapstructure:"tls"`
	CACert string `mapstructure:"ca_cert"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Addr == "" {
		return fmt.Errorf("redis: addr must not be empty")
	}
	if o.DB < 0 {
		return fmt.Errorf("redis: db must not be negative")
	}
	if o.DialTimeout < 0 {
		return fmt.Errorf("redis: dial_timeout must not be negative")
	}
	if o.ReadTimeout < 0 {
		return fmt.Errorf("redis: read_timeout must not be negative")
	}
	if o.WriteTimeout < 0 {
		return fmt.Errorf("redis: write_timeout must not be negative")
	}
	if o.PoolTimeout < 0 {
		return fmt.Errorf("redis: pool_timeout must not be negative")
	}
	if o.PoolSize < 0 {
		return fmt.Errorf("redis: pool_size must not be negative")
	}
	return nil
}

// optionsRaw is the method-less twin: %#v inside GoString must print
// raw fields without re-entering GoString (conf.Redact godoc pattern).
type optionsRaw Options

// GoString masks the password so %#v logging cannot leak it.
func (o Options) GoString() string { return fmt.Sprintf("%#v", conf.Redact(optionsRaw(o))) }

// String implements fmt.Stringer with the same redaction as GoString.
func (o Options) String() string { return o.GoString() }
