// Package account provides a ready-to-use user module with registration,
// login, token refresh, and password management.
package account

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/auth"
	"github.com/zynthara/chok/v2/auth/jwt"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/middleware"
	"github.com/zynthara/chok/v2/store"
)

// Sender delivers a password-reset code to the user (email, SMS, etc.).
// The module does not impose a delivery mechanism — callers provide their own.
type Sender interface {
	Send(ctx context.Context, to string, code string) error
}

// Service manages user accounts.
//
// Two User stores are intentionally maintained:
//
//   - userStore — Module-private, UpdateFields includes every column on the
//     User table (password_hash, password_version, roles, active, etc.).
//     Used by changePassword / resetPassword / UpdateUserRoles / SetUserActive
//     / BumpPasswordVersion / MarkEmailVerified — paths the framework
//     guarantees include the PV-bump invariant where required.
//
//   - publicStore — exposed via Module.Store(), UpdateFields restricted to
//     "name" and "email". Sensitive columns are removed from the whitelist
//     so that callers attempting m.Store().Update(..., Set({"roles": ...}))
//     receive store.ErrUnknownUpdateField at request time, instead of being
//     able to silently bypass the PV-bump contract. Roles / disable / password
//     changes MUST flow through the corresponding Module methods.
//
//     The protection is store-API-level only — Module.Store().DB() returns
//     the underlying *gorm.DB as a documented escape hatch, so a caller
//     willing to bypass the store layer can still write any column. This
//     matches CLAUDE.md's "DON'T bypass the store with raw *gorm.DB" rule:
//     the framework rejects accidental writes through the public Store API,
//     but does not (and cannot) prevent an engineer who deliberately
//     reaches for raw gorm. Treat publicStore as a guardrail, not a wall.
type Service struct {
	jwt             *jwt.Manager
	resetJWT        *jwt.Manager // short-lived tokens for password reset
	h               *db.DB       // v2 thin handle wrapping the gdb New received; tx root for OAuth flows
	userStore       *store.Store[User]
	publicStore     *store.Store[User]
	idStore         *store.Store[Identity] // OAuth identities; populated regardless of provider count
	sender          Sender                 // nil → forgot/reset-password routes are not registered
	logger          log.Logger
	limiter         *loginLimiter // nil when rate limiting is disabled
	disableRegister bool          // true → RegisterRoutes skips POST /register

	// OAuth wiring (Phase 2). Lazily assembled on first RegisterProvider —
	// pure password deployments never spin up the carrier/store goroutines.
	oauthMu                  sync.Mutex
	signingKey               string // retained for HKDF cookie-secret derivation
	providers                map[string]AuthProvider
	sessionCarrier           SessionCarrier
	sessionStore             OAuthSessionStore
	authCodeStore            AuthCodeStore
	cookieDevMode            bool              // mirrored from CookieCarrier so exchange-binding cookie picks the same Secure/SameSite
	allowedRedirects         []allowedRedirect // SPEC §6.1 parsed absolute-URL allowlist (boundary-strict)
	oauthCallbackFrontendURL string            // SPEC §7 fixed front-end landing URL
	linkByEmail              bool              // SPEC §8 default false
	firstRedirectURL         string            // first provider's RedirectURL — informs dev-mode auto-detect
}

// Option configures a Module.
type Option func(*moduleConfig)

type moduleConfig struct {
	signingKey      string
	expiration      time.Duration
	resetExpiration time.Duration
	sender          Sender
	loginRateWindow time.Duration // 0 = disabled
	loginRateLimit  int           // max attempts per window
	disableRegister bool

	// OAuth (Phase 2). Carrier / Store / AuthCodeStore default to nil so
	// New() does not spawn background resources for pure-password
	// deployments — they are created on first RegisterProvider unless
	// the caller injected explicit instances via the WithXxx options.
	sessionCarrier           SessionCarrier
	sessionStore             OAuthSessionStore
	authCodeStore            AuthCodeStore
	allowedRedirectBacks     []string
	oauthCallbackFrontendURL string
	linkByEmail              bool
}

