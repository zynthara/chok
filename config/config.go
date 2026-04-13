package config

import (
	"fmt"
	"time"
)

// Validatable is implemented by config structs that need validation.
// Run() calls Validate() on each sub-Options after unmarshal,
// then on the root config if it implements Validatable.
type Validatable interface {
	Validate() error
}

type HTTPOptions struct {
	Addr              string        `mapstructure:"addr"               default:":8080"`
	ReadTimeout       time.Duration `mapstructure:"read_timeout"       default:"30s"`
	WriteTimeout      time.Duration `mapstructure:"write_timeout"      default:"30s"`
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout" default:"10s"`
	IdleTimeout       time.Duration `mapstructure:"idle_timeout"       default:"120s"`
}

func (o *HTTPOptions) Validate() error {
	if o.Addr == "" {
		return fmt.Errorf("http: addr must not be empty")
	}
	if o.ReadTimeout < 0 {
		return fmt.Errorf("http: read_timeout must not be negative")
	}
	if o.WriteTimeout < 0 {
		return fmt.Errorf("http: write_timeout must not be negative")
	}
	if o.ReadHeaderTimeout < 0 {
		return fmt.Errorf("http: read_header_timeout must not be negative")
	}
	if o.IdleTimeout < 0 {
		return fmt.Errorf("http: idle_timeout must not be negative")
	}
	return nil
}

type MySQLOptions struct {
	Host            string        `mapstructure:"host"               default:"127.0.0.1"`
	Port            int           `mapstructure:"port"               default:"3306"`
	Username        string        `mapstructure:"username"`
	Password        string        `mapstructure:"password"`
	Database        string        `mapstructure:"database"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"     default:"100"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"     default:"10"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"  default:"1h"`
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time" default:"10m"`
}

func (o *MySQLOptions) Validate() error {
	if o.Host == "" {
		return fmt.Errorf("mysql: host must not be empty")
	}
	if o.Port <= 0 || o.Port > 65535 {
		return fmt.Errorf("mysql: port must be 1–65535, got %d", o.Port)
	}
	if o.Database == "" {
		return fmt.Errorf("mysql: database must not be empty")
	}
	return nil
}

type SQLiteOptions struct {
	Path string `mapstructure:"path" default:"app.db"`
}

func (o *SQLiteOptions) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("sqlite: path must not be empty")
	}
	return nil
}

type RedisOptions struct {
	Addr     string `mapstructure:"addr"     default:"127.0.0.1:6379"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"       default:"0"`
}

func (o *RedisOptions) Validate() error {
	if o.Addr == "" {
		return fmt.Errorf("redis: addr must not be empty")
	}
	if o.DB < 0 {
		return fmt.Errorf("redis: db must not be negative")
	}
	return nil
}

type SlogOptions struct {
	Level  string   `mapstructure:"level"  default:"info"`
	Format string   `mapstructure:"format" default:"json"`
	Output []string `mapstructure:"output" default:"stdout"`
}

func (o *SlogOptions) Validate() error {
	switch o.Level {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("log: unsupported level %q (use debug/info/warn/error)", o.Level)
	}
	switch o.Format {
	case "json", "text":
	default:
		return fmt.Errorf("log: unsupported format %q (use json/text)", o.Format)
	}
	if len(o.Output) == 0 {
		return fmt.Errorf("log: output must not be empty")
	}
	return nil
}

type CacheMemoryOptions struct {
	Enabled  bool          `mapstructure:"enabled"  default:"false"`
	Capacity int           `mapstructure:"capacity" default:"10000"`
	TTL      time.Duration `mapstructure:"ttl"      default:"5m"`
}

func (o *CacheMemoryOptions) Validate() error {
	if o.Enabled && o.Capacity <= 0 {
		return fmt.Errorf("cache.memory: capacity must be positive when enabled")
	}
	if o.TTL < 0 {
		return fmt.Errorf("cache.memory: ttl must not be negative")
	}
	return nil
}

type SwaggerOptions struct {
	Enabled    bool   `mapstructure:"enabled"     default:"false"`
	Title      string `mapstructure:"title"`
	Version    string `mapstructure:"version"     default:"1.0.0"`
	Prefix     string `mapstructure:"prefix"      default:"/swagger"`
	BearerAuth bool   `mapstructure:"bearer_auth" default:"true"`
}

type AccountOptions struct {
	Enabled         bool          `mapstructure:"enabled"          default:"false"`
	SigningKey      string        `mapstructure:"signing_key"`
	Expiration      time.Duration `mapstructure:"expiration"       default:"2h"`
	ResetExpiration time.Duration `mapstructure:"reset_expiration" default:"15m"`
}

func (o *AccountOptions) Validate() error {
	if !o.Enabled {
		return nil
	}
	if len(o.SigningKey) < 32 {
		return fmt.Errorf("account: signing_key must be at least 32 bytes")
	}
	if o.Expiration < 0 {
		return fmt.Errorf("account: expiration must not be negative")
	}
	if o.ResetExpiration < 0 {
		return fmt.Errorf("account: reset_expiration must not be negative")
	}
	return nil
}

type CacheFileOptions struct {
	Enabled bool          `mapstructure:"enabled" default:"false"`
	Path    string        `mapstructure:"path"    default:".cache"`
	TTL     time.Duration `mapstructure:"ttl"     default:"1h"`
}

func (o *CacheFileOptions) Validate() error {
	if o.Enabled && o.Path == "" {
		return fmt.Errorf("cache.file: path must not be empty when enabled")
	}
	if o.TTL < 0 {
		return fmt.Errorf("cache.file: ttl must not be negative")
	}
	return nil
}
