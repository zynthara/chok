package config

import (
	"strings"
	"testing"
	"time"
)

func TestAccountOptions_Validate(t *testing.T) {
	const goodKey = "this-is-a-test-signing-key-32bytes!"

	cases := []struct {
		name    string
		opts    AccountOptions
		wantErr string // substring; empty = no error expected
	}{
		{
			name: "disabled bypasses validation",
			opts: AccountOptions{Enabled: false},
		},
		{
			name:    "short signing key",
			opts:    AccountOptions{Enabled: true, SigningKey: "tooshort"},
			wantErr: "signing_key",
		},
		{
			name:    "negative expiration",
			opts:    AccountOptions{Enabled: true, SigningKey: goodKey, Expiration: -time.Second},
			wantErr: "expiration",
		},
		{
			name:    "half-configured rate limit (window only)",
			opts:    AccountOptions{Enabled: true, SigningKey: goodKey, LoginRateWindow: time.Minute},
			wantErr: "login_rate_window and login_rate_limit",
		},
		{
			name: "ok minimal",
			opts: AccountOptions{Enabled: true, SigningKey: goodKey},
		},
		// Phase 3 additions —————————————————————————————————————————
		{
			name: "enabled provider without frontend URL fails",
			opts: AccountOptions{
				Enabled:    true,
				SigningKey: goodKey,
				Providers: map[string]ProviderRawOptions{
					"google": {Enabled: true},
				},
			},
			wantErr: "oauth_callback_frontend_url is required",
		},
		{
			name: "all-disabled providers don't require frontend URL",
			opts: AccountOptions{
				Enabled:    true,
				SigningKey: goodKey,
				Providers: map[string]ProviderRawOptions{
					"google": {Enabled: false},
					"apple":  {Enabled: false},
				},
			},
		},
		{
			name: "enabled provider with frontend URL ok",
			opts: AccountOptions{
				Enabled:                  true,
				SigningKey:               goodKey,
				OAuthCallbackFrontendURL: "https://app.example.com/auth/finish",
				Providers: map[string]ProviderRawOptions{
					"google": {Enabled: true},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected ok, got err: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestProviderRawOptions_Decode covers the provider-side path: yaml
// decodes into ProviderRawOptions{Enabled, Raw}, and provider packages
// then call Decode to extract a typed Options struct.
func TestProviderRawOptions_Decode(t *testing.T) {
	type googleOpts struct {
		ClientID     string        `mapstructure:"client_id"`
		ClientSecret string        `mapstructure:"client_secret"`
		Timeout      time.Duration `mapstructure:"timeout"`
		Scopes       []string      `mapstructure:"scopes"`
	}

	raw := &ProviderRawOptions{
		Enabled: true,
		Raw: map[string]any{
			"client_id":     "my-client",
			"client_secret": "shh",
			"timeout":       "5s",
			"scopes":        []any{"openid", "email"},
		},
	}
	var got googleOpts
	if err := raw.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "my-client" || got.ClientSecret != "shh" {
		t.Fatalf("decoded basic fields wrong: %+v", got)
	}
	if got.Timeout != 5*time.Second {
		t.Fatalf("decoded duration wrong: %v", got.Timeout)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "openid" {
		t.Fatalf("decoded slice wrong: %v", got.Scopes)
	}
}

// TestProviderRawOptions_Decode_Nil ensures the receiver guard works —
// callers that try to Decode on a zero-value pointer get a clear error
// instead of a panic.
func TestProviderRawOptions_Decode_Nil(t *testing.T) {
	var raw *ProviderRawOptions
	var dst struct{}
	if err := raw.Decode(&dst); err == nil {
		t.Fatal("expected error for nil receiver")
	}
}
