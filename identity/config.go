package identity

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Config is the auth section of an application's configuration:
//
//	auth:
//	  token_ttl: 2h
//	  providers:
//	    google:
//	      client_id: 1234.apps.googleusercontent.com
//	      redirect_url: https://api.example.com/auth/google/callback
//	  oauth_callback_frontend_url: https://app.example.com/signed-in
//
// SigningKey is a secret: set it from the environment
// (BLOG_AUTH_SIGNING_KEY for an app loaded as "blog"), never in a file.
// New gives an empty Prefix and a zero TokenTTL, RefreshTTL, ResetTTL or
// Audit.AsyncBuffer the defaults below, whether the Config comes from a
// file or from code; a zero login throttle is off and a zero
// Audit.Retention keeps records forever.
type Config struct {
	// Prefix is where the routes mount: {prefix}/register, {prefix}/login, ...
	Prefix string `yaml:"prefix" default:"/auth"`
	// SigningKey signs the access and password-reset tokens (HS256); at
	// least 32 bytes.
	SigningKey string `yaml:"signing_key" secret:"true"`
	// TokenTTL is how long an access token is valid.
	TokenTTL time.Duration `yaml:"token_ttl" default:"2h"`
	// RefreshTTL bounds a session: refresh-token issues fresh tokens until
	// RefreshTTL after the login that started the session, then the user
	// logs in again. Every session ends: 0 takes the default.
	RefreshTTL time.Duration `yaml:"refresh_ttl" default:"168h"`
	// ResetTTL is how long a password-reset token is valid.
	ResetTTL time.Duration `yaml:"reset_ttl" default:"15m"`
	// ResetURL is the front end's password-reset page, an absolute URL:
	// given, the password-reset Mail's Link is ResetURL?token=<token>, for
	// the page to post to {prefix}/reset-password; without it the
	// application builds the link from Mail.Token.
	ResetURL string `yaml:"reset_url"`
	// LoginRateWindow and LoginRateLimit throttle failed logins: once an
	// email, or a client IP, has failed LoginRateLimit times within the
	// window, login answers 429 until the window slides. Both 0 turn the
	// throttle off.
	LoginRateWindow time.Duration `yaml:"login_rate_window" default:"15m"`
	LoginRateLimit  int           `yaml:"login_rate_limit" default:"10"`
	// DisableRegister drops the register route: accounts come from
	// Users().Create.
	DisableRegister bool `yaml:"disable_register"`
	// LinkByEmail lets a first OAuth sign-in join the local account with
	// the same email, when that account's email is verified and the
	// provider vouches for the address and does not alias it.
	LinkByEmail bool `yaml:"link_by_email"`
	// AllowedRedirectBacks lists the absolute URLs an OAuth sign-in may
	// return to through ?redirect_back: an entry ending in / allows its
	// subtree, any other entry that page. Relative paths are always allowed.
	AllowedRedirectBacks []string `yaml:"allowed_redirect_backs"`
	// OAuthCallbackFrontendURL is the front-end page an OAuth callback
	// sends the browser to with a one-time ?code, which the page trades
	// for a token at {prefix}/exchange. Required with providers.
	OAuthCallbackFrontendURL string `yaml:"oauth_callback_frontend_url"`
	// Providers configures the OAuth providers by name; the code decides
	// which ones run: identity.Providers(google.New(cfg.Providers["google"])).
	Providers map[string]ProviderConfig `yaml:"providers"`
	// Audit configures the audit log.
	Audit AuditConfig `yaml:"audit"`
}

// ProviderConfig is one OAuth provider's section, auth.providers.<name>.
// A provider uses the fields its package documents and refuses the rest.
type ProviderConfig struct {
	// ClientID is the OAuth client: Google's client ID, GitHub's and
	// Facebook's app ID, Apple's Services ID.
	ClientID string `yaml:"client_id"`
	// ClientSecret is the client's secret; for Apple, the PEM contents of
	// the .p8 signing key.
	ClientSecret string `yaml:"client_secret" secret:"true"`
	// RedirectURL is the callback registered with the provider:
	// https://api.example.com/auth/google/callback.
	RedirectURL string `yaml:"redirect_url"`
	// Scopes replaces the provider's default scopes.
	Scopes []string `yaml:"scopes"`
	// Endpoint replaces the provider's base URL: a GitHub Enterprise
	// server, a Graph API version for Facebook, the issuer for Google and
	// Apple (a test server, mostly).
	Endpoint string `yaml:"endpoint"`
	// HostedDomain accepts only accounts of one Google Workspace domain.
	HostedDomain string `yaml:"hosted_domain"`
	// TeamID and KeyID identify Apple's signing key: the developer team
	// and the key, ten characters each.
	TeamID string `yaml:"team_id"`
	KeyID  string `yaml:"key_id"`
}

