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

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
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
	if m.sessionCarrier == nil {
		secret, err := deriveOAuthSessionSecret(m.signingKey)
		if err != nil {
			return fmt.Errorf("account: derive cookie secret: %w", err)
		}
		opts := []CookieOption{}
		if isLocalDevRedirect(m.firstRedirectURL) && m.logger != nil {
			opts = append(opts, WithDevMode())
			m.logger.Info("oauth cookie carrier in dev mode (HTTP localhost detected)")
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

		// Create a new User. PV=0 marks "OAuth-only signal" (SPEC §4.1
		// v0.3.5) — combined with no password identity row, /login
		// rejects this user with the "use OAuth" message.
		randomHash, err := auth.HashPassword(randomUnguessableSecret())
		if err != nil {
			return err
		}
		newUser := &User{
			Email:           pi.Email,
			EmailVerified:   pi.EmailVerified && !pi.IsAliasedEmail,
			PasswordHash:    randomHash,
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
	raw, _ := json.Marshal(pi.Raw)
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
// user with no login method at all (SPEC §6.2). Performs an explicit
// ownership check (Identity.UserID == userID) before deletion so a
// malformed identityID cannot wipe another user's binding.
func (m *Module) UnlinkIdentity(ctx context.Context, userID, identityID string) error {
	if userID == "" || identityID == "" {
		return apierr.ErrInvalidArgument.WithMessage("userID and identityID are required")
	}

	// Load + ownership check first. ErrNotFound for both "no such row"
	// and "row belongs to a different user" — the latter must not
	// expose existence to a probing attacker.
	ident, err := m.idStore.Get(ctx, store.RID(identityID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apierr.ErrNotFound.WithMessage("identity not found")
		}
		return err
	}
	if ident.UserID != userID {
		return apierr.ErrNotFound.WithMessage("identity not found")
	}

	methods, err := m.ListLoginMethods(ctx, userID)
	if err != nil {
		return err
	}
	if len(methods) <= 1 {
		return apierr.ErrFailedPrecondition.WithMessage("至少保留一种登录方式(密码或 OAuth)")
	}
	return m.idStore.Delete(ctx, store.RID(identityID))
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
// user: a "password" virtual entry when the user has any password
// activity (PV>0 or no OAuth identities), plus one entry per OAuth
// Identity row.
//
// The PV>0 heuristic distinguishes "OAuth-only" users (synthetic random
// PasswordHash, PV=0) from "user who has set a password at least once"
// (changePassword / resetPassword bumps PV>0). A pure-password user with
// PV=0 (i.e. a freshly registered user who has never reset) still gets
// the password method because they have no OAuth identities.
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
	hasPassword := user.PasswordVersion > 0 || len(idents) == 0
	if hasPassword {
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

// userHasPasswordHistory returns true if the user has any
// non-OAuth login history. Used by /login to reject PV=0 OAuth-only
// accounts (SPEC §4.1 v0.3.5). The implementation is conservative:
// PV>0 is sufficient (any password change bumps it). For PV=0 we
// check whether ANY Identity rows exist — if zero, the account is a
// regular freshly-registered password user (PV=0 baseline).
func (m *Module) userHasPasswordHistory(ctx context.Context, userID string) (bool, error) {
	user, err := m.userStore.Get(ctx, store.RID(userID))
	if err != nil {
		return false, err
	}
	if user.PasswordVersion > 0 {
		return true, nil
	}
	idents, err := m.ListIdentities(ctx, userID)
	if err != nil {
		return false, err
	}
	return len(idents) == 0, nil
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
