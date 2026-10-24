package db

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config is the db section.
type Config struct {
	Driver string `yaml:"driver" validate:"oneof=sqlite mysql postgres"`
	// Migrate is auto (AutoMigrate the declared tables), versioned (apply
	// the SQL files given to Migrations) or off (touch nothing, but fail
	// if versioned files are pending).
	Migrate string `yaml:"migrate" default:"auto" validate:"oneof=auto versioned off"`
	// SlowThreshold logs queries slower than it at warn; 0 disables.
	SlowThreshold time.Duration  `yaml:"slow_threshold" default:"200ms" validate:"gte=0"`
	SQLite        SQLiteConfig   `yaml:"sqlite"`
	MySQL         MySQLConfig    `yaml:"mysql"`
	Postgres      PostgresConfig `yaml:"postgres"`
	// Pool sizes the connection pool of MySQL and Postgres, and the read
	// pool of a SQLite file.
	Pool PoolConfig `yaml:"pool"`
	// Store is the default posture of every store.Store on this handle.
	Store StorePolicy `yaml:"store"`
}

// SQLiteConfig is db.sqlite. A file database runs in WAL mode with one
// writer connection and a separate pool of readers.
type SQLiteConfig struct {
	// Path is the database file, or ":memory:". Driver parameters ride
	// in the query string: app.db?_pragma=busy_timeout(10000).
	Path string `yaml:"path" default:"app.db"`
	// CheckpointInterval truncates the WAL periodically; 0 disables.
	CheckpointInterval time.Duration `yaml:"checkpoint_interval" default:"5m" validate:"gte=0"`
	// OptimizeInterval runs PRAGMA optimize periodically; 0 disables.
	OptimizeInterval time.Duration `yaml:"optimize_interval" default:"1h" validate:"gte=0"`
}

// MySQLConfig is db.mysql. Sessions always run in UTC: the DSN pins
// parseTime=true and loc=UTC and every connection sets time_zone '+00:00'.
type MySQLConfig struct {
	Host     string `yaml:"host" default:"127.0.0.1"`
	Port     int    `yaml:"port" default:"3306" validate:"gte=0,lte=65535"`
	Username string `yaml:"username"`
	Password string `yaml:"password" secret:"true"`
	Database string `yaml:"database"`
	// TLS is "", true, false, skip-verify or preferred; CACert, when set,
	// verifies the server against that PEM file instead.
	TLS    string `yaml:"tls" validate:"omitempty,oneof=true false skip-verify preferred"`
	CACert string `yaml:"ca_cert"`
}

// PostgresConfig is db.postgres: a DSN, or the discrete fields.
type PostgresConfig struct {
	DSN      string `yaml:"dsn" secret:"true"`
	Host     string `yaml:"host" default:"127.0.0.1"`
	Port     int    `yaml:"port" default:"5432" validate:"gte=0,lte=65535"`
	Username string `yaml:"username"`
	Password string `yaml:"password" secret:"true"`
	Database string `yaml:"database"`
	SSLMode  string `yaml:"ssl_mode" default:"disable" validate:"omitempty,oneof=disable require verify-ca verify-full"`
	CACert   string `yaml:"ca_cert"`
}

// PoolConfig is db.pool.
type PoolConfig struct {
	MaxOpenConns    int           `yaml:"max_open_conns" default:"100" validate:"gte=0"`
	MaxIdleConns    int           `yaml:"max_idle_conns" default:"10" validate:"gte=0"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime" default:"1h" validate:"gte=0"`
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time" default:"10m" validate:"gte=0"`
}

// StorePolicy is the posture a store.Store starts from: db.store in the
// configuration, or store.Policy for one store.
type StorePolicy struct {
	// RequirePrincipal makes writing an owner-scoped row without a
	// principal in the context fail instead of writing an unowned row.
	RequirePrincipal bool `yaml:"require_principal"`
	// AdminRoles see and write every owner's rows.
	AdminRoles []string `yaml:"admin_roles"`
	// OwnerScope fences models that embed an Owned base to the
	// principal's rows; nil means true.
	OwnerScope      *bool `yaml:"owner_scope"`
	DefaultPageSize int   `yaml:"default_page_size" default:"20" validate:"gte=0"`
	MaxPageSize     int   `yaml:"max_page_size" default:"100" validate:"gte=0"`
}

// Validate checks what tags cannot: the fields the chosen driver needs.
func (c *Config) Validate() error {
	var errs []error
	switch c.Driver {
	case "sqlite":
		if c.SQLite.Path == "" {
			errs = append(errs, errors.New("sqlite.path must not be empty"))
		}
	case "mysql":
		if c.MySQL.Host == "" {
			errs = append(errs, errors.New("mysql.host must not be empty"))
		}
		if c.MySQL.Database == "" {
			errs = append(errs, errors.New("mysql.database must not be empty"))
		}
	case "postgres":
		p := c.Postgres
		if p.DSN != "" && (p.Database != "" || p.Username != "" || p.Password != "") {
			errs = append(errs, errors.New("postgres.dsn and the discrete connection fields are mutually exclusive"))
		}
		if p.DSN == "" && p.Database == "" {
			errs = append(errs, errors.New("postgres needs dsn or database"))
		}
	}
	for i, r := range c.Store.AdminRoles {
		if strings.TrimSpace(r) == "" {
			errs = append(errs, fmt.Errorf("store.admin_roles[%d] must not be empty", i))
		}
	}
	if c.Store.MaxPageSize > 0 && c.Store.DefaultPageSize > c.Store.MaxPageSize {
		errs = append(errs, fmt.Errorf("store.default_page_size %d exceeds store.max_page_size %d", c.Store.DefaultPageSize, c.Store.MaxPageSize))
	}
	return errors.Join(errs...)
}

// String keeps the password out of logs.
func (c MySQLConfig) String() string {
	if c.Password != "" {
		c.Password = "***"
	}
	type plain MySQLConfig
	return fmt.Sprintf("%+v", plain(c))
}

// String keeps the password and the DSN out of logs.
func (c PostgresConfig) String() string {
	if c.Password != "" {
		c.Password = "***"
	}
	if c.DSN != "" {
		c.DSN = "***"
	}
	type plain PostgresConfig
	return fmt.Sprintf("%+v", plain(c))
}

// clone copies the slice so a store's policy never aliases the handle's.
func (p StorePolicy) clone() StorePolicy {
	p.AdminRoles = append([]string(nil), p.AdminRoles...)
	if p.OwnerScope != nil {
		v := *p.OwnerScope
		p.OwnerScope = &v
	}
	return p
}
