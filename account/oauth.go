package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/auth"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/store"
	"github.com/zynthara/chok/v2/store/where"
)

// LoginMethod is one row of "how can this user authenticate" — returned
// by ListLoginMethods. The password slot is virtual (no Identity row);
// OAuth methods carry the underlying Identity RID so the front-end can
// invoke Unlink with a stable id.
type LoginMethod struct {
	// Type is "password" or a provider name ("google", "github", ...).
	Type string `json:"type"`

	// IdentityID is the Identity.RID for OAuth methods, empty for "password".
	IdentityID string `json:"identity_id,omitempty"`

	// Email is the address associated with the method. For "password"
	// it's User.Email; for OAuth it's Identity.Email (which can differ
	// when LinkByEmail is off and a user binds an alternate-email IdP).
	Email string `json:"email,omitempty"`
}

// RegisterProvider attaches an AuthProvider to the Module. The first
// successful call lazily assembles the SessionCarrier / OAuthSessionStore
// / AuthCodeStore (whichever weren't supplied via WithXxx options) — so
// pure-password deployments never spawn the cookie signer or the LRU
// cleanup goroutine.
//
// Re-registering the same provider name returns an error to surface
// duplicate-registration bugs at startup.
func (m *Module) RegisterProvider(p AuthProvider) error {
	if p == nil {
		return errors.New("account: RegisterProvider received nil provider")
	}
	name := p.Name()
	if name == "" {
		return errors.New("account: provider Name() must not be empty")
	}

	m.oauthMu.Lock()
	defer m.oauthMu.Unlock()

	if _, exists := m.providers[name]; exists {
		return fmt.Errorf("account: provider %q already registered", name)
	}

	if len(m.providers) == 0 {
		// First provider attaches OAuth plumbing.
		if m.oauthCallbackFrontendURL == "" {
			return errors.New("account: WithOAuthCallbackFrontendURL is required when any OAuth provider is registered")
		}
		// Pull the RedirectURL hint from the first provider (if it
		// implements RedirectURLProvider) so ensureOAuthSessionPlumbing
		// can flip dev mode for HTTP-on-localhost deployments. Without
		// this hint the dev-mode auto-detect promised in SPEC §5.1
		// v0.3.5 is dead code.
		if rp, ok := p.(RedirectURLProvider); ok {
			m.firstRedirectURL = rp.RedirectURL()
		}
		if err := m.ensureOAuthSessionPlumbing(); err != nil {
			return err
		}
	}

	m.providers[name] = p
	return nil
}

// Provider returns the registered provider by name.
func (m *Module) Provider(name string) (AuthProvider, bool) {
	m.oauthMu.Lock()
	defer m.oauthMu.Unlock()
	p, ok := m.providers[name]
	return p, ok
}

// ProviderNames returns the registered provider names (sorted by
// insertion is not guaranteed — callers that need determinism should
// sort the result themselves).
func (m *Module) ProviderNames() []string {
	m.oauthMu.Lock()
	defer m.oauthMu.Unlock()
	out := make([]string, 0, len(m.providers))
	for name := range m.providers {
		out = append(out, name)
	}
	return out
}

