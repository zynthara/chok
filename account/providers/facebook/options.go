// Package facebook provides chok's Facebook OAuth 2.0 implementation
// of account.AuthProvider.
//
// Assemble it explicitly (import decides linkage, yaml stays the
// runtime switch):
//
//	account.Module(account.WithProviders(facebook.Provider()))
//
// and configure via chok.yaml:
//
//	account:
//	  enabled: true
//	  oauth_callback_frontend_url: "https://app.example.com/auth/finish"
//	  providers:
//	    facebook:
//	      enabled: true
//	      client_id:     "${FACEBOOK_APP_ID}"
//	      client_secret: "${FACEBOOK_APP_SECRET}"
//	      redirect_url:  "https://app.example.com/auth/facebook/callback"
//	      api_version:   "v18.0"   # optional, default v18.0
//
// Protocol note: Facebook does NOT speak OIDC. Profile data comes
// from a Graph API GET /me request whose `fields=` query enumerates
// what we want back. Email may be empty when the user opts out of
// the email scope at consent time — SPEC §8.1 then refuses the
// create-User path with OAUTH_EMAIL_REQUIRED.
package facebook

import (
	"fmt"
	"net/url"
)

// Options is the typed config decoded from chok.yaml's
// account.providers.facebook block.
type Options struct {
	// ClientID and ClientSecret come from a Facebook App
	// (developers.facebook.com → App → Settings → Basic). Required
	// when Enabled. ClientSecret is masked by chok's Redact /
	// AccountOptions.GoString through the `*secret*` heuristic.
	ClientID     string `mapstructure:"client_id"`
	ClientSecret string `mapstructure:"client_secret"`

	// RedirectURL must match a Valid OAuth Redirect URI configured
	// against the Facebook App's Login product, byte-for-byte.
	RedirectURL string `mapstructure:"redirect_url"`

	// Scopes overrides the default ("email", "public_profile").
	// Without `email` Facebook never returns the email field, and
	// account.ResolveOAuthIdentity will reject the create path with
	// OAUTH_EMAIL_REQUIRED. Drop it only if your deployment has an
	// alternate identity-merge story already wired.
	Scopes []string `mapstructure:"scopes"`

	// APIVersion pins the Graph API version. Facebook deprecates
	// versions on a ~2-year cadence; we default to a recent stable
	// version and let operators bump it via yaml when they're ready
	// to verify.
	APIVersion string `mapstructure:"api_version"`
}

// Validate enforces the minimum-viable config.
func (o *Options) Validate() error {
	if o.ClientID == "" {
		return fmt.Errorf("facebook.client_id is required")
	}
	if o.ClientSecret == "" {
		return fmt.Errorf("facebook.client_secret is required")
	}
	if o.RedirectURL == "" {
		return fmt.Errorf("facebook.redirect_url is required")
	}
	u, err := url.Parse(o.RedirectURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("facebook.redirect_url must be an absolute URL with scheme and host")
	}
	return nil
}

// defaultScopes returns the consent scopes we request by default.
// `email` is what flips ProviderIdentity.Email from blank to a real
// value at /me time; `public_profile` is the documented baseline.
func defaultScopes(supplied []string) []string {
	if len(supplied) > 0 {
		return supplied
	}
	return []string{"email", "public_profile"}
}

// defaultAPIVersion is the Graph API version we use when Options
// leaves it blank. Bumped roughly yearly as Facebook deprecates older
// majors. A non-empty Options.APIVersion always takes precedence.
const defaultAPIVersion = "v18.0"

// publicGraphAPI is the standard Graph API host. Tests substitute a
// httptest.Server URL via the apiBaseOverride hook on New().
const publicGraphAPI = "https://graph.facebook.com"
