// Package testfake supplies a deterministic AuthProvider implementation
// for in-process OAuth flow tests. Real provider packages
// (account/providers/google, …) live elsewhere; this is the unit/e2e
// stand-in used by Module-level tests so they need not spin up an
// external IdP.
package testfake

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"

	"github.com/zynthara/chok/account"
)

// Provider is a configurable fake AuthProvider. Tests pre-seed the
// expected ProviderIdentity (or per-code map) and the BeginAuth
// behaviour, then exercise Module's flow against the resulting handler.
type Provider struct {
	name        string
	caps        account.ProviderCapabilities
	redirectURL string
	mu          sync.Mutex
	codes       map[string]*account.ProviderIdentity // code → identity to return on CompleteAuth
	beginFn     func(req *account.BeginRequest) (*account.BeginResponse, error)
	completeErr error // injected error for CompleteAuth — used by handleCallback fault tests
	beginErr    error // injected error for BeginAuth — used by handleBegin rollback tests
}

// New returns a Provider with default OAuth2 capabilities (GET callback,
// no nonce / PKCE / form_post).
func New(name string) *Provider {
	return &Provider{
		name:  name,
		caps:  account.ProviderCapabilities{CallbackMethod: "GET"},
		codes: map[string]*account.ProviderIdentity{},
	}
}

// WithCapabilities replaces the static capability declaration.
func (p *Provider) WithCapabilities(caps account.ProviderCapabilities) *Provider {
	p.caps = caps
	return p
}

// WithRedirectURL configures the value the Provider returns from its
// optional account.RedirectURLProvider implementation. Tests use this to
// exercise CookieCarrier dev-mode auto-detect (HTTP-on-localhost flips
// SameSite=Lax + !Secure).
func (p *Provider) WithRedirectURL(u string) *Provider {
	p.redirectURL = u
	return p
}

// WithBeginAuthErr makes BeginAuth return the given error so tests can
// drive handleBegin's rollback path (Save succeeded → Issue or BeginAuth
// failed → roll back the just-saved sid via context.WithoutCancel).
func (p *Provider) WithBeginAuthErr(err error) *Provider {
	p.beginErr = err
	return p
}

// WithCompleteAuthErr makes CompleteAuth return the given error,
// covering the IdP-rejected-our-code branch.
func (p *Provider) WithCompleteAuthErr(err error) *Provider {
	p.completeErr = err
	return p
}

// RedirectURL implements the optional account.RedirectURLProvider so
// Module.RegisterProvider's dev-mode hint pickup is exercised. Empty
// when never configured — Module treats that the same as a non-aware
// provider.
func (p *Provider) RedirectURL() string { return p.redirectURL }

// WithBeginAuthFn lets the test control the redirect URL the provider
// returns. Default behaviour returns a stub URL with the state echoed.
func (p *Provider) WithBeginAuthFn(fn func(req *account.BeginRequest) (*account.BeginResponse, error)) *Provider {
	p.beginFn = fn
	return p
}

// SeedIdentity registers the identity to return when CompleteAuth is
// called with the given code. Use this from tests to pin the IdP
// payload deterministically.
func (p *Provider) SeedIdentity(code string, ident *account.ProviderIdentity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.codes[code] = ident
}

// Name implements AuthProvider.
func (p *Provider) Name() string { return p.name }

// Capabilities implements AuthProvider.
func (p *Provider) Capabilities() account.ProviderCapabilities { return p.caps }

// BeginAuth implements AuthProvider.
func (p *Provider) BeginAuth(_ context.Context, req *account.BeginRequest) (*account.BeginResponse, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	if p.beginFn != nil {
		return p.beginFn(req)
	}
	v := url.Values{}
	v.Set("state", req.State)
	if req.Nonce != "" {
		v.Set("nonce", req.Nonce)
	}
	if req.CodeChallenge != "" {
		v.Set("code_challenge", req.CodeChallenge)
		v.Set("code_challenge_method", "S256")
	}
	return &account.BeginResponse{
		RedirectTo: "https://idp.test/" + p.name + "/authorize?" + v.Encode(),
	}, nil
}

// CompleteAuth implements AuthProvider. Returns the seeded identity for
// the given code; missing code yields ErrUnknownCode so a test of the
// "real IdP rejected our code" branch is straightforward.
func (p *Provider) CompleteAuth(_ context.Context, req *account.CompleteRequest) (*account.ProviderIdentity, error) {
	if p.completeErr != nil {
		return nil, p.completeErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ident, ok := p.codes[req.Code]
	if !ok {
		return nil, ErrUnknownCode
	}
	// Defensive copy so the test can't accidentally observe its seed
	// being mutated by Module.
	cp := *ident
	if cp.Provider == "" {
		cp.Provider = p.name
	}
	if ident.Raw != nil {
		cp.Raw = make(map[string]any, len(ident.Raw))
		for k, v := range ident.Raw {
			cp.Raw[k] = v
		}
	}
	return &cp, nil
}

// ErrUnknownCode is returned by CompleteAuth when the test did not seed
// the code. Equivalent to a real IdP rejecting the exchange.
var ErrUnknownCode = errors.New("testfake: unknown code")

// EncodedState is a small helper for tests that need to scrape the
// state parameter out of the IdP redirect URL.
func EncodedState(rawURL string) string {
	q := strings.SplitN(rawURL, "?", 2)
	if len(q) != 2 {
		return ""
	}
	v, err := url.ParseQuery(q[1])
	if err != nil {
		return ""
	}
	return v.Get("state")
}