// ensureOAuthSessionPlumbing populates sessionCarrier / sessionStore /
// authCodeStore on the Module if they were not explicitly injected via
// WithXxx. Caller must hold m.oauthMu.
func (m *Module) ensureOAuthSessionPlumbing() error {
	// cookieDevMode must reflect the actual deployment posture so the
	// /auth/exchange browser-binding cookie picks Secure / SameSite to
	// match the sid cookie. The decision flow:
	//   1. Caller-supplied *CookieCarrier — read its devMode field
	//      (same package, direct access OK). Caller's WithDevMode()
	//      choice wins.
	//   2. Caller-supplied non-cookie carrier (header/query) — fall
	//      back to firstRedirectURL HTTP-localhost detection. We have
	//      no explicit signal, so the URL hint is the best guess.
	//   3. No carrier supplied — same URL hint, then build the default
	//      CookieCarrier with WithDevMode opt if applicable.
	switch cc := m.sessionCarrier.(type) {
	case *CookieCarrier:
		m.cookieDevMode = cc.cfg.devMode
	default:
		_ = cc
		m.cookieDevMode = isLocalDevRedirect(m.firstRedirectURL)
	}

	if m.sessionCarrier == nil {
		secret, err := deriveOAuthSessionSecret(m.signingKey)
		if err != nil {
			return fmt.Errorf("account: derive cookie secret: %w", err)
		}
		opts := []CookieOption{}
		if m.cookieDevMode {
			opts = append(opts, WithDevMode())
			if m.logger != nil {
				m.logger.Info("oauth cookie carrier in dev mode (HTTP localhost detected)")
			}
		}
		m.sessionCarrier = NewCookieCarrier(secret, "_chok_oauth_sid", opts...)
	}

	// Default sessionStore + authCodeStore share the same MemorySessionStore
	// so single-instance deployments only carry one LRU map. Custom Carrier
	// or Store overrides via WithXxx are honoured independently.
	if m.sessionStore == nil && m.authCodeStore == nil {
		mem := NewMemorySessionStore()
		m.sessionStore = mem
		m.authCodeStore = NewMemoryAuthCodeStore(mem)
	} else if m.sessionStore == nil {
		m.sessionStore = NewMemorySessionStore()
	} else if m.authCodeStore == nil {
		// AuthCode-only memory backing. Different instance from sessionStore
		// because the caller explicitly provided their own sessionStore;
		// keep namespaces independent.
		mem := NewMemorySessionStore()
		m.authCodeStore = NewMemoryAuthCodeStore(mem)
	}

	return nil
}

