package config

import (
	"fmt"
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

// TestRedact_AccountOptions_MasksProviderSecrets covers the H1
// regression: ProviderRawOptions.Raw is map[string]any holding
// arbitrary provider keys (client_secret, private_key, ...). Pre-fix
// Redact only descended into structs, so secrets in Raw leaked. Now
// the recursion enters maps and a key-name heuristic masks values
// whose key matches one of the well-known secret tokens
// (`*secret*`, `*password*`, `*private_key*`, `*token*`, `*api_key*`).
func TestRedact_AccountOptions_MasksProviderSecrets(t *testing.T) {
	cfg := struct {
		Account AccountOptions `mapstructure:"account"`
	}{
		Account: AccountOptions{
			Enabled:                  true,
			SigningKey:               "this-is-a-test-signing-key-32bytes!",
			OAuthCallbackFrontendURL: "https://app.example.com/auth/finish",
			Providers: map[string]ProviderRawOptions{
				"google": {
					Enabled: true,
					Raw: map[string]any{
						"client_id":     "abc",
						"client_secret": "TOP-SECRET-VALUE",
						"scopes":        []string{"openid"},
					},
				},
				"apple": {
					Enabled: true,
					Raw: map[string]any{
						"service_id":  "com.example.svc",
						"team_id":     "TEAMABC",
						"key_id":      "KEYXYZ",
						"private_key": "-----BEGIN PRIVATE KEY-----\nXXXXX",
					},
				},
			},
		},
	}

	out := Redact(&cfg)
	dump := fmt.Sprintf("%#v", out)

	// Sanity: ordinary values still present.
	for _, want := range []string{"abc", "com.example.svc", "TEAMABC"} {
		if !strings.Contains(dump, want) {
			t.Errorf("non-sensitive value %q lost in redaction", want)
		}
	}

	// Critical: every secret-shaped value must be gone.
	for _, leaked := range []string{
		"TOP-SECRET-VALUE",
		"-----BEGIN PRIVATE KEY-----",
		"this-is-a-test-signing-key-32bytes!",
	} {
		if strings.Contains(dump, leaked) {
			t.Errorf("secret leaked in Redact output: %q\n--full dump--\n%s", leaked, dump)
		}
	}

	// Heuristic-masked keys must still be present (we mask the value,
	// not the key) so an operator reading the redacted output knows
	// what was scrubbed.
	for _, key := range []string{"client_secret", "private_key"} {
		if !strings.Contains(dump, key) {
			t.Errorf("expected key %q to remain in redacted output", key)
		}
	}
}

// TestGoString_AccountOptions_MasksProviderSecrets covers the same
// path through fmt.Sprintf("%#v", account.AccountOptions{...}) — the
// route any "log the whole config" call lands on. AccountOptions.GoString
// must not pass Providers.Raw through verbatim.
func TestGoString_AccountOptions_MasksProviderSecrets(t *testing.T) {
	o := AccountOptions{
		Enabled:    true,
		SigningKey: "this-is-a-test-signing-key-32bytes!",
		Providers: map[string]ProviderRawOptions{
			"google": {Enabled: true, Raw: map[string]any{
				"client_id":     "abc",
				"client_secret": "TOP-SECRET-VALUE",
			}},
		},
	}
	got := fmt.Sprintf("%#v", o)
	if strings.Contains(got, "TOP-SECRET-VALUE") {
		t.Errorf("client_secret leaked in GoString: %s", got)
	}
	if strings.Contains(got, "this-is-a-test-signing-key-32bytes!") {
		t.Errorf("signing_key leaked in GoString: %s", got)
	}
	if !strings.Contains(got, "abc") {
		t.Errorf("client_id (non-sensitive) lost in GoString: %s", got)
	}
}

// TestIsSensitiveKey covers the heuristic. A few tricky cases worth
// pinning so a future "tighten the matcher" doesn't accidentally
// downgrade from "redact known shapes" to "redact only literal
// `secret`".
func TestIsSensitiveKey(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"client_secret", true},
		{"ClientSecret", true},
		{"private_key", true},
		{"PRIVATE_KEY", true},
		{"api_key", true},
		{"apikey", true},
		{"refresh_token", true},
		{"signing_key", true},
		{"password", true},
		{"client_id", false},
		{"scope", false},
		{"redirect_url", false},
		{"team_id", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSensitiveKey(tc.name); got != tc.want {
				t.Fatalf("isSensitiveKey(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
