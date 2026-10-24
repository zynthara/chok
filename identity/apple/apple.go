// Package apple is Sign in with Apple for identity: OpenID Connect with
// a nonce, the callback posted as a form, and a client secret the
// provider signs itself.
//
//	auth := identity.MustNew(cfg.Auth, d,
//		identity.Providers(apple.New(cfg.Auth.Providers["apple"])))
//
// It reads, from auth.providers.apple: client_id (the Services ID, such
// as com.example.web), client_secret (the PEM contents of the .p8 key
// made for Sign in with Apple — a secret, set it from the environment),
// team_id and key_id (ten characters each), redirect_url (a return URL
// of the Services ID: https://api.example.com/auth/apple/callback),
// scopes (name and email by default) and endpoint (the issuer,
// https://appleid.apple.com unless testing). It refuses hosted_domain.
//
// Apple's quirks, handled here: the client secret is an ES256 JWT, signed
// with the .p8 key and cached for an hour; the callback is a form post
// across sites, so its cookies are SameSite=None and need https even in
// development; the user's name comes only with the first sign-in, in the
// form's user field; and a "hide my email" relay address is marked as
// aliased, so it never links to an existing account.
package apple

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"

	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/identity/internal/oauth"
	"github.com/zynthara/chok/v3/identity/internal/oidc"
)

const (
	productionIssuer = "https://appleid.apple.com"
	// audience is the aud of the client secret, whatever the issuer.
	audience = "https://appleid.apple.com"
	// secretTTL is how long a signed client secret lives; the provider
	// signs the next one a minute before it ends.
	secretTTL = time.Hour
)

type provider struct {
	pc     identity.ProviderConfig
	scopes []string
	issuer *oidc.Issuer
	secret *clientSecret
}

// New returns the Apple provider for pc. A configuration it refuses — a
// key that is not an ECDSA P-256 .p8 key, among others — makes
// identity.New fail with the reason.
func New(pc identity.ProviderConfig) identity.Provider {
	key, err := check(pc)
	if err != nil {
		return oauth.Broken{ProviderName: "apple", Err: err}
	}
	issuer := pc.Endpoint
	if issuer == "" {
		issuer = productionIssuer
	}
	scopes := pc.Scopes
	if len(scopes) == 0 {
		scopes = []string{"name", "email"}
	}
	return &provider{
		pc: pc, scopes: scopes, issuer: oidc.New(issuer),
		secret: &clientSecret{teamID: pc.TeamID, keyID: pc.KeyID, clientID: pc.ClientID, key: key, now: time.Now},
	}
}

func check(pc identity.ProviderConfig) (*ecdsa.PrivateKey, error) {
	switch {
	case pc.ClientID == "":
		return nil, errors.New("apple: client_id (the Services ID) is required")
	case len(pc.TeamID) != 10:
		return nil, fmt.Errorf("apple: team_id is 10 characters, got %d", len(pc.TeamID))
	case len(pc.KeyID) != 10:
		return nil, fmt.Errorf("apple: key_id is 10 characters, got %d", len(pc.KeyID))
	case pc.ClientSecret == "":
		return nil, errors.New("apple: client_secret (the PEM contents of the .p8 key) is required")
	case !strings.Contains(pc.ClientSecret, "BEGIN PRIVATE KEY"):
		return nil, errors.New("apple: client_secret must be the PEM-encoded .p8 key (no BEGIN PRIVATE KEY marker)")
	case pc.RedirectURL == "":
		return nil, errors.New("apple: redirect_url is required")
	}
	if err := oauth.AbsoluteURL("apple: redirect_url", pc.RedirectURL); err != nil {
		return nil, err
	}
	if pc.Endpoint != "" {
		if err := oauth.AbsoluteURL("apple: endpoint", pc.Endpoint); err != nil {
			return nil, err
		}
	}
	if err := oauth.Unused("apple", map[string]bool{"hosted_domain": pc.HostedDomain != ""}); err != nil {
		return nil, err
	}
	return parseKey(pc.ClientSecret)
}

// parseKey reads the .p8 key: PKCS #8, ECDSA.
func parseKey(pemText string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("apple: client_secret: no PEM block")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("apple: client_secret: a PEM %q block, not PRIVATE KEY (PKCS #8)", block.Type)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apple: client_secret: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("apple: client_secret: the key is %T, not ECDSA; Apple's .p8 keys are P-256", parsed)
	}
	return key, nil
}

func (p *provider) Name() string { return "apple" }