// AuditConfig is auth.audit: the audit log of who did what, written in
// the background and kept for Retention.
type AuditConfig struct {
	// Enabled turns the audit log on; off, Log does nothing.
	Enabled bool `yaml:"enabled"`
	// Retention is how long a record is kept; older ones are purged every
	// hour. 0 keeps records forever.
	Retention time.Duration `yaml:"retention" default:"4320h"`
	// AsyncBuffer is how many records Log queues for the writer.
	AsyncBuffer int `yaml:"async_buffer" default:"1024"`
	// DropOnFull drops a record when the queue is full, instead of making
	// Log wait for room (or for the caller's context to end).
	DropOnFull bool `yaml:"drop_on_full"`
	// AdminAPI mounts GET /audit/logs for administrators: the principal
	// needs the admin role, or the audit/read permission once
	// identity/rbac is in use.
	AdminAPI bool `yaml:"admin_api" default:"true"`
}

// The defaults New applies to the zero fields of a Config built in code.
const (
	defaultPrefix      = "/auth"
	defaultTokenTTL    = 2 * time.Hour
	defaultRefreshTTL  = 7 * 24 * time.Hour
	defaultResetTTL    = 15 * time.Minute
	defaultAuditBuffer = 1024
)

// withDefaults fills what a Config built in code leaves at zero; one from
// chok.Load already carries every default.
func (c Config) withDefaults() Config {
	if c.Prefix == "" {
		c.Prefix = defaultPrefix
	}
	if c.TokenTTL == 0 {
		c.TokenTTL = defaultTokenTTL
	}
	if c.RefreshTTL == 0 {
		c.RefreshTTL = defaultRefreshTTL
	}
	if c.ResetTTL == 0 {
		c.ResetTTL = defaultResetTTL
	}
	if c.Audit.AsyncBuffer == 0 {
		c.Audit.AsyncBuffer = defaultAuditBuffer
	}
	return c
}

var (
	prefixShape   = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)
	providerShape = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// Validate checks the key and the combinations that cannot work. It runs
// when chok.Load reads the configuration and again in New.
func (c *Config) Validate() error {
	var errs []error
	if len(c.SigningKey) < 32 {
		errs = append(errs, fmt.Errorf("signing_key must be at least 32 bytes, got %d: set it from the environment", len(c.SigningKey)))
	}
	if c.Prefix != "" && !prefixShape.MatchString(c.Prefix) {
		errs = append(errs, fmt.Errorf("prefix %q must be a path such as /auth, without a trailing slash", c.Prefix))
	}
	for name, d := range map[string]time.Duration{
		"token_ttl": c.TokenTTL, "refresh_ttl": c.RefreshTTL, "reset_ttl": c.ResetTTL, "login_rate_window": c.LoginRateWindow,
	} {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s must not be negative", name))
		}
	}
	if c.LoginRateLimit < 0 {
		errs = append(errs, errors.New("login_rate_limit must not be negative"))
	}
	if (c.LoginRateWindow > 0) != (c.LoginRateLimit > 0) {
		errs = append(errs, errors.New("login_rate_window and login_rate_limit are both set or both 0"))
	}
	for _, raw := range c.AllowedRedirectBacks {
		if _, err := parseAllowedRedirect(raw); err != nil {
			errs = append(errs, fmt.Errorf("allowed_redirect_backs %q: %w", raw, err))
		}
	}
	if c.ResetURL != "" && !absoluteURL(c.ResetURL) {
		errs = append(errs, errors.New("reset_url must be an absolute http or https URL"))
	}
	if c.OAuthCallbackFrontendURL != "" {
		if u, err := url.Parse(c.OAuthCallbackFrontendURL); err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, errors.New("oauth_callback_frontend_url must be an absolute URL"))
		}
	} else if len(c.Providers) > 0 {
		errs = append(errs, errors.New("oauth_callback_frontend_url is required with providers"))
	}
	for name := range c.Providers {
		if !providerShape.MatchString(name) {
			errs = append(errs, fmt.Errorf("providers: %q must be a lower-case name such as google", name))
		}
	}
	if c.Audit.Retention < 0 {
		errs = append(errs, errors.New("audit.retention must not be negative"))
	}
	if c.Audit.AsyncBuffer < 0 {
		errs = append(errs, errors.New("audit.async_buffer must not be negative"))
	}
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	slices.Sort(msgs)
	return errors.New(strings.Join(msgs, "; "))
}

// absoluteURL reports an http or https URL with a host: a link that works
// from a mail.
func absoluteURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
