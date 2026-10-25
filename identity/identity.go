package identity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zynthara/chok/v3"
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

	// The mails in flight: Close stops new ones and waits for these, and
	// cancels their contexts if it gives up.
	mailMu     sync.Mutex
	mailClosed bool
	mailing    sync.WaitGroup
	mailCtx    context.Context
	stopMail   context.CancelFunc

	stopSweep func() // ends the login code sweep and waits; nil without passwordless

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

// Sender gives identity a way to mail: it enables the forgot-password and
// reset-password routes, which are not mounted without it, and
// passwordless sign-in needs it.
func Sender(s MailSender) Option { return func(o *options) { o.sender = s } }

// Logger is where identity logs outside requests; the default is
// slog.Default(). On a request it logs with chok.LoggerFrom.
func Logger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// MailSender sends the mails identity asks for, typically through the
// mail battery with a template per Mail.Kind. identity calls it in the
// background, so a response never waits on it; an error is logged as a
// warning. ctx carries the request's values, not its cancellation.
type MailSender interface {
	Send(ctx context.Context, m Mail) error
}

// Mail is a mail identity asks the application to send; the application's
// MailSender picks the template by Kind.
type Mail struct {
	// Kind is what the mail is for: "password-reset" or "login-code".
	Kind string
	// To is the address to send it to.
	To string
	// Token is the password-reset token, which the reset page posts to
	// {prefix}/reset-password with the new password, or the login code's
	// magic-link token, which the sign-in page posts to
	// {prefix}/passwordless/verify.
	Token string
	// Code is the login code, auth.passwordless.code_length digits; empty
	// for a password reset.
	Code string
	// Link is the page that takes Token: auth.reset_url or
	// auth.passwordless.magic_link_url with ?token= added. Empty without
	// them, and the application builds its own from Token.
	Link string
	// ExpiresAt is when Token, and Code, stop working, in UTC.
	ExpiresAt time.Time
}

// New builds the identity battery on d: it registers the users,
// identities, audit_logs and auth_login_codes tables under the owner
// "identity" (created now in migrate: auto; from its embedded SQL files in
// migrate: versioned) and checks cfg and every provider. A Config from
// chok.Load carries the defaults; one built in code gets them for its
// zero fields.
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
	if cfg.Passwordless.Enabled && o.sender == nil {
		return nil, errors.New("identity: auth.passwordless.enabled needs a Sender to mail the codes: pass identity.Sender(...)")
	}
	idn := &Identity{cfg: cfg, d: d, logger: o.logger, sender: o.sender, cost: costOf()}
	// The mails outlive the requests that ask for them; Close ends them.
	idn.mailCtx, idn.stopMail = context.WithCancel(context.Background())
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
	if cfg.Passwordless.Enabled {
		idn.startSweep()
	}
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

// Close waits, within ctx, for the mails in flight and writes the queued
// audit events, and stops the background work: the login code sweep, the
// login throttle's sweeps and the OAuth session store's.
func (idn *Identity) Close(ctx context.Context) error {
	idn.closeOnce.Do(func() {
		errs := []error{idn.waitForMail(ctx)}
		if idn.stopSweep != nil {
			idn.stopSweep()
		}
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

// sendMail hands m to the Sender in the background, so that a response
// takes as long whether or not a mail goes out. The send keeps ctx's
// values — the request's logger and id — but not its cancellation; Close
// waits for it. A failure is a warning in the log.
func (idn *Identity) sendMail(ctx context.Context, m Mail) {
	logger := chok.LoggerFrom(ctx)
	idn.mailMu.Lock()
	if idn.mailClosed {
		idn.mailMu.Unlock()
		logger.WarnContext(ctx, "identity: closed; mail not sent", "kind", m.Kind, "to", maskEmail(m.To))
		return
	}
	idn.mailing.Add(1)
	idn.mailMu.Unlock()
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(idn.mailCtx, cancel)
	go func() {
		defer idn.mailing.Done()
		defer cancel()
		defer stop()
		defer func() {
			if p := recover(); p != nil {
				logger.ErrorContext(ctx, "identity: the MailSender panicked", "kind", m.Kind, "panic", p)
			}
		}()
		if err := idn.sender.Send(ctx, m); err != nil {
			logger.WarnContext(ctx, "identity: mail not sent", "kind", m.Kind, "to", maskEmail(m.To), "error", err)
		}
	}()
}

// waitForMail stops new mails and waits, within ctx, for the ones in
// flight; those still going when ctx ends have their contexts canceled.
func (idn *Identity) waitForMail(ctx context.Context) error {
	idn.mailMu.Lock()
	idn.mailClosed = true
	idn.mailMu.Unlock()
	defer idn.stopMail()
	done := make(chan struct{})
	go func() {
		idn.mailing.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("identity: closed before the mails in flight were sent: %w", ctx.Err())
	}
}

// withToken is base with token added as the query parameter token, ""
// without a base.
func withToken(base, token string) string {
	if base == "" {
		return ""
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "token=" + url.QueryEscape(token)
}
