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

	// ReadOnly removes schema and write capabilities from this instance.
	// The zero value preserves the historical read/write behaviour.
	ReadOnly bool `mapstructure:"read_only"`

	// Driver selects the blessed backend: sqlite | mysql | postgres.
	Driver string `mapstructure:"driver"`

	// Migrate picks the schema strategy (SPEC §5.3):
	//   auto      — gorm AutoMigrate over the module's WithTables specs
	//               (dev default, v1 behaviour)
	//   versioned — embedded forward-only migrations/*.sql with a
	//               schema_migrations ledger and a cross-process lock;
	//               built-in component schemas remain owner-managed
	//               (catalog: db.FrameworkTables)
	//   off       — the framework touches no schema at all, battery
	//               tables included; operations own DDL entirely
	Migrate string `mapstructure:"migrate" default:"auto"`

	// SlowThreshold controls module-managed slow-query logging. Queries
	// at or above the threshold are logged at Warn with parameter values
	// removed; query errors are logged independently. 0 disables only
	// slow-query logs. Library-level Open remains silent.
	SlowThreshold time.Duration `mapstructure:"slow_threshold" default:"200ms"`

	// MigrationStatusInterval refreshes versioned-migration metrics while
	// the process is running. The initial post-migrate sample always runs;
	// 0 disables subsequent refreshes.
	MigrationStatusInterval time.Duration `mapstructure:"migration_status_interval" default:"30s"`

	// Store is the app-level default posture for every Store built
	// over this instance's handle (SPEC §5.1; db-layer review #2).
	Store StorePolicy `mapstructure:"store"`

	SQLite   SQLiteOptions   `mapstructure:"sqlite"`
	MySQL    MySQLOptions    `mapstructure:"mysql"`
	Postgres PostgresOptions `mapstructure:"postgres"`
}

// StorePolicy is the "db.store" block (and per instance,
// "db.instances.<name>.store"): application-wide defaults applied to
// every store.New over this instance's handle, so production
// hardening is a config flip instead of a WithStrict() reminder at
// each construction site. Construction options override it per store
// — the explicit opt-outs are store.WithoutStrict and
// store.WithoutRequirePrincipal; store.WithMaxPageSize(0) /
// store.WithDefaultPageSize(0) restore the package defaults.
//
// The policy is baked into each Store at construction, so like every
// db field it is restart-only. The zero value changes nothing.
type StorePolicy struct {
	// Strict rejects auto-discovered field surfaces at store
	// construction (models must declare `store` tags, WithQueryFields /
	// WithUpdateFields, or explicitly consent via WithAllQueryFields /
	// WithAllUpdateFields) and makes ListFromQuery reject unknown
	// query parameters instead of dropping them.
	Strict bool `mapstructure:"strict"`

	// RequirePrincipal fail-closes Create / BatchCreate / Upsert on
	// db.Owned models when the context has no authenticated principal.
	// Background jobs that legitimately write Owned rows attach a
	// system principal or opt out per store.
	RequirePrincipal bool `mapstructure:"require_principal"`

	// MaxPageSize caps List / ListFromQuery page sizes; requests above
	// it are clamped. 0 = unlimited.
	MaxPageSize int `mapstructure:"max_page_size"`

	// DefaultPageSize is the page size when the client sends none.
	// 0 = the store package default (20).
	DefaultPageSize int `mapstructure:"default_page_size"`
}

func (p *StorePolicy) validate() error {
	if p.MaxPageSize < 0 {
		return fmt.Errorf("db: store: max_page_size must be >= 0 (0 = unlimited), got %d", p.MaxPageSize)
	}
	if p.DefaultPageSize < 0 {
		return fmt.Errorf("db: store: default_page_size must be >= 0 (0 = package default), got %d", p.DefaultPageSize)
	}
	if p.MaxPageSize > 0 && p.DefaultPageSize > p.MaxPageSize {
		return fmt.Errorf("db: store: default_page_size %d exceeds max_page_size %d", p.DefaultPageSize, p.MaxPageSize)
	}
	return nil
}

// SQLiteOptions configures the sqlite driver branch (pure-Go
// glebarez/modernc build — no CGO). File databases run a read/write
// split: writes serialize on one dedicated connection (fair Go-side
// queueing — SQLite physically allows a single writer per file),
// reads run in parallel on a WAL-snapshot pool. Memory databases pin
// a single connection and skip the split and the maintenance loop.
type SQLiteOptions struct {
	Path string `mapstructure:"path" default:"app.db"`

	// MaxOpenConns caps the READ pool (0 = max(4, NumCPU)). The write
	// side is always exactly one connection — that is what makes
	// writers queue fairly in Go instead of colliding on the file
	// lock — so this knob only tunes read parallelism. Idle
	// connections follow the same cap: reopening a SQLite connection
	// re-parses the schema, keeping them warm is free.
	MaxOpenConns int `mapstructure:"max_open_conns"`

	// CheckpointInterval is the cadence of the background
	// PRAGMA wal_checkpoint(TRUNCATE): it folds the WAL back into the
	// main file and truncates the log, so a busy writer next to
	// long-lived readers cannot grow the -wal file without bound.
	// 0 disables. Module-managed — library-level db.Open runs no
	// background maintenance.
	CheckpointInterval time.Duration `mapstructure:"checkpoint_interval" default:"5m"`

	// OptimizeInterval is the cadence of PRAGMA optimize — refreshes
	// the query planner's statistics, a cheap no-op when nothing
	// changed (SQLite's own recommendation for long-lived
	// connections). A final optimize also runs at module Close.
	// 0 disables. Module-managed, like CheckpointInterval.
	OptimizeInterval time.Duration `mapstructure:"optimize_interval" default:"1h"`
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
	if o.ReadOnly && o.Migrate == MigrateVersioned {
		return fmt.Errorf("db: read_only cannot be combined with migrate: versioned; use migrate: off")
	}
	if o.SlowThreshold < 0 {
		return fmt.Errorf("db: slow_threshold must be >= 0 (0 disables slow-query logs), got %s", o.SlowThreshold)
	}
	if o.MigrationStatusInterval < 0 {
		return fmt.Errorf("db: migration_status_interval must be >= 0 (0 disables periodic refresh), got %s", o.MigrationStatusInterval)
	}
	if err := o.Store.validate(); err != nil {
		return err
	}
	switch o.Driver {
	case "sqlite":
		if o.SQLite.Path == "" {
			return fmt.Errorf("db: sqlite: path must not be empty")
		}
		if o.SQLite.MaxOpenConns < 0 {
			return fmt.Errorf("db: sqlite: max_open_conns must be >= 0, got %d", o.SQLite.MaxOpenConns)
		}
		if o.SQLite.CheckpointInterval < 0 {
			return fmt.Errorf("db: sqlite: checkpoint_interval must be >= 0 (0 disables), got %s", o.SQLite.CheckpointInterval)
		}
		if o.SQLite.OptimizeInterval < 0 {
			return fmt.Errorf("db: sqlite: optimize_interval must be >= 0 (0 disables), got %s", o.SQLite.OptimizeInterval)
		}
		if o.ReadOnly && sqliteIsMemory(o.SQLite.Path) {
			return fmt.Errorf("db: sqlite: read_only requires a file database; an in-memory database is always empty")
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
