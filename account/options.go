package account

import (
	"fmt"
	"time"

	"github.com/go-viper/mapstructure/v2"

	"github.com/zynthara/chok/v2/conf"
)

// Options is the "account" yaml section. Every field is restart-only
// (untagged = restart): signing keys, rate-limit windows and provider
// wiring are not safely hot-swappable under live traffic.
type Options struct {
	// Enabled defaults to true — v2 assembly is intent (chok.Use of
	// the module says you want accounts; yaml is the kill switch).
	// Note the flip from v1's default-false.
	Enabled bool `mapstructure:"enabled" default:"true"`

	// SigningKey signs access and reset JWTs (required, ≥ 32 bytes).
	SigningKey string `mapstructure:"signing_key" sensitive:"true"`

	Expiration      time.Duration `mapstructure:"expiration"       default:"2h"`
	ResetExpiration time.Duration `mapstructure:"reset_expiration" default:"15m"`

	// LoginRateWindow / LoginRateLimit enable per-email + per-IP login
	// throttling when both are positive (429 with Retry-After above
	// the threshold). Recommended production values: 15m / 10.
	LoginRateWindow time.Duration `mapstructure:"login_rate_window"`
	LoginRateLimit  int           `mapstructure:"login_rate_limit"`

	// DisableRegister skips the anonymous POST /register route for
	// deployments where admins provision accounts.
	DisableRegister bool `mapstructure:"disable_register"`

	// LinkByEmail enables the OAuth auto-merge path for verified,
	// non-aliased IdP emails matching an existing local user.
	LinkByEmail bool `mapstructure:"link_by_email"`

	// AllowedRedirectBacks is the absolute-URL allowlist for the
	// ?redirect_back parameter (relative paths are always allowed).
	AllowedRedirectBacks []string `mapstructure:"allowed_redirect_backs"`

	// OAuthCallbackFrontendURL is the fixed front-end landing URL the
	// OAuth callback 302s to with the one-shot ?code. Required when
	// any provider is enabled.
	OAuthCallbackFrontendURL string `mapstructure:"oauth_callback_frontend_url"`

	// Providers maps provider name → its yaml block. Each entry needs
	// a matching assembled ProviderSpec (account.WithProviders);
	// enabled: false keeps the block as a kill switch without
	// removing it.
	Providers map[string]ProviderRaw `mapstructure:"providers"`
}

// ProviderRaw is one `account.providers.<name>` block: the enabled
// switch plus every remaining key routed into Raw (mapstructure's
// `,remain`), which ProviderSpec.Build decodes into the provider's own
// typed Options. This keeps the account section independent of the
// provider packages.
type ProviderRaw struct {
	// Enabled is the master switch. The module skips entries with
	// Enabled=false even when a matching spec is assembled.
	Enabled bool `mapstructure:"enabled"`
	// Raw collects every key under the provider entry that isn't
	// `enabled`.
	Raw map[string]any `mapstructure:",remain"`
}

// DecodeProviderConfig converts a provider's raw yaml block into its
// typed Options struct — the helper every ProviderSpec.Build uses:
//
//	var opts google.Options
//	if err := account.DecodeProviderConfig(raw, &opts); err != nil { ... }
//
// The `mapstructure` tags on the target drive the mapping;
// time.Duration and comma-separated slices decode via the same hooks
// the conf loader wires.
func DecodeProviderConfig(raw map[string]any, out any) error {
	cfg := &mapstructure.DecoderConfig{
		Result:           out,
		TagName:          "mapstructure",
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	}
	dec, err := mapstructure.NewDecoder(cfg)
	if err != nil {
		return err
	}
	return dec.Decode(raw)
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if len(o.SigningKey) < 32 {
		return fmt.Errorf("account: signing_key must be at least 32 bytes")
	}
	if o.Expiration < 0 {
		return fmt.Errorf("account: expiration must not be negative")
	}
	if o.ResetExpiration < 0 {
		return fmt.Errorf("account: reset_expiration must not be negative")
	}
	if o.LoginRateWindow < 0 {
		return fmt.Errorf("account: login_rate_window must not be negative")
	}
	if o.LoginRateLimit < 0 {
		return fmt.Errorf("account: login_rate_limit must not be negative")
	}
	// Half-configured limiter is almost certainly an operator mistake
	// (one field set, the other left zero) — refuse rather than silently
	// disabling. Either both > 0 (limiter on) or both == 0 (off).
	if (o.LoginRateWindow > 0) != (o.LoginRateLimit > 0) {
		return fmt.Errorf("account: login_rate_window and login_rate_limit must both be set or both be zero")
	}
	// Any enabled provider requires the front-end landing URL because
	// the callback 302 ends in `?code=…` and the SPA there is the one
	// running /auth/exchange. Without it the OAuth round trip can't
	// complete.
	hasEnabledProvider := false
	for _, p := range o.Providers {
		if p.Enabled {
			hasEnabledProvider = true
			break
		}
	}
	if hasEnabledProvider && o.OAuthCallbackFrontendURL == "" {
		return fmt.Errorf("account: oauth_callback_frontend_url is required when any provider is enabled")
	}
	return nil
}

// Method-less twins (conf.Redact godoc pattern). Provider secrets live
// in Raw maps and are masked by Redact's sensitive-key heuristics
// (client_secret, private_key, ...).
type (
	optionsRaw     Options
	providerRawRaw ProviderRaw
)

// GoString masks the signing key and provider secrets so %#v logging
// cannot leak them.
func (o Options) GoString() string { return fmt.Sprintf("%#v", conf.Redact(optionsRaw(o))) }

// String implements fmt.Stringer with the same redaction as GoString.
func (o Options) String() string { return o.GoString() }

// GoString masks secret-shaped keys inside the raw block.
func (r ProviderRaw) GoString() string { return fmt.Sprintf("%#v", conf.Redact(providerRawRaw(r))) }

// String implements fmt.Stringer with the same redaction as GoString.
func (r ProviderRaw) String() string { return r.GoString() }
