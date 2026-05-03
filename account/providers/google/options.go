// Package google provides chok's Google OAuth 2.0 + OpenID Connect
// implementation of account.AuthProvider.
//
// Wire it via chok.yaml — chok bundles this provider through the
// account/providers/blessed curator, so applications need no Go code:
//
//	account:
//	  enabled: true
//	  oauth_callback_frontend_url: "https://app.example.com/auth/finish"
//	  providers:
//	    google:
//	      enabled: true
//	      client_id:     "${GOOGLE_CLIENT_ID}"
//	      client_secret: "${GOOGLE_CLIENT_SECRET}"
//	      redirect_url:  "https://app.example.com/auth/google/callback"
//	      hosted_domain: "example.com"   # optional G Suite restriction
//
// The package's init() registers a factory keyed "google" against
// account.providerRegistry; Module.RegisterConfiguredProviders looks
// it up and invokes it with the operator's ProviderRawOptions.
//
// Protocol choice: we use the OIDC ID Token rather than a userinfo
// roundtrip — Google ships a verified JWT with email, name, picture
// in the token exchange response, saving an HTTP call and giving us
// authenticated claims (signature + iss + aud + exp) for free.
package google

import (
	"fmt"
	"net/url"
)

// Options is the typed config decoded from yaml's
// account.providers.google block. ProviderRawOptions.Decode populates
// it through mapstructure tags.
type Options struct {
	// Enabled mirrors the kill switch in yaml. When false the factory
	// short-circuits before validating client_id / secret, so an
	// operator can keep an "enabled: false" stanza in chok.yaml as a
	// staging area for credentials they aren't ready to deploy.
	Enabled bool `mapstructure:"enabled"`

	// ClientID and ClientSecret come from a Google Cloud Console OAuth
	// client. They are required when Enabled is true. ClientSecret is
	// flagged sensitive: chok's config.Redact / GoString helpers mask
	// it via the heuristic key match (`*secret*`).
	ClientID     string `mapstructure:"client_id"`
	ClientSecret string `mapstructure:"client_secret"`

	// RedirectURL must match a redirect URI registered against the
	// Google Console OAuth client byte-for-byte (scheme + host + path
	// + trailing slash). Mismatched entries fail at the IdP with a
	// generic redirect_uri_mismatch error.
	RedirectURL string `mapstructure:"redirect_url"`

	// Scopes overrides the default set of three OIDC scopes
	// ("openid", "email", "profile"). Almost no deployment needs to
	// change this — the defaults map onto every field
	// account.ProviderIdentity exposes. Override only to widen access
	// (e.g. drive.readonly for a Workspace add-on).
	Scopes []string `mapstructure:"scopes"`

	// HostedDomain restricts logins to a single G Suite / Google
	// Workspace tenant. Pass "company.com" to refuse any account
	// whose `hd` claim isn't an exact match. Empty (the default)
	// allows any Google account, including consumer @gmail.com.
	HostedDomain string `mapstructure:"hosted_domain"`

	// IssuerURL overrides the OIDC issuer used for ID Token
	// verification. Production deployments leave this empty (we use
	// "https://accounts.google.com"); tests and staging environments
	// substitute a httptest.Server URL via the factory's optional
	// hooks.
	IssuerURL string `mapstructure:"issuer_url"`
}

// Validate enforces the minimum-viable config when the provider is
// enabled. Pre-construction checks beat surfacing the same errors at
// /auth/google/start time — operators see fail-fast at startup.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.ClientID == "" {
		return fmt.Errorf("google.client_id is required")
	}
	if o.ClientSecret == "" {
		return fmt.Errorf("google.client_secret is required")
	}
	if o.RedirectURL == "" {
		return fmt.Errorf("google.redirect_url is required")
	}
	u, err := url.Parse(o.RedirectURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("google.redirect_url must be an absolute URL with scheme and host")
	}
	if o.IssuerURL != "" {
		iu, err := url.Parse(o.IssuerURL)
		if err != nil || iu.Scheme == "" || iu.Host == "" {
			return fmt.Errorf("google.issuer_url must be an absolute URL with scheme and host")
		}
	}
	return nil
}

// defaultScopes returns the OIDC standard set, used when Options.Scopes
// is empty. "openid" must always be present for Google to return an
// id_token alongside the access token.
func defaultScopes(supplied []string) []string {
	if len(supplied) > 0 {
		return supplied
	}
	return []string{"openid", "email", "profile"}
}

// productionIssuer is Google's stable OIDC issuer URL. We pin it as a
// constant rather than letting Options default to it because tests
// substitute a different issuer — Options.IssuerURL is the override
// and a non-empty string takes precedence.
const productionIssuer = "https://accounts.google.com"
