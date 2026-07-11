package account

import (
	"context"
	"fmt"
	"sort"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
)

// ModOpt configures the account module at assembly time.
type ModOpt func(*Component)

// WithProviders assembles OAuth provider specs. Which of them actually
// run is yaml's call (`account.providers.<name>.enabled`); an enabled
// yaml provider with no assembled spec fails startup, an assembled
// spec that yaml doesn't enable is skipped (kill switch).
func WithProviders(specs ...ProviderSpec) ModOpt {
	return func(c *Component) { c.specs = append(c.specs, specs...) }
}

// WithOptions appends library-level Options — the code-not-config
// knobs: WithSender for the forgot/reset flow, session-store injection
// for multi-replica OAuth, custom carriers. Later options win over the
// yaml-derived ones.
func WithOptions(opts ...Option) ModOpt {
	return func(c *Component) { c.extra = append(c.extra, opts...) }
}

// Module returns the account component for chok.Use. It mounts the
// full auth surface under /auth, migrates the users/identities schema
// (honouring the db migrate mode) and exposes the assembled *Service
// via the accessor for admin flows.
//
// Route protection for application routes goes through account.Authn:
//
//	api := r.Group("/api/v1", account.Authn(k))
func Module(opts ...ModOpt) kernel.Component {
	c := &Component{}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Component owns the account service lifecycle.
type Component struct {
	specs []ProviderSpec
	extra []Option

	opts Options
	svc  *Service
	mode string // db migrate mode captured at Init
	h    *db.DB
}

// Describe implements kernel.Component.
func (c *Component) Describe() kernel.Descriptor {
	return kernel.Descriptor{
		Kind:      "account",
		ConfigKey: "account",
		Options:   Options{},
		Schema:    kernel.SchemaOwner{Tables: []string{"users", "identities"}},
		Needs: []kernel.Dep{
			{Kind: "db"},
			{Kind: "log", Optional: true},
		},
	}
}

// Init decodes the section, builds the service and runs the provider
// assembly×config matrix. Schema creation belongs to Migrate.
func (c *Component) Init(ctx context.Context, k kernel.Kernel) error {
	if err := k.Config().Section("account", &c.opts); err != nil {
		return fmt.Errorf("account: decode section: %w", err)
	}
	logger, ok := k.Logger().(log.Logger)
	if !ok {
		logger = log.Empty()
	}

	dbc, ok := kernel.Get[interface {
		Handle() *db.DB
		MigrateMode() string
	}](k, "db")
	if !ok {
		return fmt.Errorf("account: db module not available")
	}
	c.h = dbc.Handle()
	if c.h == nil {
		return fmt.Errorf("account: db handle not initialised")
	}
	if c.h.ReadOnly() {
		return fmt.Errorf("account: db instance is read_only — account requires a writable database")
	}
	c.mode = dbc.MigrateMode()

	modOpts := c.optionsFromConfig()
	modOpts = append(modOpts, c.extra...)
	m, err := New(c.h, logger, modOpts...)
	if err != nil {
		return fmt.Errorf("account: %w", err)
	}

	if err := c.registerProviders(ctx, m); err != nil {
		_ = m.Close() // tear down the half-built service (limiter goroutine etc.)
		return err
	}
	c.svc = m
	return nil
}

// optionsFromConfig translates the decoded yaml section into
// library-level Options (the v1 OptionsFromConfig, now module-internal).
func (c *Component) optionsFromConfig() []Option {
	out := []Option{WithSigningKey(c.opts.SigningKey)}
	if c.opts.Expiration > 0 {
		out = append(out, WithExpiration(c.opts.Expiration))
	}
	if c.opts.ResetExpiration > 0 {
		out = append(out, WithResetExpiration(c.opts.ResetExpiration))
	}
	if c.opts.LoginRateWindow > 0 && c.opts.LoginRateLimit > 0 {
		out = append(out, WithLoginRateLimit(c.opts.LoginRateWindow, c.opts.LoginRateLimit))
	}
	if c.opts.DisableRegister {
		out = append(out, WithoutPublicRegister())
	}
	if c.opts.LinkByEmail {
		out = append(out, WithLinkByEmail(true))
	}
	if len(c.opts.AllowedRedirectBacks) > 0 {
		out = append(out, WithAllowedRedirectBacks(c.opts.AllowedRedirectBacks...))
	}
	if c.opts.OAuthCallbackFrontendURL != "" {
		out = append(out, WithOAuthCallbackFrontendURL(c.opts.OAuthCallbackFrontendURL))
	}
	return out
}

// registerProviders runs the assembly×config matrix in deterministic
// order: yaml-enabled + assembled ⇒ build and register; yaml-enabled
// with no assembled spec ⇒ fail-fast (a typo must not silently
// disable an IdP — the v1 unknown-provider posture); assembled but
// not yaml-enabled ⇒ skipped (kill switch); duplicate spec names ⇒
// fail-fast.
func (c *Component) registerProviders(ctx context.Context, m *Service) error {
	byName := make(map[string]ProviderSpec, len(c.specs))
	for _, spec := range c.specs {
		if spec.Name == "" || spec.Build == nil {
			return fmt.Errorf("account: WithProviders received an incomplete ProviderSpec (name %q)", spec.Name)
		}
		if _, dup := byName[spec.Name]; dup {
			return fmt.Errorf("account: provider %q assembled twice", spec.Name)
		}
		byName[spec.Name] = spec
	}

	names := make([]string, 0, len(c.opts.Providers))
	for name := range c.opts.Providers {
		names = append(names, name)
	}
	sort.Strings(names) // map iteration is randomised; keep registration deterministic

	for _, name := range names {
		raw := c.opts.Providers[name]
		if !raw.Enabled {
			continue
		}
		spec, ok := byName[name]
		if !ok {
			assembled := make([]string, 0, len(byName))
			for n := range byName {
				assembled = append(assembled, n)
			}
			sort.Strings(assembled)
			return fmt.Errorf("account: provider %q is enabled in config but not assembled — "+
				"add it to account.WithProviders(...) (assembled: %v)", name, assembled)
		}
		provider, err := spec.Build(ctx, raw.Raw)
		if err != nil {
			return fmt.Errorf("account: build provider %q: %w", name, err)
		}
		if err := m.RegisterProvider(provider); err != nil {
			return fmt.Errorf("account: register provider %q: %w", name, err)
		}
	}
	return nil
}

// Migrate implements kernel.Migrator: users/identities AutoMigrate +
// the has_password backfill, honouring the framework migrate mode
// (SPEC §5.3 — off touches neither schema nor maintenance DML;
// operations own both).
func (c *Component) Migrate(ctx context.Context) error {
	if c.mode == db.MigrateOff {
		c.svc.logger.Info("account: migrate mode off — users/identities schema untouched (operations own DDL)")
		return nil
	}
	if err := MigrateSchema(ctx, c.h); err != nil {
		return fmt.Errorf("account: migrate: %w", err)
	}
	return nil
}

// Mount implements kernel.Mounter: the full auth surface under /auth.
func (c *Component) Mount(r kernel.Router) error {
	c.svc.RegisterRoutes(r.Group("/auth"))
	return nil
}

// Close releases the service's background resources (rate limiter,
// OAuth session plumbing).
func (c *Component) Close(ctx context.Context) error {
	if c.svc == nil {
		return nil
	}
	return c.svc.Close()
}

// Service exposes the assembled account service for admin flows
// (UpdateUserRoles / SetUserActive / ...), Store() access and custom
// wiring. nil before Init.
func (c *Component) Service() *Service { return c.svc }

// Authn is the component-level role surface peers discover
// structurally (`interface{ Authn() kernel.Middleware }` against kind
// "account") — the audit admin API rides it. Same semantics as
// account.Authn(k): token verification + ActiveCheck. Only meaningful
// after Init (peers capture it there; ordering comes from their
// account Needs edge).
func (c *Component) Authn() kernel.Middleware { return c.svc.Authn() }

// Authn returns the blessed authentication middleware (token
// verification + ActiveCheck — the v1 AuthChain semantics) for
// protecting application routes:
//
//	api := r.Group("/api/v1", account.Authn(k))
//
// It panics with assembly guidance when the account module is absent,
// disabled or not yet initialised — inside a Routes callback that is
// an assembly error, and fail-fast beats a route that silently serves
// unauthenticated traffic (mirrors db.From).
func Authn(k kernel.Kernel) kernel.Middleware {
	ac, ok := kernel.Get[*Component](k, "account")
	if !ok || ac.Service() == nil {
		panic("account.Authn: account module not available — assemble chok.Use(account.Module()) and keep account.enabled true")
	}
	return ac.Service().Authn()
}