// deriveOAuthSessionSecret derives a 32-byte HMAC secret from the JWT
// SigningKey via HKDF-SHA256 with an info tag pinning the use case.
// The tag (`chok/oauth-session/v1`) cryptographically isolates the
// derived key from anything else SigningKey is used for, so a leaked
// cookie secret cannot be inverted into the JWT signing key.
func deriveOAuthSessionSecret(jwtSigningKey string) ([]byte, error) {
	if len(jwtSigningKey) < 32 {
		return nil, errors.New("signing key must be >= 32 bytes")
	}
	h := hkdf.New(sha256.New, []byte(jwtSigningKey), nil, []byte("chok/oauth-session/v1"))
	secret := make([]byte, 32)
	if _, err := io.ReadFull(h, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// isLocalDevRedirect inspects a provider RedirectURL and returns true if
// the URL is HTTP-on-localhost. Used to auto-enable WithDevMode on the
// default CookieCarrier.
func isLocalDevRedirect(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// ResolveOAuthIdentity is the canonical OAuth-callback decision tree:
// look up an existing (provider, provider_account_id), else create or
// link based on email policy, else error per SPEC §8.1. See the SPEC
// for the full state machine; comments here cover the implementation
// nuances.
func (m *Module) ResolveOAuthIdentity(ctx context.Context, pi *ProviderIdentity) (*User, *Identity, error) {
	if pi == nil {
		return nil, nil, errors.New("account: ResolveOAuthIdentity received nil ProviderIdentity")
	}
	if pi.Provider == "" || pi.ProviderAccountID == "" {
		return nil, nil, errors.New("account: ProviderIdentity missing Provider or ProviderAccountID")
	}

	// Normalize email so OAuth lookups / writes share the same casing rule
	// as /login, /register, /forgot-password (account/handler.go normalizeEmail).
	// Without this, an IdP returning "Alice@idp.test" would never match a
	// local "alice@idp.test" user — squatting protection in §8 LinkByEmail
	// would silently fall through, and OAuth-only-bootstrapped accounts
	// could not later receive a /forgot-password link.
	pi.Email = normalizeEmail(pi.Email)

	// 1. Existing identity → just load the user.
	existing, err := m.idStore.Get(ctx, store.Where(
		where.WithFilter("provider", pi.Provider),
		where.WithFilter("provider_account_id", pi.ProviderAccountID),
	))
	if err == nil {
		user, err := m.userStore.Get(ctx, store.RID(existing.UserID))
		if err != nil {
			return nil, nil, err
		}
		if !user.Active {
			return nil, nil, apierr.ErrUnauthenticated.WithMessage("account is disabled")
		}
		// Best-effort touch of LastUsedAt; failures shouldn't block login.
		go m.touchIdentity(context.WithoutCancel(ctx), existing.RID)
		return user, existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, nil, err
	}

	// 2. New identity. SPEC §8.1: email must be non-empty + verified +
	// not aliased before we create a User. Returning a structured 400
	// lets the front-end direct the user toward password registration.
	if pi.Email == "" || !pi.EmailVerified || pi.IsAliasedEmail {
		return nil, nil, apierr.ErrInvalidArgument.
			WithReason("OAUTH_EMAIL_REQUIRED").
			WithMessage("无法仅通过 OAuth 创建账号:provider 未提供已验证的真实邮箱").
			WithDetails(map[string]any{
				"provider":     pi.Provider,
				"missing":      missingFieldFor(pi),
				"next_actions": defaultNextActions(pi.Provider),
			})
	}

	// 3. Wrap link / create in a transaction so an Identity write failure
	// does not leave a dangling User row (SPEC §6.2).
	var resultUser *User
	var resultIdent *Identity
	err = db.RunInTx(ctx, m.userStore.DB(), func(txCtx context.Context) error {
		canAutoLink := m.linkByEmail && pi.EmailVerified && !pi.IsAliasedEmail
		if canAutoLink {
			user, lookupErr := m.userByEmailVerified(txCtx, pi.Email)
			if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
				return lookupErr
			}
			if user != nil {
				ident, err := m.linkIdentityTx(txCtx, user.RID, pi)
				if err != nil {
					return err
				}
				resultUser, resultIdent = user, ident
				return nil
			}
		}

		// Create a new OAuth-only User. HasPassword=false explicitly
		// flags "user has not set a password" — /login rejects this
		// account with the OAUTH_ONLY_ACCOUNT directing message until
		// /forgot-password sets a real password (which flips HasPassword
		// to true). PasswordHash is a random unguessable placeholder
		// because the schema column is NOT NULL; the value is never
		// actually compared against anything.
		randomHash, err := auth.HashPassword(randomUnguessableSecret())
		if err != nil {
			return err
		}
		newUser := &User{
			Email:           pi.Email,
			EmailVerified:   pi.EmailVerified && !pi.IsAliasedEmail,
			PasswordHash:    randomHash,
			HasPassword:     false,
			PasswordVersion: 0,
			Name:            firstNonEmpty(pi.Name, maskEmail(pi.Email)),
			Active:          true,
		}
		txDB := db.DBFromContext(txCtx)
		if err := m.userStore.WithTx(txDB).Create(txCtx, newUser); err != nil {
			if errors.Is(err, store.ErrDuplicate) {
				return apierr.ErrConflict.WithMessage(
					"该邮箱已被使用,请先用密码登录后到设置页绑定 " + pi.Provider)
			}
			return err
		}
		ident, err := m.linkIdentityTx(txCtx, newUser.RID, pi)
		if err != nil {
			return err
		}
		resultUser, resultIdent = newUser, ident
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return resultUser, resultIdent, nil
}

// LinkIdentity attaches a fresh Identity to an existing user (post-login
// flow: the user is already authenticated and is binding an extra IdP).
// Wraps the inner write in a transaction so caller doesn't need to.
func (m *Module) LinkIdentity(ctx context.Context, userID string, pi *ProviderIdentity) (*Identity, error) {
	if userID == "" {
		return nil, apierr.ErrInvalidArgument.WithMessage("userID is required")
	}
	if pi == nil || pi.Provider == "" || pi.ProviderAccountID == "" {
		return nil, apierr.ErrInvalidArgument.WithMessage("ProviderIdentity must include Provider and ProviderAccountID")
	}
	var ident *Identity
	err := db.RunInTx(ctx, m.idStore.DB(), func(txCtx context.Context) error {
		i, err := m.linkIdentityTx(txCtx, userID, pi)
		if err != nil {
			return err
		}
		ident = i
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ident, nil
}

// linkIdentityTx writes Identity inside a caller-supplied tx context.
// Translates store.ErrDuplicate into the actionable "already bound to
// another user" 409.
func (m *Module) linkIdentityTx(ctx context.Context, userID string, pi *ProviderIdentity) (*Identity, error) {
	raw, marshalErr := json.Marshal(pi.Raw)
	if marshalErr != nil && m.logger != nil {
		// pi.Raw is map[string]any so a Marshal failure means the IdP
		// payload contained an unsupported type (chan/func, cyclic
		// pointer). Log and proceed with raw=nil — the Identity row is
		// still useful without the audit blob, and the IdP-level fields
		// (Email, Provider, ProviderAccountID) all live on the typed
		// columns.
		m.logger.Warn("oauth identity profile marshal failed; persisting without raw payload",
			"provider", pi.Provider, "error", marshalErr)
		raw = nil
	}
	ident := &Identity{
		UserID:            userID,
		Provider:          pi.Provider,
		ProviderAccountID: pi.ProviderAccountID,
		Email:             pi.Email,
		Profile:           datatypes.JSON(raw),
		LastUsedAt:        time.Now(),
	}
	txDB := db.DBFromContext(ctx)
	if err := m.idStore.WithTx(txDB).Create(ctx, ident); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return nil, apierr.ErrConflict.WithMessage("该 OAuth 账号已绑定到另一个用户")
		}
		return nil, err
	}
	return ident, nil
}

// UnlinkIdentity removes an Identity row, refusing if it would leave the
// user with no login method at all (SPEC §6.2). The whole load + count +
// delete sequence runs inside a transaction with a row-level lock on
// the user record so two concurrent unlink calls cannot each see two
// methods, both pass the guard, and both delete — leaving the account
// with zero recoverable login methods.
//
// On SQLite the FOR UPDATE clause is silently dropped, but SQLite's tx
// writer serialization yields the same outcome: only one goroutine
// holds the write lock at a time, so the second one observes the
// already-decremented count. On MySQL / PG / TiDB the lock makes the
// guarantee explicit.
func (m *Module) UnlinkIdentity(ctx context.Context, userID, identityID string) error {
	if userID == "" || identityID == "" {
		return apierr.ErrInvalidArgument.WithMessage("userID and identityID are required")
	}

	return db.RunInTx(ctx, m.idStore.DB(), func(txCtx context.Context) error {
		txDB := db.DBFromContext(txCtx)

		// Row-lock the user; serializes concurrent unlinks on the same
		// account so the subsequent count+delete is an atomic critical
		// section with respect to other unlinks.
		var u User
		if err := txDB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("rid = ? AND deleted_at IS NULL", userID).
			Take(&u).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apierr.ErrNotFound.WithMessage("user not found")
			}
			return err
		}

		// Ownership + existence check inside the lock. ErrNotFound for
		// both "no such row" and "row belongs to a different user" — the
		// latter must not expose existence to a probing attacker.
		var ident Identity
		if err := txDB.Where("rid = ? AND user_id = ? AND deleted_at IS NULL", identityID, userID).
			Take(&ident).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apierr.ErrNotFound.WithMessage("identity not found")
			}
			return err
		}

		// Method count = 1 (password slot, if user has set a password)
		// + len(non-deleted identities). Compute under the same lock so
		// no concurrent unlink can race the guard. user.HasPassword is
		// the source of truth — set true at /register, flipped true by
		// /reset-password / /change-password, left false for OAuth-only
		// accounts (random unguessable PasswordHash placeholder).
		var identCount int64
		if err := txDB.Model(&Identity{}).
			Where("user_id = ? AND deleted_at IS NULL", userID).
			Count(&identCount).Error; err != nil {
			return err
		}
		methodCount := int(identCount)
		if u.HasPassword {
			methodCount++
		}
		if methodCount <= 1 {
			return apierr.ErrFailedPrecondition.WithMessage(
				"至少保留一种登录方式(密码或 OAuth)")
		}

		return m.idStore.WithTx(txDB).Delete(txCtx, store.RID(identityID))
	})
}