// WithSigningKey sets the JWT signing key (required, >= 32 bytes).
func WithSigningKey(key string) Option {
	return func(c *moduleConfig) { c.signingKey = key }
}

// WithExpiration sets the access token lifetime. Defaults to 2 hours.
func WithExpiration(d time.Duration) Option {
	return func(c *moduleConfig) { c.expiration = d }
}

// WithResetExpiration sets the password-reset token lifetime. Defaults to 15 minutes.
func WithResetExpiration(d time.Duration) Option {
	return func(c *moduleConfig) { c.resetExpiration = d }
}

// WithSender enables the forgot/reset-password flow.
func WithSender(s Sender) Option {
	return func(c *moduleConfig) { c.sender = s }
}

// WithLoginRateLimit enables per-email login attempt rate limiting.
// When the threshold is exceeded within the window, the login endpoint
// returns 429 Too Many Requests. Defaults to disabled (zero values).
//
// Recommended production values: window=15m, maxAttempts=10.
func WithLoginRateLimit(window time.Duration, maxAttempts int) Option {
	return func(c *moduleConfig) {
		c.loginRateWindow = window
		c.loginRateLimit = maxAttempts
	}
}

// WithoutPublicRegister disables the POST /register endpoint. Login and
// authenticated endpoints still work; only the anonymous self-register
// route is skipped. Use this in deployments where admins provision
// accounts (via account.Module.Store().Create) instead of letting
// visitors register themselves.
func WithoutPublicRegister() Option {
	return func(c *moduleConfig) { c.disableRegister = true }
}

// WithSessionCarrier overrides the default cookie-based SessionCarrier
// (HMAC-signed cookie, secret HKDF-derived from SigningKey). Use this
// when you need a query-string carrier (legacy SPAs that strip cookies)
// or a custom signing scheme.
//
// Passing nil panics at Module.New — Carrier is mandatory once OAuth is
// in play.
func WithSessionCarrier(c SessionCarrier) Option {
	return func(m *moduleConfig) { m.sessionCarrier = c }
}

// WithOAuthSessionStore overrides the default in-process LRU
// MemorySessionStore. Production deployments serving multiple replicas
// MUST inject a shared backend (Redis is the canonical choice) — Memory
// only works when /auth/start and /auth/callback land on the same
// process.
func WithOAuthSessionStore(s OAuthSessionStore) Option {
	return func(m *moduleConfig) { m.sessionStore = s }
}

// WithAuthCodeStore overrides the default AuthCodeStore (which shares
// backing storage with the default OAuthSessionStore via prefixed keys).
// Independent override is supported so deployments can keep auth-code
// state local while pushing OAuth session state to Redis, or vice versa.
func WithAuthCodeStore(s AuthCodeStore) Option {
	return func(m *moduleConfig) { m.authCodeStore = s }
}

// WithAllowedRedirectBacks declares a set of absolute URL prefixes that
// /auth/{name}/start will accept on its ?redirect_back parameter. The
// default empty list permits relative paths only; supply this option to
// support multi-front-end deployments where the SPA lives on a different
// host than the chok back-end.
//
// Each entry must include scheme + host (+ port if non-default). Trailing
// "/" widens to an entire site; an exact URL narrows to one landing page.
// HTTP entries are accepted but emit a startup WARN — production
// deployments should be HTTPS-only.
func WithAllowedRedirectBacks(urls ...string) Option {
	return func(m *moduleConfig) {
		m.allowedRedirectBacks = append(m.allowedRedirectBacks, urls...)
	}
}

