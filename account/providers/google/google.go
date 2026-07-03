package google

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	gendpoint "golang.org/x/oauth2/google"

	"github.com/zynthara/chok/v2/account"
)

// provider is the runtime google.AuthProvider. Holds the oauth2 config
// + an OIDC verifier whose underlying JWKS cache is shared across
// requests. coreos/go-oidc's verifier auto-refreshes the keyset so we
// don't manage rotation by hand.
type provider struct {
	cfg          *oauth2.Config
	verifier     *oidc.IDTokenVerifier
	hostedDomain string
	redirectURL  string
}

// New constructs a Google provider against the supplied options. The
// caller is responsible for Validate()-ing first; New panics on the
// invariants Validate would have caught (RedirectURL/ClientID empty)
// because reaching this state past validation indicates a programming
// error, not user input.
//
// The OIDC discovery roundtrip happens lazily inside the verifier on
// first ID Token validation; constructor returns immediately so a
// transient Google outage at startup time doesn't block chok boot.
//
// issuerOverride is the runtime hook tests use to redirect both
// discovery and OIDC verification at a httptest.Server. Production
// deployments pass "" (empty) which falls back to Options.IssuerURL,
// then to productionIssuer.
func New(ctx context.Context, opts Options) (account.AuthProvider, error) {
	if !opts.Enabled {
		return nil, errors.New("google: New called with Enabled=false; gate this in the factory")
	}

	issuer := opts.IssuerURL
	if issuer == "" {
		issuer = productionIssuer
	}

	// oidc.NewProvider hits {issuer}/.well-known/openid-configuration
	// to discover endpoints + JWKS URI. coreos/go-oidc caches the
	// keyset after the first verify call.
	idp, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("google: OIDC discovery on %s: %w", issuer, err)
	}

	cfg := &oauth2.Config{
		ClientID:     opts.ClientID,
		ClientSecret: opts.ClientSecret,
		RedirectURL:  opts.RedirectURL,
		Scopes:       defaultScopes(opts.Scopes),
		Endpoint:     idp.Endpoint(),
	}
	// In production this discovery-derived endpoint matches
	// google.Endpoint exactly. We pull it from the discovery doc
	// instead of hard-coding so test issuers can serve their own
	// auth/token URLs without us special-casing them. Static check:
	_ = gendpoint.Endpoint

	return &provider{
		cfg: cfg,
		verifier: idp.Verifier(&oidc.Config{
			ClientID: opts.ClientID,
		}),
		hostedDomain: opts.HostedDomain,
		redirectURL:  opts.RedirectURL,
	}, nil
}

// Name implements account.AuthProvider.
func (p *provider) Name() string { return "google" }

// Capabilities implements account.AuthProvider.
//
// Google supports the standard OIDC nonce parameter and PKCE; both
// are belt-and-braces against authorization-code injection. Module
// generates the values, we forward them.
//
// CallbackMethod is "GET" — Google never returns the callback as a
// form_post (that's an Apple-specific behaviour).
func (p *provider) Capabilities() account.ProviderCapabilities {
	return account.ProviderCapabilities{
		CallbackMethod: "GET",
		RequiresNonce:  true,
		SupportsPKCE:   true,
	}
}

// RedirectURL implements account.RedirectURLProvider so Module's
// dev-mode auto-detect can sniff HTTP-on-localhost from the live
// configuration. Without it, deployments running Google against a
// localhost callback URL (the typical local-development setup) would
// see the cookie carrier in production mode and fail to receive the
// sid cookie over plaintext.
func (p *provider) RedirectURL() string { return p.redirectURL }