// ListIdentities returns every Identity bound to the given user.
func (m *Module) ListIdentities(ctx context.Context, userID string) ([]Identity, error) {
	if userID == "" {
		return nil, apierr.ErrInvalidArgument.WithMessage("userID is required")
	}
	page, err := m.idStore.List(ctx, where.WithFilter("user_id", userID))
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

// ListLoginMethods aggregates every authentication path available to a
// user: a "password" virtual entry when User.HasPassword is true, plus
// one entry per OAuth Identity row.
//
// HasPassword is the explicit signal — set true by /register and by
// changePassword/resetPassword, left false for OAuth-only-bootstrapped
// accounts (whose PasswordHash is a random unguessable placeholder).
// Earlier code derived "has password" from `PasswordVersion > 0 ||
// len(idents) == 0`; that broke as soon as a regular password user
// linked OAuth (PV stayed 0, idents=1, password slot disappeared).
func (m *Module) ListLoginMethods(ctx context.Context, userID string) ([]LoginMethod, error) {
	if userID == "" {
		return nil, apierr.ErrInvalidArgument.WithMessage("userID is required")
	}
	idents, err := m.ListIdentities(ctx, userID)
	if err != nil {
		return nil, err
	}
	user, err := m.userStore.Get(ctx, store.RID(userID))
	if err != nil {
		return nil, err
	}

	out := make([]LoginMethod, 0, 1+len(idents))
	if user.HasPassword {
		out = append(out, LoginMethod{Type: "password", Email: user.Email})
	}
	for i := range idents {
		out = append(out, LoginMethod{
			Type:       idents[i].Provider,
			IdentityID: idents[i].RID,
			Email:      idents[i].Email,
		})
	}
	return out, nil
}

// userHasPasswordHistory reports whether /login should accept a password
// for this user. Returns user.HasPassword directly — the field tracks
// "user has set a password" intent explicitly, replacing the earlier
// `PV>0 || len(idents)==0` heuristic that was wrong for password users
// who later linked OAuth.
func (m *Module) userHasPasswordHistory(ctx context.Context, userID string) (bool, error) {
	user, err := m.userStore.Get(ctx, store.RID(userID))
	if err != nil {
		return false, err
	}
	return user.HasPassword, nil
}

// userByEmailVerified looks up a verified-email User by address. Used
// by ResolveOAuthIdentity's auto-link path. Soft-deleted rows excluded.
func (m *Module) userByEmailVerified(ctx context.Context, email string) (*User, error) {
	user, err := m.userStore.Get(ctx, store.Where(where.WithFilter("email", email)))
	if err != nil {
		return nil, err
	}
	if !user.EmailVerified {
		return nil, store.ErrNotFound
	}
	return user, nil
}

// touchIdentity bumps last_used_at on a successful OAuth login. Errors
// are logged but never surfaced — the user is already logged in and a
// stale timestamp is harmless.
func (m *Module) touchIdentity(ctx context.Context, identityID string) {
	defer func() {
		if r := recover(); r != nil && m.logger != nil {
			m.logger.Error("touchIdentity panicked", "panic", r)
		}
	}()
	err := m.idStore.Update(ctx, store.RID(identityID),
		store.Set(map[string]any{"last_used_at": time.Now()}))
	if err != nil && m.logger != nil {
		m.logger.Warn("touchIdentity failed", "identity_id", identityID, "error", err)
	}
}

// randomUnguessableSecret returns a 32-byte random base64-url string
// suitable for the OAuth-only User.PasswordHash bcrypt input.
func randomUnguessableSecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is catastrophic; fall back to time-based
		// entropy so we never return a constant. The real failure will
		// surface elsewhere (TLS, JWT signing) and crash the process.
		return fmt.Sprintf("oauth-secret-fallback-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// firstNonEmpty returns the first non-empty argument, or "" if all are.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// missingFieldFor inspects pi and returns the SPEC §8.1 hint key for
// the front-end's structured error response.
func missingFieldFor(pi *ProviderIdentity) string {
	switch {
	case pi.Email == "":
		return "email"
	case !pi.EmailVerified:
		return "verified"
	case pi.IsAliasedEmail:
		return "non_aliased"
	default:
		return ""
	}
}

// defaultNextActions returns the SPEC §8.1 user-facing remediation
// suggestions. Provider name interpolated for clarity.
func defaultNextActions(provider string) []string {
	prov := provider
	if prov == "" {
		prov = "OAuth"
	}
	return []string{
		"已注册用户:用密码登录后到设置页绑定 " + prov,
		"未注册用户:暂不支持纯 " + prov + " 创建账号(请先用邮箱密码注册)",
	}
}

// randomID returns a base64url-encoded 16-byte random id, used as both
// session sid and one-shot auth code. 128 bits of entropy is comfortably
// outside brute-force range for our 5-minute / 5-second TTLs.
func randomID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// Same fallback rationale as randomUnguessableSecret.
		return fmt.Sprintf("rid-fallback-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// pkceChallenge converts a PKCE verifier into the SHA-256
// code_challenge per RFC 7636 §4.2.
func pkceChallenge(verifier string) string {
	if verifier == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// providerHTTPMethodFor returns "POST" iff the provider's capabilities
// declare RequiresFormPost or CallbackMethod=="POST"; "GET" otherwise.
// Centralises the fallback so route registration and tests agree.
func providerHTTPMethodFor(p AuthProvider) string {
	caps := p.Capabilities()
	if strings.EqualFold(caps.CallbackMethod, "POST") || caps.RequiresFormPost {
		return "POST"
	}
	return "GET"
}