func (p *provider) Check() error { return nil }

// Capabilities: Apple posts the callback as a form, takes a nonce and no
// PKCE.
func (p *provider) Capabilities() oauth.Capabilities {
	return oauth.Capabilities{CallbackMethod: http.MethodPost, RequiresNonce: true, RequiresFormPost: true, RedirectURL: p.pc.RedirectURL}
}

func (p *provider) config(ctx context.Context) (*oauth2.Config, error) {
	m, err := p.issuer.Metadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("apple: %w", err)
	}
	// The secret goes in the form body, per request, not as a static
	// client secret.
	return &oauth2.Config{
		ClientID: p.pc.ClientID, RedirectURL: p.pc.RedirectURL, Scopes: p.scopes,
		Endpoint: oauth2.Endpoint{AuthURL: m.AuthorizationEndpoint, TokenURL: m.TokenEndpoint, AuthStyle: oauth2.AuthStyleInParams},
	}, nil
}

// BeginAuth builds the authorization URL: response_mode=form_post, which
// asking for the name and email requires, and the nonce.
func (p *provider) BeginAuth(ctx context.Context, req *oauth.BeginRequest) (string, error) {
	cfg, err := p.config(ctx)
	if err != nil {
		return "", err
	}
	opts := []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("response_mode", "form_post")}
	if req.Nonce != "" {
		opts = append(opts, oauth2.SetAuthURLParam("nonce", req.Nonce))
	}
	return cfg.AuthCodeURL(req.State, opts...), nil
}

type idClaims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	// Apple sends these two as "true" / "false" strings, or as booleans.
	EmailVerified  any    `json:"email_verified"`
	IsPrivateEmail any    `json:"is_private_email"`
	Nonce          string `json:"nonce"`
}

// CompleteAuth trades the code for tokens, with a fresh-enough client
// secret, and reads the user from the ID token; the name comes from the
// form's user field, which Apple sends with the first sign-in only.
func (p *provider) CompleteAuth(ctx context.Context, req *oauth.CompleteRequest) (*oauth.Identity, error) {
	cfg, err := p.config(ctx)
	if err != nil {
		return nil, err
	}
	secret, err := p.secret.get()
	if err != nil {
		return nil, err
	}
	tok, err := cfg.Exchange(ctx, req.Code, oauth2.SetAuthURLParam("client_secret", secret))
	if err != nil {
		return nil, fmt.Errorf("apple: token exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, errors.New("apple: the token response has no id_token")
	}
	var c idClaims
	if err := p.issuer.Verify(ctx, raw, p.pc.ClientID, &c); err != nil {
		return nil, fmt.Errorf("apple: id_token verify: %w", err)
	}
	if req.Nonce != "" && c.Nonce != req.Nonce {
		return nil, errors.New("apple: id_token nonce mismatch")
	}
	private := truthy(c.IsPrivateEmail)
	return &oauth.Identity{
		Provider: "apple", ProviderAccountID: c.Sub, Email: c.Email, EmailVerified: truthy(c.EmailVerified),
		IsAliasedEmail: private, Name: nameFrom(req.FormBody),
		Raw: map[string]any{"is_private_email": private},
	}, nil
}

// truthy reads Apple's string-or-boolean claims.
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true")
	}
	return false
}

// nameFrom reads the name in the form's user field, {"name":{"firstName",
// "lastName"}}; it is "" after the first sign-in or when malformed.
func nameFrom(form url.Values) string {
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
	if json.Unmarshal([]byte(raw), &u) != nil {
		return ""
	}
	return strings.TrimSpace(u.Name.FirstName + " " + u.Name.LastName)
}

// clientSecret is the ES256 JWT Apple takes as the client secret,
// signed once and reused until a minute before it expires.
type clientSecret struct {
	teamID, keyID, clientID string
	key                     *ecdsa.PrivateKey
	now                     func() time.Time

	mu      sync.Mutex
	value   string
	renewAt time.Time
}

func (c *clientSecret) get() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.value != "" && now.Before(c.renewAt) {
		return c.value, nil
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": c.teamID, "iat": now.Unix(), "exp": now.Add(secretTTL).Unix(), "aud": audience, "sub": c.clientID,
	})
	tok.Header["kid"] = c.keyID
	signed, err := tok.SignedString(c.key)
	if err != nil {
		return "", fmt.Errorf("apple: sign the client secret: %w", err)
	}
	c.value, c.renewAt = signed, now.Add(secretTTL-time.Minute)
	return signed, nil
}
