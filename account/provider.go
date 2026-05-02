package account

import (
	"context"
	"net/url"
)

// AuthProvider is the chok-internal contract for an OAuth identity
// provider. Password authentication is NOT a provider — it lives on
// /login + User.PasswordHash and is intentionally outside this interface.
//
// Provider implementations live under account/providers/<name>/ and
// register themselves to the package-level providerRegistry via init() +
// RegisterProviderFactory; Module.RegisterProvider then attaches the
// runtime instance during Phase 3 config-driven assembly.
type AuthProvider interface {
	// Name is the lowercase provider identifier ("google", "github", ...).
	// Used as the URL segment in /auth/{name}/start and as the
	// providers map key.
	Name() string

	// Capabilities is statically declared at provider construction. The
	// Module reads it to decide:
	//   - HTTP method for /auth/{name}/callback (GET vs POST)
	//   - whether to generate a nonce in BeginAuth
	//   - whether to generate a PKCE code verifier
	//   - whether to ParseForm() before reading callback parameters
	Capabilities() ProviderCapabilities

	// BeginAuth returns the IdP authorization URL. The Module passes a
	// freshly-generated state (CSRF), nonce (if RequiresNonce), and PKCE
	// code_challenge (if SupportsPKCE) so the provider can fold them into
	// the redirect URL.
	BeginAuth(ctx context.Context, req *BeginRequest) (*BeginResponse, error)

	// CompleteAuth runs in /auth/{name}/callback. The Module has already
	// loaded the original session (via SessionCarrier + OAuthSessionStore)
	// and verified state matches; the provider exchanges the code for an
	// IdP token, validates id_token / userinfo, and returns the canonical
	// ProviderIdentity.
	CompleteAuth(ctx context.Context, req *CompleteRequest) (*ProviderIdentity, error)
}

// ProviderCapabilities is provider-side static configuration the Module
// consults to dispatch the OAuth flow correctly. Always returned by value
// — providers should never mutate it after construction.
type ProviderCapabilities struct {
	// CallbackMethod is "GET" (default OAuth2) or "POST" (Apple form_post).
	CallbackMethod string

	// RequiresNonce signals OIDC standard nonce — Module generates one in
	// BeginAuth, stores it in the session, and the provider validates the
	// id_token nonce claim against it inside CompleteAuth.
	RequiresNonce bool

	// SupportsPKCE — Module generates a code_verifier in BeginAuth, stores
	// it, hashes (SHA-256) for the code_challenge sent to IdP, and replays
	// the verifier in CompleteAuth.
	SupportsPKCE bool

	// RequiresFormPost — IdP returns the callback as an HTML form POST
	// (Apple). Module must ParseForm() and read code/state from the body
	// instead of the query string.
	RequiresFormPost bool
}

// BeginRequest is what the Module hands to AuthProvider.BeginAuth.
// Provider implementations are read-only consumers — they MUST NOT mutate.
type BeginRequest struct {
	// State is the CSRF token. Module generates it, stores it in the
	// OAuthSession, and the provider folds it into the IdP authorize URL
	// as &state=...
	State string

	// Nonce is the OIDC nonce, populated only when Capabilities().RequiresNonce.
	Nonce string

	// CodeChallenge is the PKCE SHA-256 challenge of the verifier. The
	// Module already computed the hash; provider just appends to URL.
	CodeChallenge string

	// RedirectBack is the validated relative path or whitelisted absolute
	// URL that the front-end will use after /auth/exchange. Provided here
	// for providers that round-trip it (most don't); the canonical copy
	// lives in OAuthSession and is consulted by handleCallback after
	// SessionStore.Take.
	RedirectBack string
}

// BeginResponse is what AuthProvider.BeginAuth returns to the Module.
type BeginResponse struct {
	// RedirectTo is the fully-formed IdP authorize URL the Module will
	// 302 to. Required.
	RedirectTo string
}

// CompleteRequest is what the Module hands to AuthProvider.CompleteAuth.
type CompleteRequest struct {
	// Code is the OAuth code (from query string for GET callbacks, from
	// form body for Apple POST). Module's getParam helper picks the
	// right source.
	Code string

	// State is the round-tripped CSRF token. Module has already verified
	// it matches OAuthSession.State before calling CompleteAuth — this
	// field is provided for providers that re-attach it to downstream IdP
	// requests, not for re-validation.
	State string

	// Nonce is the OAuthSession.Nonce; the provider compares it against
	// the id_token nonce claim during token verification.
	Nonce string

	// CodeVerifier is the PKCE secret the provider sends to the IdP token
	// endpoint to prove possession of the original code_challenge.
	CodeVerifier string

	// FormBody is the raw POST form data for RequiresFormPost providers
	// (Apple). Most providers ignore this field. Apple reads
	// FormBody.Get("user") for the optional first-name JSON.
	FormBody url.Values
}

// ProviderIdentity is the canonical chok-internal representation of an
// IdP user, returned by CompleteAuth. ResolveOAuthIdentity then maps it
// to a chok User (lookup / link / create per SPEC §6.2 + §8).
type ProviderIdentity struct {
	// Provider is the provider Name(). Set redundantly so the Identity
	// row carries it without needing to walk back up the call.
	Provider string

	// ProviderAccountID is the IdP's stable user identifier (Google sub,
	// GitHub id, Apple sub). Combined with Provider it is the unique
	// (provider, provider_account_id) join key on the Identity table.
	ProviderAccountID string

	// Email is the user's primary email address as reported by the IdP.
	// May be empty (GitHub default scope, locked-down enterprise OIDC) —
	// SPEC §8.1 gates account creation on non-empty + EmailVerified +
	// !IsAliasedEmail.
	Email string

	// EmailVerified reflects the IdP's claim that the user controls Email.
	EmailVerified bool

	// IsAliasedEmail flags Apple's privaterelay.appleid.com style relay
	// addresses. Even when EmailVerified=true, an aliased email does NOT
	// represent ownership of the underlying mailbox, so SPEC §8 blocks
	// LinkByEmail auto-merge for these.
	IsAliasedEmail bool

	// Name is the user's display name as reported by the IdP, or empty.
	// ResolveOAuthIdentity falls back to maskEmail(Email) on first
	// account creation.
	Name string

	// Picture is the avatar URL, optional.
	Picture string

	// Raw is the full provider-specific payload. Persisted as JSON in
	// Identity.Profile for audit / future migration. Implementations
	// should put canonical fields in the typed members above and use Raw
	// only for vendor-specific extras.
	Raw map[string]any
}
