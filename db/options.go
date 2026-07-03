package db

import (
	"fmt"
	"time"

	"github.com/zynthara/chok/v2/conf"
)

// Options is the "db" yaml section (and each named instance's
// "db.instances.<name>" section). The driver field is the
// discriminator: it selects which nested block is validated and used;
// the others are ignored. Connection parameters cannot be swapped
// under a live pool, so every field here is restart-only (untagged =
// restart, the conservative conf default).
type Options struct {
	Enabled bool `mapstructure:"enabled" default:"true"`

	// Driver selects the blessed backend: sqlite | mysql | postgres.
	Driver string `mapstructure:"driver"`

	// Migrate picks the schema strategy (SPEC §5.3):
	//   auto      — gorm AutoMigrate over the module's WithTables specs
	//               (dev default, v1 behaviour)
	//   versioned — embedded forward-only migrations/*.sql with a
	//               schema_migrations ledger and a cross-process lock;
	//               framework battery tables stay AutoMigrate-managed
	//               (whitelist: db.FrameworkTables)
	//   off       — the framework touches no schema at all, battery
	//               tables included; operations own DDL entirely
	Migrate string `mapstructure:"migrate" default:"auto"`

	SQLite   SQLiteOptions   `mapstructure:"sqlite"`
	MySQL    MySQLOptions    `mapstructure:"mysql"`
	Postgres PostgresOptions `mapstructure:"postgres"`
}

// SQLiteOptions configures the sqlite driver branch.
type SQLiteOptions struct {
	Path string `mapstructure:"path" default:"app.db"`
}

// MySQLOptions configures the mysql driver branch. TLS and CACert are
// the toffs v0.4.2 back-port: TLS maps to go-sql-driver's built-in
// modes; a non-empty CACert takes precedence and builds a verifying
// per-host tls.Config against that CA (managed databases presenting a
// private-CA certificate — DigitalOcean, RDS — need exactly this).
type MySQLOptions struct {
	Host     string `mapstructure:"host"     default:"127.0.0.1"`
	Port     int    `mapstructure:"port"     default:"3306"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password" sensitive:"true"`
	Database string `mapstructure:"database"`

	// TLS: "" (plaintext) | "true" (verify against system roots) |
	// "false" | "skip-verify" (encrypt without verification) |
	// "preferred" (TLS when offered). Ignored when CACert is set.
	TLS    string `mapstructure:"tls"`
	CACert string `mapstructure:"ca_cert"`

	MaxOpenConns    int           `mapstructure:"max_open_conns"     default:"100"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"     default:"10"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"  default:"1h"`
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time" default:"10m"`
}

// PostgresOptions configures the postgres driver branch (pgx via the
// gorm driver). Either a full DSN or discrete fields — not both. The
// DSN is treated as sensitive wholesale because it customarily embeds
// credentials.
type PostgresOptions struct {
	DSN string `mapstructure:"dsn" sensitive:"true"`

	Host     string `mapstructure:"host"     default:"127.0.0.1"`
	Port     int    `mapstructure:"port"     default:"5432"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password" sensitive:"true"`
	Database string `mapstructure:"database"`

	// SSLMode: disable | require | verify-ca | verify-full. CACert maps
	// to sslrootcert for private-CA managed databases.
	SSLMode string `mapstructure:"ssl_mode" default:"disable"`
	CACert  string `mapstructure:"ca_cert"`

	MaxOpenConns    int           `mapstructure:"max_open_conns"     default:"100"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"     default:"10"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"  default:"1h"`
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time" default:"10m"`
}

// Migrate mode constants (Options.Migrate).
const (
	MigrateAuto      = "auto"
	MigrateVersioned = "versioned"
	MigrateOff       = "off"
)

// IsSelfValidating marks Options as a discriminator config: Validate
// covers exactly the selected branch, and the recursive walker must
// not descend into the unselected ones (conf.SelfValidating).
func (*Options) IsSelfValidating() {}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	switch o.Migrate {
	case MigrateAuto, MigrateVersioned, MigrateOff:
	default:
		return fmt.Errorf("db: migrate must be one of auto|versioned|off, got %q", o.Migrate)
	}
	switch o.Driver {
	case "sqlite":
		if o.SQLite.Path == "" {
			return fmt.Errorf("db: sqlite: path must not be empty")
		}
		return nil
	case "mysql":
		return o.MySQL.validate()
	case "postgres":
		return o.Postgres.validate()
	case "":
		return fmt.Errorf("db: driver must be set (sqlite|mysql|postgres); use enabled: false to turn the module off")
	default:
		return fmt.Errorf("db: unsupported driver %q (use sqlite, mysql or postgres)", o.Driver)
	}
}

func (o *MySQLOptions) validate() error {
	if o.Host == "" {
		return fmt.Errorf("db: mysql: host must not be empty")
	}
	if o.Port <= 0 || o.Port > 65535 {
		return fmt.Errorf("db: mysql: port must be 1-65535, got %d", o.Port)
	}
	if o.Database == "" {
		return fmt.Errorf("db: mysql: database must not be empty")
	}
	switch o.TLS {
	case "", "true", "false", "skip-verify", "preferred":
	default:
		return fmt.Errorf(`db: mysql: tls must be one of ""|true|false|skip-verify|preferred, got %q`, o.TLS)
	}
	return nil
}

func (o *PostgresOptions) validate() error {
	if o.DSN != "" {
		if o.Database != "" || o.Username != "" || o.Password != "" {
			return fmt.Errorf("db: postgres: dsn and discrete connection fields are mutually exclusive")
		}
		return nil
	}
	if o.Host == "" {
		return fmt.Errorf("db: postgres: host must not be empty")
	}
	if o.Port <= 0 || o.Port > 65535 {
		return fmt.Errorf("db: postgres: port must be 1-65535, got %d", o.Port)
	}
	if o.Database == "" {
		return fmt.Errorf("db: postgres: database must not be empty")
	}
	switch o.SSLMode {
	case "disable", "require", "verify-ca", "verify-full":
	default:
		return fmt.Errorf("db: postgres: ssl_mode must be one of disable|require|verify-ca|verify-full, got %q", o.SSLMode)
	}
	return nil
}

// Method-less twins: %#v inside GoString must print raw fields without
// re-entering GoString (conf.Redact godoc pattern).
type (
	optionsRaw         Options
	mysqlOptionsRaw    MySQLOptions
	postgresOptionsRaw PostgresOptions
)

// GoString/String mask credentials so %#v, %v and %+v logging cannot
// leak them (fmt consults GoString for %#v and String for %v/%+v).

func (o Options) GoString() string { return fmt.Sprintf("%#v", conf.Redact(optionsRaw(o))) }

// String implements fmt.Stringer with the same redaction as GoString.
func (o Options) String() string { return o.GoString() }

// GoString masks the password so %#v logging cannot leak it.
func (o MySQLOptions) GoString() string { return fmt.Sprintf("%#v", conf.Redact(mysqlOptionsRaw(o))) }

// String implements fmt.Stringer with the same redaction as GoString.
func (o MySQLOptions) String() string { return o.GoString() }

// GoString masks the password and DSN so %#v logging cannot leak them.
func (o PostgresOptions) GoString() string {
	return fmt.Sprintf("%#v", conf.Redact(postgresOptionsRaw(o)))
}

// String implements fmt.Stringer with the same redaction as GoString.
func (o PostgresOptions) String() string { return o.GoString() }