// WithOAuthCallbackFrontendURL sets the fixed front-end landing URL the
// OAuth callback flow redirects to with the one-shot ?code parameter.
// Required for any deployment that registers OAuth providers — Module
// returns an error from RegisterProvider if this is empty when the
// provider count is about to become non-zero.
//
// Typical value: "https://app.example.com/auth/oauth-finish".
func WithOAuthCallbackFrontendURL(u string) Option {
	return func(m *moduleConfig) { m.oauthCallbackFrontendURL = u }
}

// WithLinkByEmail enables the SPEC §8 auto-merge path: when an OAuth
// callback arrives for a brand-new (provider, provider_account_id) and
// the IdP-supplied email matches an existing local user, the new
// Identity is attached to that user. Defaults to false because automerge
// only works safely once the local /register flow includes an email
// verification step (which chok does not bundle today). Even with this
// option enabled, ResolveOAuthIdentity still requires
// IdP-side EmailVerified=true and !IsAliasedEmail before merging.
func WithLinkByEmail(enabled bool) Option {
	return func(m *moduleConfig) { m.linkByEmail = enabled }
}

// New creates an account module against the v2 thin database handle
// (kernel-less embedding and tests; the kernel path is account.Module).
func New(h *db.DB, logger log.Logger, opts ...Option) (*Service, error) {
	cfg := &moduleConfig{
		expiration:      2 * time.Hour,
		resetExpiration: 15 * time.Minute,
	}
	for _, o := range opts {
		o(cfg)
	}

	jwtMgr, err := jwt.NewManager(jwt.Options{
		SigningKey: cfg.signingKey,
		Issuer:     "access",
		Expiration: cfg.expiration,
	})
	if err != nil {
		return nil, err
	}

	resetMgr, err := jwt.NewManager(jwt.Options{
		SigningKey: cfg.signingKey,
		Issuer:     "reset",
		Expiration: cfg.resetExpiration,
	})
	if err != nil {
		return nil, err
	}

	// userStore is Module-private: full UpdateFields whitelist so internal
	// flows (changePassword, resetPassword, UpdateUserRoles, ...) can write
	// every column. Sensitive writes are gated by Module methods, not by the
	// store layer.
	userStore := store.New[User](h, logger,
		store.WithQueryFields("id", "email", "name", "email_verified", "created_at"),
		store.WithUpdateFields("name", "email", "email_verified", "password_hash", "has_password", "password_version", "roles", "active"),
	)
	// publicStore is what Module.Store() exposes. UpdateFields drops
	// password_hash / password_version / roles / active / email_verified —
	// callers attempting to write them through the store API get
	// store.ErrUnknownUpdateField. This is store-API-level enforcement;
	// raw gorm via Module.Store().DB() remains an escape hatch (see
	// Module struct doc).
	publicStore := store.New[User](h, logger,
		store.WithQueryFields("id", "email", "name", "email_verified", "created_at"),
		store.WithUpdateFields("name", "email"),
	)
	// idStore is always created — Identity rows are written only by OAuth
	// flows, but having the store ready means switching deployments from
	// password-only to OAuth at runtime needs no Module surgery. Schema
	// is created by parts.AccountComponent.Migrate (Phase 2 update).
	idStore := store.New[Identity](h, logger,
		store.WithQueryFields("id", "user_id", "provider", "provider_account_id", "email", "last_used_at"),
		store.WithUpdateFields("email", "profile", "last_used_at"),
	)

	// Parse and validate the redirect_back allowlist once at startup so a
	// malformed entry surfaces as a fail-fast error rather than letting a
	// silent fall-through allow nothing (or worse, accidentally permit an
	// open redirect through some other code path).
	parsedAllow := make([]allowedRedirect, 0, len(cfg.allowedRedirectBacks))
	for _, raw := range cfg.allowedRedirectBacks {
		entry, err := parseAllowedRedirect(raw)
		if err != nil {
			return nil, fmt.Errorf("account: WithAllowedRedirectBacks %q: %w", raw, err)
		}
		if entry.scheme == "http" && logger != nil {
			logger.Warn("account: redirect_back allowlist entry uses http (production should be https)", "url", raw)
		}
		parsedAllow = append(parsedAllow, entry)
	}

	m := &Service{
		h:                        h,
		jwt:                      jwtMgr,
		resetJWT:                 resetMgr,
		userStore:                userStore,
		publicStore:              publicStore,
		idStore:                  idStore,
		sender:                   cfg.sender,
		logger:                   logger,
		disableRegister:          cfg.disableRegister,
		signingKey:               cfg.signingKey,
		providers:                map[string]AuthProvider{},
		sessionCarrier:           cfg.sessionCarrier,
		sessionStore:             cfg.sessionStore,
		authCodeStore:            cfg.authCodeStore,
		allowedRedirects:         parsedAllow,
		oauthCallbackFrontendURL: cfg.oauthCallbackFrontendURL,
		linkByEmail:              cfg.linkByEmail,
	}
	if cfg.loginRateWindow > 0 && cfg.loginRateLimit > 0 {
		m.limiter = newLoginLimiter(cfg.loginRateWindow, cfg.loginRateLimit)
	}
	return m, nil
}

