package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/zynthara/chok/v2/account"
)

// provider is the runtime apple.AuthProvider. It owns the
// ES256-signed client_secret cache, the OIDC verifier (JWKS auto-
// refresh through coreos/go-oidc), the oauth2 config, and the
// per-flow capabilities Module dispatches against.
type provider struct {
	cfg          *oauth2.Config
	verifier     *oidc.IDTokenVerifier
	secretCache  *clientSecretCache
	redirectURL  string
}

// New constructs an Apple provider. PEM parsing happens here so a bad
// .p8 surfaces at chok startup rather than at the first user click.
//
// OIDC discovery hits {issuer}/.well-known/openid-configuration
// lazily on the first ID Token verify; constructor returns
// immediately so a transient outage at boot doesn't block chok.
func New(ctx context.Context, opts Options) (account.AuthProvider, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	privKey, err := parsePrivateKey(opts.PrivateKey)
	if err != nil {
		return nil, err
	}

	issuer := opts.IssuerURL
	if issuer == "" {
		issuer = productionIssuer
	}
	idp, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("apple: OIDC discovery on %s: %w", issuer, err)
	}

	return &provider{
		cfg: &oauth2.Config{
			ClientID: opts.ServiceID, // Apple's "client" is the Service ID
			// ClientSecret is intentionally left blank — Apple
			// expects a per-request signed JWT in the form body, not
			// a static value. CompleteAuth injects clientSecretCache.Get()
			// into oauth2.Exchange via SetAuthURLParam, bypassing
			// the oauth2.Config-level secret.
			RedirectURL: opts.RedirectURL,
			Scopes:      []string{"name", "email"},
			Endpoint:    idp.Endpoint(),
		},
		verifier: idp.Verifier(&oidc.Config{
			ClientID: opts.ServiceID,
		}),
		secretCache: newClientSecretCache(
			opts.TeamID, opts.KeyID, opts.ServiceID, opts.ClientSecretTTL, privKey,
		),
		redirectURL: opts.RedirectURL,
	}, nil
}

// Name implements account.AuthProvider.
func (p *provider) Name() string { return "apple" }

// Capabilities implements account.AuthProvider.
//
// Apple is the only blessed provider with non-default capabilities:
//   - CallbackMethod=POST: Apple delivers the callback as form_post,
//     so Module mounts /auth/apple/callback as a POST route and
//     reads code/state from c.Request.PostForm.
//   - RequiresFormPost=true: triggers Module's ParseForm() and the
//     getParam helper to source from form body instead of query.
//   - RequiresNonce=true: Apple is a strict OIDC IdP; ID Token
//     replay defense requires nonce. Module generates one in Begin
//     and we validate the claim in Complete.
//   - SupportsPKCE=false: Apple Sign-In does not currently advertise
//     PKCE support; setting it true would result in extra params
//     Apple ignores.
func (p *provider) Capabilities() account.ProviderCapabilities {
	return account.ProviderCapabilities{
		CallbackMethod:   "POST",
		RequiresNonce:    true,
		SupportsPKCE:     false,
		RequiresFormPost: true,
	}
}

// RedirectURL implements account.RedirectURLProvider.
//
// Note that for Apple's form_post flow specifically, dev-mode (HTTP
// localhost) is incompatible: the callback POST is cross-site
// (appleid.apple.com → app.example.com), and SameSite=Lax (which
// dev-mode applies) drops cookies on cross-site POST. Module's
// auto-detect will still flip dev-mode if redirect_url is HTTP
// localhost; production Apple users must use HTTPS even locally.
func (p *provider) RedirectURL() string { return p.redirectURL }

// BeginAuth implements account.AuthProvider.
//
// Apple specifically requires `response_mode=form_post` to trigger
// the form_post callback; without it, Apple uses query string.
// Module's RequiresFormPost capability promises form-body parsing
// downstream, so we must request the matching response_mode.
func (p *provider) BeginAuth(_ context.Context, req *account.BeginRequest) (*account.BeginResponse, error) {
	authOpts := []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam("response_mode", "form_post"),
	}
	if req.Nonce != "" {
		authOpts = append(authOpts, oidc.Nonce(req.Nonce))
	}
	return &account.BeginResponse{
		RedirectTo: p.cfg.AuthCodeURL(req.State, authOpts...),
	}, nil
}

