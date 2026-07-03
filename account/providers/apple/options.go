// Package apple provides chok's Apple Sign-In implementation of
// account.AuthProvider.
//
// Assemble it explicitly (import decides linkage, yaml stays the
// runtime switch):
//
//	account.Module(account.WithProviders(apple.Provider()))
//
// and configure via chok.yaml:
//
//	account:
//	  enabled: true
//	  oauth_callback_frontend_url: "https://app.example.com/auth/finish"
//	  providers:
//	    apple:
//	      enabled: true
//	      service_id:        "${APPLE_SERVICE_ID}"      # Web client ID, e.g. com.example.web
//	      team_id:           "${APPLE_TEAM_ID}"         # 10-character Team ID
//	      key_id:            "${APPLE_KEY_ID}"          # 10-character Key ID
//	      private_key:       "${APPLE_PRIVATE_KEY_PEM}" # multiline .p8 contents
//	      redirect_url:      "https://app.example.com/auth/apple/callback"
//	      client_secret_ttl: 720h                       # 30 days; Apple max 180d
//
// Apple Sign-In is the most protocol-quirky of the blessed providers:
//
//   - The OAuth client_secret is NOT a static string. It's an ES256
//     JWT signed with the developer's .p8 private key, valid up to
//     6 months. We cache it in the provider and refresh near expiry.
//
//   - The callback is delivered as an HTML form_post, not a query
//     string. SPEC §5 v0.3.4 added Capabilities.RequiresFormPost so
//     Module mounts the route as POST and reads code/state from
//     c.Request.PostForm.
//
//   - User-supplied profile (firstName/lastName) only appears on the
//     FIRST callback after consent, in a `user` form field. Later
//     logins return nothing. Provider parses it from req.FormBody
//     when present and surfaces nil-safe defaults otherwise.
//
//   - The `email` field may be a privaterelay.appleid.com alias when
//     the user opts into "Hide my email". claims.IsPrivateEmail
//     surfaces as ProviderIdentity.IsAliasedEmail so SPEC §8
//     LinkByEmail and §8.1 create-user gate both reject these.
//
//   - Dev-mode is incompatible: form_post is a cross-site POST from
//     appleid.apple.com, and SameSite=Lax (chok's dev-mode sid
//     cookie posture) drops on cross-site POST. Apple debugging must
//     use HTTPS — local-dev tunnels (mkcert/ngrok) are the workflow.
package apple

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Options is the typed config decoded from chok.yaml's
// account.providers.apple block.
type Options struct {
	// ServiceID is the Apple Web Service ID (a Service identifier
	// like com.example.web, distinct from the App ID). Treated as
	// the OAuth client_id. Required.
	ServiceID string `mapstructure:"service_id"`

	// TeamID is the 10-character Apple Developer Team ID. Required.
	TeamID string `mapstructure:"team_id"`

	// KeyID is the 10-character Apple Sign-In Key ID. Required.
	KeyID string `mapstructure:"key_id"`

	// PrivateKey is the PEM-encoded contents of a .p8 ECDSA P-256
	// private key generated under "Sign in with Apple" key
	// configuration in the Apple Developer Portal. Required.
	//
	// Yaml block scalar (`|-`) works for multi-line PEM, but
	// production deployments should pass via env / secret manager
	// rather than baking into chok.yaml.
	PrivateKey string `mapstructure:"private_key"`

	// RedirectURL must match a Return URL registered against the
	// Apple Service ID byte-for-byte. Required.
	RedirectURL string `mapstructure:"redirect_url"`

	// ClientSecretTTL controls how long the dynamically-signed JWT
	// is valid. Apple's upper bound is 6 months (180 days); below 0
	// or zero is rejected because each ES256 sign is non-trivial CPU
	// and disabling cache means every login costs a fresh signature.
	// Recommended: 30d (720h) — short enough to limit blast radius
	// of a stolen secret, long enough to keep signing cost amortised.
	ClientSecretTTL time.Duration `mapstructure:"client_secret_ttl"`

	// IssuerURL overrides the OIDC issuer used for ID Token
	// verification. Production deployments leave it empty (default
	// "https://appleid.apple.com"); tests substitute a httptest
	// server URL via the factory's optional hooks.
	IssuerURL string `mapstructure:"issuer_url"`
}

// Validate enforces every required field structurally. SPEC v0.3.6
// removed the Enabled=false short-circuit so misconfigured Options
// surface at startup regardless of how Apple is wired in (yaml,
// programmatic, etc.).
func (o *Options) Validate() error {
	if o.ServiceID == "" {
		return fmt.Errorf("apple.service_id is required")
	}
	if len(o.TeamID) != 10 {
		return fmt.Errorf("apple.team_id must be exactly 10 chars (got %d)", len(o.TeamID))
	}
	if len(o.KeyID) != 10 {
		return fmt.Errorf("apple.key_id must be exactly 10 chars (got %d)", len(o.KeyID))
	}
	if o.PrivateKey == "" {
		return fmt.Errorf("apple.private_key is required (PEM-encoded .p8 contents)")
	}
	if !strings.Contains(o.PrivateKey, "BEGIN PRIVATE KEY") {
		return fmt.Errorf("apple.private_key must be PEM-encoded (BEGIN PRIVATE KEY marker not found)")
	}
	if o.RedirectURL == "" {
		return fmt.Errorf("apple.redirect_url is required")
	}
	u, err := url.Parse(o.RedirectURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("apple.redirect_url must be an absolute URL with scheme and host")
	}
	if o.ClientSecretTTL <= 0 {
		return fmt.Errorf("apple.client_secret_ttl must be > 0 (recommend 30d)")
	}
	if o.ClientSecretTTL > 180*24*time.Hour {
		return fmt.Errorf("apple.client_secret_ttl must be <= 180 days (Apple upper bound)")
	}
	if o.IssuerURL != "" {
		iu, err := url.Parse(o.IssuerURL)
		if err != nil || iu.Scheme == "" || iu.Host == "" {
			return fmt.Errorf("apple.issuer_url must be an absolute URL with scheme and host")
		}
	}
	return nil
}

// parsePrivateKey decodes the PEM and parses the resulting PKCS8
// payload into an *ecdsa.PrivateKey. Apple .p8 files are always
// PKCS8-wrapped P-256 ECDSA keys; anything else is misconfiguration
// (RSA, raw EC, multiple PEM blocks, etc.) and we reject it loudly.
//
// SPEC §3 v0.3.5 mandates this runs in apple.New(opts) so a bad PEM
// surfaces at chok startup, not at the first /auth/apple/start hit.
func parsePrivateKey(pemContents string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemContents))
	if block == nil {
		return nil, fmt.Errorf("apple: PEM decode failed (no BEGIN block found)")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("apple: PEM block type %q, expected PRIVATE KEY (PKCS8)", block.Type)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apple: parse PKCS8: %w", err)
	}
	ecKey, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("apple: private key is not ECDSA (got %T) — Apple .p8 keys must be P-256", parsed)
	}
	return ecKey, nil
}

// productionIssuer is Apple's stable OIDC issuer URL.
const productionIssuer = "https://appleid.apple.com"

// productionAudience is the audience value Apple expects on the
// client_secret JWT we sign. Constant — not configurable.
const productionAudience = "https://appleid.apple.com"
