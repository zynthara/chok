package identity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity/internal/authz"
	"github.com/zynthara/chok/v3/internal/conf"
	"github.com/zynthara/chok/v3/store"
)

// Identity is the identity battery: accounts with passwords and OAuth
// sign-in, the tokens that prove them, the middleware that checks those
// tokens, and the audit log. Register it with app.Use: it mounts its
// routes, reports health and closes.
type Identity struct {
	cfg    Config
	d      *db.DB
	logger *slog.Logger
	sender MailSender

	users *store.Store[User]
	links *store.Store[linkedIdentity]

	access *tokens // access tokens
	reset  *tokens // password-reset tokens

	cost    int    // bcrypt cost of every password hash
	padding string // a hash at that cost, compared against when the email is unknown

	limiter *loginLimiter // nil when the login throttle is off
	oauth   *oauthFlow    // nil without providers
	audit   *Audit

	authorize atomic.Pointer[authz.Func] // installed by identity/rbac

	closeOnce sync.Once
	closeErr  error
}

func init() {
	authz.Attach = func(idn any, fn authz.Func) { idn.(*Identity).authorize.Store(&fn) }
}

// Option configures New.
type Option func(*options)

type options struct {
	providers []Provider
	sender    MailSender
	logger    *slog.Logger
}

// Providers enables OAuth sign-in through the given providers, from
// identity/google, identity/github, identity/apple and identity/facebook.
func Providers(p ...Provider) Option {
	return func(o *options) { o.providers = append(o.providers, p...) }
}

// Sender enables the forgot-password and reset-password routes: s mails
// the reset token. Without it the routes are not mounted.
func Sender(s MailSender) Option { return func(o *options) { o.sender = s } }

// Logger is where identity logs outside requests; the default is
// slog.Default(). On a request it logs with chok.LoggerFrom.
func Logger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// MailSender delivers a password-reset token: put it in a link to the
// application's reset page, which posts it to {prefix}/reset-password.
type MailSender interface {
	Send(ctx context.Context, to, token string) error
}

// New builds the identity battery on d: it registers the users,
// identities and audit_logs tables under the owner "identity" (created
// now in migrate: auto; from its embedded SQL files in migrate:
// versioned) and checks cfg and every provider. A Config from chok.Load
// carries the defaults; one built in code gets them for its zero fields.
func New(cfg Config, d *db.DB, opts ...Option) (*Identity, error) {
	if d == nil {
		return nil, errors.New("identity: nil *db.DB")
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	cfg = cfg.withDefaults()
	if err := conf.Check(&cfg); err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	// A provider configured but not linked is a typo or a forgotten line:
	// failing beats a sign-in button that silently does nothing.
	linked := map[string]bool{}
	for _, p := range o.providers {
		if p != nil {
			linked[p.Name()] = true
		}
	}
	for name := range cfg.Providers {
		if !linked[name] {
			return nil, fmt.Errorf("identity: auth.providers.%s is configured but not linked: pass %s.New(cfg.Providers[%q]) to identity.Providers", name, name, name)
		}
	}
	idn := &Identity{cfg: cfg, d: d, logger: o.logger, sender: o.sender, cost: costOf()}
	var err error
	if idn.access, err = newTokens(cfg.SigningKey, issuerAccess, cfg.TokenTTL); err != nil {
		return nil, err
	}
	if idn.reset, err = newTokens(cfg.SigningKey, issuerReset, cfg.ResetTTL); err != nil {
		return nil, err
	}
	// The padding hash costs what a real one does, or the login of an
	// unknown email would answer faster and give the email away.
	if idn.padding, err = hashPassword("identity: constant-time padding", idn.cost); err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	// The providers are checked before anything touches the database.
	if len(o.providers) > 0 {
		if idn.oauth, err = newOAuthFlow(idn, o.providers); err != nil {
			return nil, err
		}
	}
	if err := d.RegisterSchema("identity", tables(), migrations()); err != nil {
		if idn.oauth != nil {
			_ = idn.oauth.close()
		}
		return nil, fmt.Errorf("identity: %w", err)
	}
	idn.users = store.New[User](d)
	idn.links = store.New[linkedIdentity](d)
	if cfg.LoginRateWindow > 0 && cfg.LoginRateLimit > 0 {
		idn.limiter = newLoginLimiter(cfg.LoginRateWindow, cfg.LoginRateLimit)
	}
	idn.audit = newAudit(d, cfg.Audit, o.logger)
	return idn, nil
}

// MustNew is New that panics on error.
func MustNew(cfg Config, d *db.DB, opts ...Option) *Identity {
	idn, err := New(cfg, d, opts...)
	if err != nil {
		panic(err)
	}
	return idn
}

// migrations are identity's SQL files for migrate: versioned, one
// directory per dialect.
func migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	return sub
}

// Users is the administration of accounts.
func (idn *Identity) Users() *UserService { return &UserService{idn: idn} }

// Audit is the audit log.
func (idn *Identity) Audit() *Audit { return idn.audit }

// Name is "identity", its name in the health report.
func (idn *Identity) Name() string { return "identity" }

// Health fails while the audit log cannot write: a write failed within
// the last minute.
func (idn *Identity) Health(context.Context) error { return idn.audit.health() }

// Close writes the queued audit events, within ctx, and stops the
// background work: the login throttle's sweeps and the OAuth session
// store's.
func (idn *Identity) Close(ctx context.Context) error {
	idn.closeOnce.Do(func() {
		var errs []error
		if idn.limiter != nil {
			idn.limiter.Close()
		}
		if idn.oauth != nil {
			errs = append(errs, idn.oauth.close())
		}
		errs = append(errs, idn.audit.close(ctx))
		idn.closeErr = errors.Join(errs...)
	})
	return idn.closeErr
}