// CompleteAuth implements account.AuthProvider.
//
// Three concerns interleave:
//   1. The token exchange must inject the dynamically-signed
//      client_secret into the form body (oauth2.Config has no slot
//      for a per-request secret, so we use SetAuthURLParam).
//   2. The id_token verifier checks signature + iss + aud + exp
//      via JWKS. Nonce is per-request — we compare claims.Nonce
//      against req.Nonce.
//   3. The user's name (firstName/lastName) only arrives on the
//      first callback after consent in a `user` form field. We
//      parse it nil-safely and concat into ProviderIdentity.Name.
//
// Apple's `is_private_email` claim signals a privaterelay alias
// rather than the user's real mailbox; we propagate it as
// IsAliasedEmail so Module's §8 LinkByEmail and §8.1 create-user
// gate both refuse it.
func (p *provider) CompleteAuth(ctx context.Context, req *account.CompleteRequest) (*account.ProviderIdentity, error) {
	clientSecret, err := p.secretCache.Get()
	if err != nil {
		return nil, fmt.Errorf("apple: client_secret: %w", err)
	}

	tok, err := p.cfg.Exchange(ctx, req.Code,
		// Apple's token endpoint expects the dynamic JWT in
		// `client_secret`, NOT a basic-auth header. SetAuthURLParam
		// adds it to the POST body the oauth2 lib sends.
		oauth2.SetAuthURLParam("client_secret", clientSecret),
	)
	if err != nil {
		return nil, fmt.Errorf("apple: token exchange: %w", err)
	}

	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return nil, errors.New("apple: token response missing id_token")
	}

	idTok, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		return nil, fmt.Errorf("apple: id_token verify: %w", err)
	}
	if req.Nonce != "" && idTok.Nonce != req.Nonce {
		return nil, errors.New("apple: id_token nonce mismatch")
	}

	var claims appleIDClaims
	if err := idTok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("apple: decode id_token claims: %w", err)
	}

	name := nameFromUserField(req.FormBody)

	return &account.ProviderIdentity{
		Provider:          "apple",
		ProviderAccountID: claims.Sub,
		Email:             claims.Email,
		// claims.EmailVerified is "true"|"false" (string, not bool)
		// in Apple's id_token. Apple-specific quirk; coerce to bool.
		EmailVerified:  claims.EmailVerifiedAsBool(),
		// Apple's `is_private_email` flags the @privaterelay.appleid.com
		// alias case. Surface as IsAliasedEmail so Module's SPEC §8
		// LinkByEmail double-check and §8.1 create-user gate both
		// reject it. See package doc for the squatting rationale.
		IsAliasedEmail: claims.IsPrivateEmailAsBool(),
		Name:           name,
		Picture:        "", // Apple Sign-In does not return a profile picture
		Raw: map[string]any{
			"iss":              idTok.Issuer,
			"aud":              idTok.Audience,
			"is_private_email": claims.IsPrivateEmailAsBool(),
		},
	}, nil
}

// nameFromUserField parses the `user` form field Apple returns ONCE
// on the first authorisation. It contains a JSON object like
// `{"name":{"firstName":"...","lastName":"..."}, "email":"..."}`.
// Subsequent logins return no `user` field; we degrade gracefully to
// an empty Name and let downstream account flow fall back on
// maskEmail / username. Errors during JSON parse are non-fatal —
// log-worthy upstream but not login-blocking.
func nameFromUserField(form url.Values) string {
	raw := form.Get("user")
	if raw == "" {
		return ""
	}
	var u struct {
		Name struct {
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
		} `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		return ""
	}
	first, last := u.Name.FirstName, u.Name.LastName
	switch {
	case first != "" && last != "":
		return first + " " + last
	case first != "":
		return first
	case last != "":
		return last
	default:
		return ""
	}
}

// Compile-time interface assertions.
var (
	_ account.AuthProvider        = (*provider)(nil)
	_ account.RedirectURLProvider = (*provider)(nil)
)

// appleIDClaims captures the subset of Apple's ID Token we care
// about. Apple's quirks:
//   - `email_verified` is a STRING ("true"/"false"), not a bool. We
//     decode as `any` and coerce in EmailVerifiedAsBool.
//   - `is_private_email` is the same shape. Same coercion path.
type appleIDClaims struct {
	Sub            string `json:"sub"`
	Email          string `json:"email"`
	EmailVerified  any    `json:"email_verified"`
	IsPrivateEmail any    `json:"is_private_email"`
}

// EmailVerifiedAsBool coerces Apple's stringly-typed
// email_verified claim to a bool. Apple has historically returned
// "true" / "false" / true / false in different SDK paths; we accept
// any of the four.
func (c appleIDClaims) EmailVerifiedAsBool() bool {
	return coerceBoolClaim(c.EmailVerified)
}

// IsPrivateEmailAsBool coerces is_private_email the same way.
func (c appleIDClaims) IsPrivateEmailAsBool() bool {
	return coerceBoolClaim(c.IsPrivateEmail)
}

func coerceBoolClaim(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true")
	default:
		return false
	}
}
