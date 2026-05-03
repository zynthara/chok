package parts

import (
	"fmt"
	"sort"

	"gorm.io/gorm"

	"github.com/zynthara/chok/account"
	"github.com/zynthara/chok/cache"
	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/db"
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
// LoginRateWindow + LoginRateLimit are forwarded to
// account.WithLoginRateLimit when both are positive. The pair-or-zero
// invariant is enforced upstream by AccountOptions.Validate so this
// builder can rely on the values being internally consistent.
//
// OAuth wiring (Phase 3): every entry in opts.Providers with
// enabled=true triggers a lookup in account's global provider factory
// registry (populated by provider packages' init() via
// account.RegisterProviderFactory). Unknown provider names cause a
// fail-fast error at builder time so a typo in chok.yaml doesn't
// silently disable an IdP. The factory receives the entry's
// *config.ProviderRawOptions so it can decode provider-specific keys
// from Raw via raw.Decode(&typedOpts).
func DefaultAccountBuilder(opts *config.AccountOptions) AccountBuilder {
	return func(k component.Kernel, gdb *gorm.DB) (*account.Module, error) {
		if opts == nil || !opts.Enabled {
			return nil, nil
		}
		aopts := []account.Option{
			account.WithSigningKey(opts.SigningKey),
		}
		if opts.Expiration > 0 {
			aopts = append(aopts, account.WithExpiration(opts.Expiration))
		}
		if opts.ResetExpiration > 0 {
			aopts = append(aopts, account.WithResetExpiration(opts.ResetExpiration))
		}
		if opts.LoginRateWindow > 0 && opts.LoginRateLimit > 0 {
			aopts = append(aopts, account.WithLoginRateLimit(opts.LoginRateWindow, opts.LoginRateLimit))
		}
		if opts.DisableRegister {
			aopts = append(aopts, account.WithoutPublicRegister())
		}
		if opts.LinkByEmail {
			aopts = append(aopts, account.WithLinkByEmail(true))
		}
		if len(opts.AllowedRedirectBacks) > 0 {
			aopts = append(aopts, account.WithAllowedRedirectBacks(opts.AllowedRedirectBacks...))
		}
		if opts.OAuthCallbackFrontendURL != "" {
			aopts = append(aopts, account.WithOAuthCallbackFrontendURL(opts.OAuthCallbackFrontendURL))
		}

		m, err := account.New(gdb, k.Logger(), aopts...)
		if err != nil {
			return nil, err
		}

		// Provider auto-registration. Iterate in sorted order so the
		// resulting Set-Cookie / route order is deterministic across
		// runs (map iteration is randomized).
		names := make([]string, 0, len(opts.Providers))
		for name := range opts.Providers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			raw := opts.Providers[name]
			if !raw.Enabled {
				continue
			}
			factory, ok := account.LookupProviderFactory(name)
			if !ok {
				return nil, fmt.Errorf("account: provider %q is enabled in config but no factory is registered "+
					"(missing `_ \"github.com/zynthara/chok/account/providers/%s\"` import?)",
					name, name)
			}
			provider, err := factory(&raw)
			if err != nil {
				return nil, fmt.Errorf("account: build provider %q: %w", name, err)
			}
			if err := m.RegisterProvider(provider); err != nil {
				return nil, fmt.Errorf("account: register provider %q: %w", name, err)
			}
		}
		return m, nil
	}
}

// DefaultSwaggerResolver returns a SwaggerResolver that maps
// config.SwaggerOptions to SwaggerSettings.
func DefaultSwaggerResolver(opts *config.SwaggerOptions) SwaggerResolver {
	return func(any) *SwaggerSettings {
		if opts == nil || !opts.Enabled {
			return nil
		}
		return &SwaggerSettings{
			Enabled:    true,
			Title:      opts.Title,
			Version:    opts.Version,
			Prefix:     opts.Prefix,
			BearerAuth: opts.BearerAuth,
		}
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

// DefaultHTTPResolver returns an HTTPResolver that forwards
// config.HTTPOptions directly.
func DefaultHTTPResolver(opts *config.HTTPOptions) HTTPResolver {
	return func(any) *config.HTTPOptions {
		return opts
	}
}

// DefaultCacheBuilder returns a CacheBuilder that constructs a multi-layer
// cache (memory → file → Redis) from discovered config. Redis is added only
// when a RedisComponent is available in the registry at Init time.
func DefaultCacheBuilder(memOpts *config.CacheMemoryOptions, fileOpts *config.CacheFileOptions) CacheBuilder {
	return func(k component.Kernel) (cache.Cache, error) {
		var bopts cache.BuildOptions
		bopts.Logger = k.Logger()

		if memOpts != nil && memOpts.Enabled {
			bopts.Memory = &cache.MemoryOptions{
				Capacity: memOpts.Capacity,
				TTL:      memOpts.TTL,
			}
		}
		if fileOpts != nil && fileOpts.Enabled {
			bopts.File = &cache.FileOptions{
				Path: fileOpts.Path,
				TTL:  fileOpts.TTL,
			}
		}
		if rc, ok := k.Get("redis").(*RedisComponent); ok && rc != nil {
			bopts.Redis = rc.Client()
		}

		return cache.Build(bopts)
	}
}

// --- helpers for auto-register callers ---

// NewDefaultHTTPComponent is a shorthand for the common case.
// Returns nil when opts is nil.
func NewDefaultHTTPComponent(opts *config.HTTPOptions) *HTTPComponent {
	if opts == nil || !opts.Enabled {
		return nil
	}
	return NewHTTPComponent(DefaultHTTPResolver(opts))
}

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

// NewDefaultSwaggerComponent is a shorthand for the common case.
// Returns nil when opts is nil or Enabled is false.
func NewDefaultSwaggerComponent(opts *config.SwaggerOptions) *SwaggerComponent {
	if opts == nil || !opts.Enabled {
		return nil
	}
	return NewSwaggerComponent(DefaultSwaggerResolver(opts))
}

// NewDefaultRedisComponent is a shorthand for the common case.
// Returns nil when opts is nil.
func NewDefaultRedisComponent(opts *config.RedisOptions) *RedisComponent {
	if opts == nil || !opts.Enabled {
		return nil
	}
	return NewRedisComponent(DefaultRedisResolver(opts))
}
