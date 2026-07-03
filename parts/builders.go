package parts

import (
	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/account"
	"github.com/zynthara/chok/v2/cache"
	"github.com/zynthara/chok/v2/component"
	"github.com/zynthara/chok/v2/config"
	"github.com/zynthara/chok/v2/db"
)

// SQLiteBuilder returns a DBBuilder that opens a SQLite connection from
// the provided options. Use with NewDBComponent for auto-registration:
//
//	parts.NewDBComponent(parts.SQLiteBuilder(opts), tables...)
func SQLiteBuilder(opts *config.SQLiteOptions) DBBuilder {
	return func(_ component.Kernel) (*gorm.DB, error) {
		return db.NewSQLite(opts)
	}
}

// MySQLBuilder returns a DBBuilder that opens a MySQL connection from
// the provided options.
func MySQLBuilder(opts *config.MySQLOptions) DBBuilder {
	return func(_ component.Kernel) (*gorm.DB, error) {
		return db.NewMySQL(opts)
	}
}

// DefaultAccountBuilder returns an AccountBuilder that creates an
// account.Module from config.AccountOptions. Returns (nil, nil) when
// opts is nil or Enabled is false, putting the component into disabled
// mode (Mount/Migrate/Module become no-ops).
//
// Delegates all yaml→Option mapping and provider loop to
// account.OptionsFromConfig + account.RegisterConfiguredProviders so
// the builder and the standalone account.Setup entry stay in sync.
// Earlier divergence (Setup forwarded only signing key + expirations
// while the builder also handled the new OAuth fields) silently broke
// standalone Setup callers using yaml-driven OAuth.
func DefaultAccountBuilder(opts *config.AccountOptions) AccountBuilder {
	return func(k component.Kernel, gdb *gorm.DB) (*account.Module, error) {
		if opts == nil || !opts.Enabled {
			return nil, nil
		}
		m, err := account.New(gdb, k.Logger(), account.OptionsFromConfig(opts)...)
		if err != nil {
			return nil, err
		}
		if err := account.RegisterConfiguredProviders(m, opts); err != nil {
			_ = m.Close() // tear down half-built module on provider failure
			return nil, err
		}
		return m, nil
	}
}

// DefaultRedisResolver returns a RedisResolver that forwards
// config.RedisOptions. Returns nil when opts is nil, disabling the
// component.
func DefaultRedisResolver(opts *config.RedisOptions) RedisResolver {
	return func(any) *config.RedisOptions {
		return opts
	}
}

// DefaultCacheBuilder returns a CacheBuilder that constructs a multi-layer
// cache (memory → Redis) from discovered config. Redis is added only
// when a RedisComponent is available in the registry at Init time.
// The badger file layer is gone since the M4 migration (SPEC §2.4);
// this v1-residue glue dies with parts/.
func DefaultCacheBuilder(memOpts *config.CacheMemoryOptions) CacheBuilder {
	return func(k component.Kernel) (cache.Cache, error) {
		var bopts cache.BuildOptions

		if memOpts != nil && memOpts.Enabled {
			bopts.Memory = &cache.MemoryOptions{
				Capacity: memOpts.Capacity,
				TTL:      memOpts.TTL,
			}
		}
		if rc, ok := k.Get("redis").(*RedisComponent); ok && rc != nil {
			bopts.Redis = rc.Client()
		}

		return cache.Build(bopts)
	}
}

// --- helpers for auto-register callers ---

// NewDefaultDBComponent is a shorthand for the common case:
// SQLite or MySQL builder from discovered config, plus user-supplied
// table specs. Returns nil when no options are provided.
func NewDefaultDBComponent(sqlite *config.SQLiteOptions, mysql *config.MySQLOptions, tables []db.TableSpec) *DBComponent {
	if sqlite != nil && sqlite.Enabled {
		return NewDBComponent(SQLiteBuilder(sqlite), tables...)
	}
	if mysql != nil && mysql.Enabled {
		return NewDBComponent(MySQLBuilder(mysql), tables...)
	}
	return nil
}

// NewDefaultAccountComponent is a shorthand for the common case.
// Returns nil when opts is nil or Enabled is false.
func NewDefaultAccountComponent(opts *config.AccountOptions) *AccountComponent {
	if opts == nil || !opts.Enabled {
		return nil
	}
	return NewAccountComponent(DefaultAccountBuilder(opts), "/auth")
}

// NewDefaultRedisComponent is a shorthand for the common case.
// Returns nil when opts is nil.
func NewDefaultRedisComponent(opts *config.RedisOptions) *RedisComponent {
	if opts == nil || !opts.Enabled {
		return nil
	}
	return NewRedisComponent(DefaultRedisResolver(opts))
}