// Close releases the module's background resources. The login rate
// limiter spawns short-lived cleanup goroutines on every Nth failure;
// Close waits for any in-flight cleanup so the App's shutdown budget
// covers them rather than leaving the goroutine running until the
// process terminates. Safe to call multiple times.
//
// OAuth resources (sessionCarrier, sessionStore, authCodeStore) are
// optional — they are only assembled lazily on the first
// RegisterProvider call. Close walks them with an io.Closer type
// assertion so stateless implementations (e.g. CookieCarrier, the
// default authCodeStore-as-prefix-bucket adapter) don't pay a no-op
// Close cost. errors.Join surfaces all close failures without short-
// circuiting, matching the SPEC §12 contract.
func (m *Service) Close() error {
	if m == nil {
		return nil
	}
	if m.limiter != nil {
		m.limiter.Close()
	}
	var errs []error
	if c, ok := m.sessionCarrier.(io.Closer); ok {
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("sessionCarrier: %w", err))
		}
	}
	if s, ok := m.sessionStore.(io.Closer); ok {
		if err := s.Close(); err != nil {
			errs = append(errs, fmt.Errorf("sessionStore: %w", err))
		}
	}
	if a, ok := m.authCodeStore.(io.Closer); ok {
		// MemorySessionStore.Close is sync.Once-guarded, so when both
		// stores share a backing *MemorySessionStore the second Close
		// is a no-op return nil — no need for an explicit alias check.
		if err := a.Close(); err != nil {
			errs = append(errs, fmt.Errorf("authCodeStore: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Store returns the User store for callers that need read-side user
// management (list, get) plus a restricted set of writes (name, email).
//
// Sensitive columns — password_hash, password_version, roles, active,
// email_verified — are intentionally NOT in the exposed store's update
// whitelist. Attempting to write them through this handle returns
// store.ErrUnknownUpdateField. The PV-bump invariant (every roles or
// active change must invalidate existing tokens) is therefore not
// bypassable through the store API; callers MUST use:
//
//   - Module.UpdateUserRoles      to change Roles (atomic UPDATE, bumps PV)
//   - Module.SetUserActive        to enable/disable an account (atomic UPDATE, bumps PV)
//   - Module.BumpPasswordVersion  to force-revoke all tokens (atomic UPDATE)
//   - Module.MarkEmailVerified    to flip EmailVerified (no PV bump)
//   - the /change-password and /reset-password routes for password writes
//
// Intended consumers: application admin handlers (user listing, profile
// edits) and any read-side aggregation. Casual access from request
// handlers should still go through Module's auth flows.
//
// Caveat: the returned *store.Store exposes DB() / ScopedDB() as
// documented escape hatches. A caller that reaches for raw gorm bypasses
// every restriction listed above. This is intentional — the framework
// trusts engineers to honour CLAUDE.md's "DON'T bypass the store with
// raw *gorm.DB" rule for sensitive writes.
func (m *Service) Store() *store.Store[User] { return m.publicStore }

// LoginRateLimitEnabled reports whether per-email login throttling is
// active. Useful for diagnostics, /healthz reporting, and tests that
// need to confirm a builder propagated the LoginRateWindow /
// LoginRateLimit configuration without poking at internal state.
func (m *Service) LoginRateLimitEnabled() bool {
	return m != nil && m.limiter != nil
}

// TokenParser returns the JWT manager for use with middleware.Authn.
func (m *Service) TokenParser() middleware.TokenParser {
	return m.jwt
}

// PrincipalResolver returns a resolver that builds a Principal from JWT claims.
// Roles are read from the "roles" claim in the token — no DB lookup required.
func (m *Service) PrincipalResolver() middleware.PrincipalResolver {
	return func(_ context.Context, subject string, claims map[string]any) (auth.Principal, error) {
		p := auth.Principal{Subject: subject, Claims: claims}
		if name, ok := claims["name"].(string); ok {
			p.Name = name
		}
		if roles, ok := claims["roles"].([]any); ok {
			for _, r := range roles {
				if s, ok := r.(string); ok {
					p.Roles = append(p.Roles, s)
				}
			}
		}
		return p, nil
	}
}

// Authn is the blessed authentication middleware for protected
// business routes: token verification (middleware.Authn) composed
// with ActiveCheck. Mount it on a group:
//
//	api := r.Group("/api/v1", acct.Authn())
//
// **Use Authn, not bare middleware.Authn.** Bare token verification
// only validates the JWT signature and populates auth.Principal — it
// does not touch the database. The framework's "PV bump invalidates
// outstanding tokens" guarantee (UpdateUserRoles / SetUserActive /
// BumpPasswordVersion) requires ActiveCheck, which queries the user
// row on every request and rejects disabled accounts or stale token
// versions. A route that mounts only token verification will continue
// serving disabled / role-revoked users until their token naturally
// expires, breaking the revocation contract.
func (m *Service) Authn() kernel.Middleware {
	authn := middleware.Authn(m.TokenParser(), m.PrincipalResolver())
	active := m.ActiveCheck()
	return func(next http.Handler) http.Handler {
		return authn(active(next))
	}
}

// ActiveCheck returns a middleware that verifies the authenticated user
// still exists, is active, and the JWT's password version matches the
// current one in the DB. It hits the database on every request.
//
// PrincipalResolver is stateless by design (no DB per request). Apply
// ActiveCheck to routes where real-time revocation matters — or just
// mount Authn(), which composes both.
//
// The pv check rejects tokens whose roles or password are stale: any
// admin operation that wants to invalidate existing tokens (disable,
// role change, password reset) bumps PasswordVersion in the DB, and the
// next request from that token's holder will be 401'd here.
func (m *Service) ActiveCheck() kernel.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.PrincipalFrom(r.Context())
			if !ok {
				handler.WriteResponse(w, r, 0, nil, apierr.ErrUnauthenticated)
				return
			}
			user, err := m.userStore.Get(r.Context(), store.RID(p.Subject))
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					handler.WriteResponse(w, r, 0, nil, apierr.ErrUnauthenticated.WithMessage("account not found"))
				} else {
					handler.WriteResponse(w, r, 0, nil, apierr.ErrInternal)
				}
				return
			}
			if !user.Active {
				handler.WriteResponse(w, r, 0, nil, apierr.ErrUnauthenticated.WithMessage("account is disabled"))
				return
			}
			// pv mismatch ⇒ 用户的 roles / password 在 token 签发后被改过，
			// 老 token 不应再被接受（让 client 重新登录拿新 claims）。
			// JSON unmarshal 把数字转成 float64，所以这里类型断言走 float64。
			if pv, ok := p.Claims["pv"].(float64); !ok || int(pv) != user.PasswordVersion {
				handler.WriteResponse(w, r, 0, nil, apierr.ErrUnauthenticated.WithMessage("token invalidated, please re-login"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RegisterRoutes registers account API routes on the given router —
// typically the "/auth" group the account module mounts (the paths
// below are relative; group prefix + relative path yields the SPEC §7
// absolute URLs, so hardcoding "/auth/" here would double-prefix).
//
// Public routes (no auth required):
//
//	POST /register          (skipped when WithoutPublicRegister is set)
//	POST /login
//	POST /forgot-password   (only if WithSender is configured)
//	POST /reset-password    (only if WithSender is configured)
//	GET  /{name}/start      (one entry per registered OAuth provider)
//	GET|POST /{name}/callback
//	POST /exchange          (only if any OAuth provider is registered)
//
// Authenticated routes (Authn = token verification + ActiveCheck, so
// the PV-bump revocation invariant is enforced on the module's own
// routes too — without ActiveCheck, an admin's BumpPasswordVersion /
// UpdateUserRoles call would not invalidate the user's ability to hit
// /change-password until the token's natural expiry):
//
//	POST   /refresh-token
//	PUT    /change-password
//	GET    /identities           (only if any OAuth provider is registered)
//	POST   /identities/link
//	DELETE /identities/{id}
func (m *Service) RegisterRoutes(r kernel.Router) {
	if !m.disableRegister {
		r.Handle(http.MethodPost, "/register", handler.HandleRequest(m.register, handler.WithSuccessCode(201)))
	}
	r.Handle(http.MethodPost, "/login", handler.HandleRequest(m.login))

	if m.sender != nil {
		r.Handle(http.MethodPost, "/forgot-password", handler.HandleAction(m.forgotPassword))
		r.Handle(http.MethodPost, "/reset-password", handler.HandleAction(m.resetPassword))
	}

	// OAuth public routes — only mounted when any provider was
	// registered. The provider count is captured under oauthMu to avoid
	// racing with a concurrent RegisterProvider; the routes themselves
	// are read-only references to m so post-registration provider
	// changes are visible immediately.
	m.oauthMu.Lock()
	providerNames := make([]string, 0, len(m.providers))
	for name := range m.providers {
		providerNames = append(providerNames, name)
	}
	hasOAuth := len(providerNames) > 0
	m.oauthMu.Unlock()

	for _, name := range providerNames {
		p := m.providers[name]
		r.Handle(http.MethodGet, "/"+name+"/start", m.handleBegin(p))
		switch providerHTTPMethodFor(p) {
		case "POST":
			r.Handle(http.MethodPost, "/"+name+"/callback", m.handleCallback(p))
		default:
			r.Handle(http.MethodGet, "/"+name+"/callback", m.handleCallback(p))
		}
	}
	if hasOAuth {
		r.Handle(http.MethodPost, "/exchange", http.HandlerFunc(m.handleExchange))
	}

	authed := r.Group("", m.Authn())
	authed.Handle(http.MethodPost, "/refresh-token", handler.HandleRequest(m.refreshToken))
	authed.Handle(http.MethodPut, "/change-password", handler.HandleAction(m.changePassword))
	if hasOAuth {
		authed.Handle(http.MethodGet, "/identities", handler.HandleRequest(m.handleListIdentities))
		// /identities/link is a raw handler because the link flow needs
		// to write the same SessionCarrier cookie that /{name}/start does —
		// HandleRequest's signature has no writer for cookie writes.
		authed.Handle(http.MethodPost, "/identities/link", http.HandlerFunc(m.handleLinkIdentity))
		authed.Handle(http.MethodDelete, "/identities/{id}", handler.HandleAction(m.handleUnlinkIdentity))
	}
}