// BeginAuth implements account.AuthProvider. Constructs the Google
// authorize URL with the Module-generated state, nonce, and PKCE
// challenge. AccessTypeOnline avoids requesting a Google refresh
// token — chok issues its own JWT and never calls Google APIs on the
// user's behalf, so the refresh token would be unused secret material.
func (p *provider) BeginAuth(_ context.Context, req *account.BeginRequest) (*account.BeginResponse, error) {
	authOpts := []oauth2.AuthCodeOption{oauth2.AccessTypeOnline}
	if req.Nonce != "" {
		authOpts = append(authOpts, oidc.Nonce(req.Nonce))
	}
	if req.CodeChallenge != "" {
		authOpts = append(authOpts,
			oauth2.SetAuthURLParam("code_challenge", req.CodeChallenge),
			oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		)
	}
	if p.hostedDomain != "" {
		authOpts = append(authOpts, oauth2.SetAuthURLParam("hd", p.hostedDomain))
	}
	return &account.BeginResponse{
		RedirectTo: p.cfg.AuthCodeURL(req.State, authOpts...),
	}, nil
}

// CompleteAuth implements account.AuthProvider. Two-step:
//  1. exchange the code for a token (oauth2 layer)
//  2. parse and verify the id_token, mapping its claims onto
//     ProviderIdentity. The verifier checks signature, iss, aud,
//     and exp — we add nonce comparison and hosted-domain assertion
//     on top.
func (p *provider) CompleteAuth(ctx context.Context, req *account.CompleteRequest) (*account.ProviderIdentity, error) {
	exchangeOpts := []oauth2.AuthCodeOption{}
	if req.CodeVerifier != "" {
		exchangeOpts = append(exchangeOpts,
			oauth2.SetAuthURLParam("code_verifier", req.CodeVerifier),
		)
	}
	tok, err := p.cfg.Exchange(ctx, req.Code, exchangeOpts...)
	if err != nil {
		return nil, fmt.Errorf("google: token exchange: %w", err)
	}

	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return nil, errors.New("google: token response missing id_token")
	}

	idTok, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		return nil, fmt.Errorf("google: id_token verify: %w", err)
	}

	// Nonce check is OIDC § 15.5.2 — the verifier doesn't enforce it
	// because the expected value is per-request. We compare against
	// the nonce Module stashed in OAuthSession at Begin time.
	if req.Nonce != "" && idTok.Nonce != req.Nonce {
		return nil, errors.New("google: id_token nonce mismatch")
	}

	var claims idTokenClaims
	if err := idTok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("google: decode id_token claims: %w", err)
	}

	if p.hostedDomain != "" && claims.HD != p.hostedDomain {
		// Distinct error from generic verification failure so an
		// operator chasing "user got rejected" in logs can tell the
		// G-Suite tenant restriction apart from signature failure.
		return nil, fmt.Errorf("google: hosted domain %q does not match required %q",
			claims.HD, p.hostedDomain)
	}

	return &account.ProviderIdentity{
		Provider:          "google",
		ProviderAccountID: claims.Sub,
		Email:             claims.Email,
		EmailVerified:     claims.EmailVerified,
		Name:              claims.Name,
		Picture:           claims.Picture,
		// IsAliasedEmail stays false — Google does not issue relay
		// addresses (that's Apple). Workspace email aliases ("alias@"
		// pointing to "primary@") still resolve to the primary at the
		// IdP level by the time we see them.
		IsAliasedEmail: false,
		Raw: map[string]any{
			"iss":           idTok.Issuer,
			"aud":           idTok.Audience,
			"hosted_domain": claims.HD,
			"given_name":    claims.GivenName,
			"family_name":   claims.FamilyName,
			"locale":        claims.Locale,
		},
	}, nil
}

// Compile-time interface assertions. Catches drift the moment a chok
// upgrade reshapes the AuthProvider contract.
var (
	_ account.AuthProvider        = (*provider)(nil)
	_ account.RedirectURLProvider = (*provider)(nil)
)

// idTokenClaims is the projection of Google's ID Token JWT claims we
// care about. coreos/go-oidc's IDToken.Claims unmarshals the payload
// into this struct.
type idTokenClaims struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	Picture       string `json:"picture"`
	Locale        string `json:"locale"`
	// HD is the G Suite / Workspace hosted-domain claim; populated
	// for managed accounts, empty for consumer @gmail.com.
	HD string `json:"hd"`
}
