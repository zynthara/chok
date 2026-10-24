// Package google is Sign in with Google for identity: OpenID Connect,
// with a nonce and PKCE.
//
//	auth := identity.MustNew(cfg.Auth, d,
//		identity.Providers(google.New(cfg.Auth.Providers["google"])))
//
// It reads, from auth.providers.google: client_id and client_secret (an
// OAuth client of the Google Cloud console), redirect_url (registered
// with that client: https://api.example.com/auth/google/callback),
// scopes (openid, email and profile by default), hosted_domain (accept
// only accounts of that Workspace domain) and endpoint (the issuer,
// https://accounts.google.com unless testing). It refuses team_id and
// key_id. The user's identity comes from the verified ID token, not from
// another API call.
package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"

	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/identity/internal/oauth"
	"github.com/zynthara/chok/v3/identity/internal/oidc"
)

const productionIssuer = "https://accounts.google.com"

type provider struct {
	pc     identity.ProviderConfig
	scopes []string
	issuer *oidc.Issuer
}

// New returns the Google provider for pc. A configuration it refuses
// makes identity.New fail with the reason.
func New(pc identity.ProviderConfig) identity.Provider {
	if err := check(pc); err != nil {
		return oauth.Broken{ProviderName: "google", Err: err}
	}
	issuer := pc.Endpoint
	if issuer == "" {
		issuer = productionIssuer
	}
	scopes := pc.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	return &provider{pc: pc, scopes: scopes, issuer: oidc.New(issuer)}
}

func check(pc identity.ProviderConfig) error {
	switch {
	case pc.ClientID == "":
		return errors.New("google: client_id is required")
	case pc.ClientSecret == "":
		return errors.New("google: client_secret is required")
	case pc.RedirectURL == "":
		return errors.New("google: redirect_url is required")
	}
	if err := oauth.AbsoluteURL("google: redirect_url", pc.RedirectURL); err != nil {
		return err
	}
	if pc.Endpoint != "" {
		if err := oauth.AbsoluteURL("google: endpoint", pc.Endpoint); err != nil {
			return err
		}
	}
	return oauth.Unused("google", map[string]bool{"team_id": pc.TeamID != "", "key_id": pc.KeyID != ""})
}

func (p *provider) Name() string { return "google" }

func (p *provider) Check() error { return nil }

// Capabilities: Google takes a nonce and PKCE, and calls back with GET.
func (p *provider) Capabilities() oauth.Capabilities {
	return oauth.Capabilities{CallbackMethod: http.MethodGet, RequiresNonce: true, SupportsPKCE: true, RedirectURL: p.pc.RedirectURL}
}

func (p *provider) config(ctx context.Context) (*oauth2.Config, error) {
	m, err := p.issuer.Metadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("google: %w", err)
	}
	return &oauth2.Config{
		ClientID: p.pc.ClientID, ClientSecret: p.pc.ClientSecret, RedirectURL: p.pc.RedirectURL, Scopes: p.scopes,
		Endpoint: oauth2.Endpoint{AuthURL: m.AuthorizationEndpoint, TokenURL: m.TokenEndpoint},
	}, nil
}

// BeginAuth builds the authorization URL with the state, the nonce, the
// PKCE challenge and, when set, the hosted domain. Access is online: the
// app never calls Google for the user, so it asks for no refresh token.
func (p *provider) BeginAuth(ctx context.Context, req *oauth.BeginRequest) (string, error) {
	cfg, err := p.config(ctx)
	if err != nil {
		return "", err
	}
	opts := []oauth2.AuthCodeOption{oauth2.AccessTypeOnline}
	if req.Nonce != "" {
		opts = append(opts, oauth2.SetAuthURLParam("nonce", req.Nonce))
	}
	if req.CodeChallenge != "" {
		opts = append(opts, oauth2.SetAuthURLParam("code_challenge", req.CodeChallenge), oauth2.SetAuthURLParam("code_challenge_method", "S256"))
	}
	if p.pc.HostedDomain != "" {
		opts = append(opts, oauth2.SetAuthURLParam("hd", p.pc.HostedDomain))
	}
	return cfg.AuthCodeURL(req.State, opts...), nil
}

type idClaims struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	Picture       string `json:"picture"`
	Locale        string `json:"locale"`
	Nonce         string `json:"nonce"`
	// HD is the Workspace domain of a managed account; empty for gmail.com.
	HD string `json:"hd"`
}

// CompleteAuth trades the code for tokens and reads the user from the ID
// token: signature, issuer, audience and expiry checked against Google's
// published keys, then the nonce and the hosted domain.
func (p *provider) CompleteAuth(ctx context.Context, req *oauth.CompleteRequest) (*oauth.Identity, error) {
	cfg, err := p.config(ctx)
	if err != nil {
		return nil, err
	}
	var opts []oauth2.AuthCodeOption
	if req.CodeVerifier != "" {
		opts = append(opts, oauth2.SetAuthURLParam("code_verifier", req.CodeVerifier))
	}
	tok, err := cfg.Exchange(ctx, req.Code, opts...)
	if err != nil {
		return nil, fmt.Errorf("google: token exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, errors.New("google: the token response has no id_token")
	}
	var c idClaims
	if err := p.issuer.Verify(ctx, raw, p.pc.ClientID, &c); err != nil {
		return nil, fmt.Errorf("google: id_token verify: %w", err)
	}
	// The verifier cannot know the nonce: it is this sign-in's.
	if req.Nonce != "" && c.Nonce != req.Nonce {
		return nil, errors.New("google: id_token nonce mismatch")
	}
	if p.pc.HostedDomain != "" && c.HD != p.pc.HostedDomain {
		return nil, fmt.Errorf("google: hosted domain %q is not the required %q", c.HD, p.pc.HostedDomain)
	}
	return &oauth.Identity{
		Provider: "google", ProviderAccountID: c.Sub, Email: c.Email, EmailVerified: c.EmailVerified,
		Name: c.Name, Picture: c.Picture,
		Raw: map[string]any{"hosted_domain": c.HD, "given_name": c.GivenName, "family_name": c.FamilyName, "locale": c.Locale},
	}, nil
}
