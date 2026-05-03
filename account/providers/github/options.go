// Package github provides chok's GitHub OAuth 2.0 implementation of
// account.AuthProvider.
//
// Wire it via chok.yaml — chok bundles this provider through the
// account/providers/blessed curator, so applications need no Go code:
//
//	account:
//	  enabled: true
//	  oauth_callback_frontend_url: "https://app.example.com/auth/finish"
//	  providers:
//	    github:
//	      enabled: true
//	      client_id:     "${GITHUB_CLIENT_ID}"
//	      client_secret: "${GITHUB_CLIENT_SECRET}"
//	      redirect_url:  "https://app.example.com/auth/github/callback"
//	      enterprise_url: "https://github.company.com"   # optional
//
// Protocol note: GitHub does NOT speak OIDC, so unlike the google
// provider we cannot validate an ID Token. Trust comes from HTTPS
// endpoint pinning + access-token-bound REST calls. The OAuth state
// parameter (Module-generated) is the CSRF defense.
//
// GitHub's `/user` endpoint may return an empty email when the user
// has hidden their primary in privacy settings; we fall back to
// `/user/emails` to find a primary + verified address. Without the
// `user:email` scope (default) /user/emails returns 404, so requests
// MUST keep that scope present.
package github

import (
	"fmt"
	"net/url"
)

// Options is the typed config decoded from chok.yaml's
// account.providers.github block.
type Options struct {
	// Enabled mirrors the kill switch in yaml. When false the factory
	// short-circuits before validating client_id/secret, so an
	// operator can keep `enabled: false` as a staging stanza.
	Enabled bool `mapstructure:"enabled"`

	// ClientID and ClientSecret come from a GitHub OAuth App
	// (Settings → Developer settings → OAuth Apps). Required when
	// Enabled is true. ClientSecret is masked by chok.config.Redact /
	// AccountOptions.GoString through the `*secret*` heuristic.
	ClientID     string `mapstructure:"client_id"`
	ClientSecret string `mapstructure:"client_secret"`

	// RedirectURL must match a callback URL registered against the
	// GitHub OAuth App byte-for-byte (scheme + host + path).
	RedirectURL string `mapstructure:"redirect_url"`

	// Scopes overrides the default set ("read:user", "user:email").
	// `user:email` is required for the email-fallback path against
	// /user/emails when /user.email is hidden. Removing it without
	// understanding the email-flow consequence breaks /login on any
	// account that has hidden its email — be very deliberate.
	Scopes []string `mapstructure:"scopes"`

	// EnterpriseURL points to a self-hosted GitHub Enterprise Server
	// install (e.g. "https://github.company.com"). When set, all
	// auth/token endpoints AND REST API calls are rooted there
	// instead of github.com / api.github.com. Empty = github.com.
	EnterpriseURL string `mapstructure:"enterprise_url"`
}

// Validate enforces the minimum-viable config when the provider is
// enabled. Run pre-construction so a missing client_id surfaces as a
// startup failure, not a runtime auth one.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.ClientID == "" {
		return fmt.Errorf("github.client_id is required")
	}
	if o.ClientSecret == "" {
		return fmt.Errorf("github.client_secret is required")
	}
	if o.RedirectURL == "" {
		return fmt.Errorf("github.redirect_url is required")
	}
	u, err := url.Parse(o.RedirectURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("github.redirect_url must be an absolute URL with scheme and host")
	}
	if o.EnterpriseURL != "" {
		eu, err := url.Parse(o.EnterpriseURL)
		if err != nil || eu.Scheme == "" || eu.Host == "" {
			return fmt.Errorf("github.enterprise_url must be an absolute URL with scheme and host")
		}
	}
	return nil
}

// defaultScopes returns the GitHub OAuth scopes we request by default.
// `user:email` is REQUIRED for the email-fallback path; a deployment
// that drops it must accept that any user with a hidden email will
// fail SPEC §8.1's email gate.
func defaultScopes(supplied []string) []string {
	if len(supplied) > 0 {
		return supplied
	}
	return []string{"read:user", "user:email"}
}

// publicGitHubAPI is the canonical REST API host for github.com.
// Enterprise installs override it via Options.EnterpriseURL +
// "/api/v3".
const publicGitHubAPI = "https://api.github.com"
