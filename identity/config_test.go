package identity

import (
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/internal/dbtest"
)

func TestConfig_Validate(t *testing.T) {
	ok := Config{SigningKey: testKey}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a key is all a config needs: %v", err)
	}
	cases := map[string]struct {
		cfg  Config
		want string
	}{
		"a short key":            {Config{SigningKey: "short"}, "signing_key must be at least 32 bytes"},
		"no key":                 {Config{}, "signing_key"},
		"half a throttle":        {Config{SigningKey: testKey, LoginRateWindow: 15 * time.Minute}, "both set or both 0"},
		"the other half":         {Config{SigningKey: testKey, LoginRateLimit: 5}, "both set or both 0"},
		"providers, no frontend": {Config{SigningKey: testKey, Providers: map[string]ProviderConfig{"google": {}}}, "oauth_callback_frontend_url is required"},
		"a relative frontend":    {Config{SigningKey: testKey, OAuthCallbackFrontendURL: "/finish"}, "oauth_callback_frontend_url must be an absolute URL"},
		"a bad provider name":    {Config{SigningKey: testKey, OAuthCallbackFrontendURL: "https://a.test/f", Providers: map[string]ProviderConfig{"Google!": {}}}, "lower-case name"},
		"a trailing slash":       {Config{SigningKey: testKey, Prefix: "/auth/"}, "prefix"},
		"a relative prefix":      {Config{SigningKey: testKey, Prefix: "auth"}, "prefix"},
		"a negative ttl":         {Config{SigningKey: testKey, TokenTTL: -time.Second}, "token_ttl must not be negative"},
		"a bad redirect entry":   {Config{SigningKey: testKey, AllowedRedirectBacks: []string{"ftp://x.test/"}}, "allowed_redirect_backs"},
		"a negative retention":   {Config{SigningKey: testKey, Audit: AuditConfig{Retention: -time.Hour}}, "audit.retention"},
	}
	for name, c := range cases {
		err := c.cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
}

func TestConfig_LoadsWithDefaults(t *testing.T) {
	type appConfig struct {
		chok.Base
		Auth Config `yaml:"auth"`
	}
	t.Setenv("IDT_AUTH_SIGNING_KEY", testKey)
	cfg, err := chok.Load[appConfig]("idt", chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Auth
	if a.Prefix != "/auth" || a.TokenTTL != 2*time.Hour || a.RefreshTTL != 168*time.Hour || a.ResetTTL != 15*time.Minute ||
		a.LoginRateWindow != 15*time.Minute || a.LoginRateLimit != 10 || a.SigningKey != testKey {
		t.Fatalf("defaults: %+v", a)
	}
	if a.Audit.Enabled || a.Audit.Retention != 180*24*time.Hour || a.Audit.AsyncBuffer != 1024 || a.Audit.DropOnFull || !a.Audit.AdminAPI {
		t.Fatalf("audit defaults: %+v", a.Audit)
	}
	// The key is a secret: missing, the load fails and names it.
	t.Setenv("IDT_AUTH_SIGNING_KEY", "")
	if _, err := chok.Load[appConfig]("idt", chok.Args(nil)); err == nil || !strings.Contains(err.Error(), "signing_key") {
		t.Fatalf("a missing key: %v", err)
	}
}

func TestNew_DefaultsZeroFields(t *testing.T) {
	idn := newIdentity(t, Config{})
	c := idn.cfg
	if c.Prefix != "/auth" || c.TokenTTL != 2*time.Hour || c.RefreshTTL != 7*24*time.Hour || c.ResetTTL != 15*time.Minute || c.Audit.AsyncBuffer != 1024 {
		t.Fatalf("defaults in code: %+v", c)
	}
	if idn.limiter != nil {
		t.Fatal("a Config built in code has the throttle off unless it sets one")
	}
	if idn.Name() != "identity" || idn.Users() == nil || idn.Audit() == nil || idn.Audit().Enabled() {
		t.Fatal("the accessors")
	}
}

func TestNew_RefusesBadConfig(t *testing.T) {
	d := dbtest.Open(t)
	if _, err := New(Config{SigningKey: "too-short"}, d); err == nil || !strings.Contains(err.Error(), "signing_key") {
		t.Fatalf("a short key fails New and names it: %v", err)
	}
	if _, err := New(Config{SigningKey: testKey}, nil); err == nil {
		t.Fatal("a nil database fails New")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustNew panics on a bad config")
		}
	}()
	MustNew(Config{}, d)
}
