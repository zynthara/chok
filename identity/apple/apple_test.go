package apple_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/identity/apple"
	"github.com/zynthara/chok/v3/identity/internal/idptest"
	"github.com/zynthara/chok/v3/identity/internal/oauth"
)

func p8(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func ecKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func config(t *testing.T, idp *idptest.IdP, key *ecdsa.PrivateKey) identity.ProviderConfig {
	t.Helper()
	pc := identity.ProviderConfig{
		ClientID: "com.example.web", ClientSecret: p8(t, key), TeamID: "TEAM123456", KeyID: "KEY1234567",
		RedirectURL: "https://app.example.test/auth/apple/callback",
	}
	if idp != nil {
		pc.Endpoint = idp.URL()
	}
	return pc
}

func provider(t *testing.T, pc identity.ProviderConfig) identity.Provider {
	t.Helper()
	p := apple.New(pc)
	if err := p.Check(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNew_RefusesBadConfig(t *testing.T) {
	ok := config(t, nil, ecKey(t))
	if err := apple.New(ok).Check(); err != nil {
		t.Fatalf("a full config: %v", err)
	}
	for name, c := range map[string]struct {
		mutate func(*identity.ProviderConfig)
		want   string
	}{
		"no client_id":     {func(pc *identity.ProviderConfig) { pc.ClientID = "" }, "client_id"},
		"a short team_id":  {func(pc *identity.ProviderConfig) { pc.TeamID = "short" }, "team_id"},
		"a short key_id":   {func(pc *identity.ProviderConfig) { pc.KeyID = "x" }, "key_id"},
		"no key":           {func(pc *identity.ProviderConfig) { pc.ClientSecret = "" }, "client_secret"},
		"not a PEM":        {func(pc *identity.ProviderConfig) { pc.ClientSecret = "not a pem" }, "PEM-encoded"},
		"no redirect_url":  {func(pc *identity.ProviderConfig) { pc.RedirectURL = "" }, "redirect_url"},
		"relative":         {func(pc *identity.ProviderConfig) { pc.RedirectURL = "/cb" }, "redirect_url"},
		"a bad endpoint":   {func(pc *identity.ProviderConfig) { pc.Endpoint = "issuer" }, "endpoint"},
		"google's setting": {func(pc *identity.ProviderConfig) { pc.HostedDomain = "example.com" }, "hosted_domain"},
	} {
		pc := ok
		c.mutate(&pc)
		if err := apple.New(pc).Check(); err == nil || !strings.HasPrefix(err.Error(), "apple: ") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestNew_RejectsRSAKey(t *testing.T) {
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pc := config(t, nil, ecKey(t))
	pc.ClientSecret = p8(t, rk)
	if err := apple.New(pc).Check(); err == nil || !strings.Contains(err.Error(), "ECDSA") {
		t.Fatalf("an RSA key: %v", err)
	}
}

func TestNew_RejectsCorruptPEM(t *testing.T) {
	pc := config(t, nil, ecKey(t))
	pc.ClientSecret = "-----BEGIN PRIVATE KEY-----\nXXXNOT-VALID-BASE64XXX\n-----END PRIVATE KEY-----\n"
	if err := apple.New(pc).Check(); err == nil {
		t.Fatal("a corrupt PEM passed")
	}
}

func TestCapabilities_FormPostNonceOnly(t *testing.T) {
	p := provider(t, config(t, nil, ecKey(t)))
	c := p.Capabilities()
	if p.Name() != "apple" || c.CallbackMethod != http.MethodPost || !c.RequiresFormPost || !c.RequiresNonce || c.SupportsPKCE {
		t.Fatalf("%s %+v", p.Name(), c)
	}
}

func TestProvider_FromLoadedConfig(t *testing.T) {
	type app struct {
		chok.Base
		Auth identity.Config `yaml:"auth"`
	}
	key := p8(t, ecKey(t))
	dir := t.TempDir()
	yaml := "auth:\n  oauth_callback_frontend_url: https://app.example.test/finish\n  providers:\n    apple:\n" +
		"      client_id: com.example.web\n      team_id: TEAM123456\n      key_id: KEY1234567\n" +
		"      redirect_url: https://api.example.test/auth/apple/callback\n      client_secret: placeholder\n"
	if err := os.WriteFile(filepath.Join(dir, "chok.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATEST_CONFIG", filepath.Join(dir, "chok.yaml"))
	t.Setenv("ATEST_AUTH_SIGNING_KEY", strings.Repeat("k", 32))
	// The key is a secret: it comes from the environment, the file only
	// names the provider.
	t.Setenv("ATEST_AUTH_PROVIDERS_APPLE_CLIENT_SECRET", key)
	cfg, err := chok.Load[app]("atest", chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	if pc := cfg.Auth.Providers["apple"]; pc.ClientSecret != key || apple.New(pc).Check() != nil {
		t.Fatalf("%+v", pc)
	}
}

func TestBeginAuth_RequestsFormPostAndNonce(t *testing.T) {
	idp := idptest.New(t)
	raw, err := provider(t, config(t, idp, ecKey(t))).BeginAuth(context.Background(), &oauth.BeginRequest{State: "state-1", Nonce: "nonce-1"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	if q.Get("response_mode") != "form_post" || q.Get("nonce") != "nonce-1" || q.Get("state") != "state-1" || q.Get("client_id") != "com.example.web" ||
		!strings.Contains(q.Get("scope"), "name") || !strings.Contains(q.Get("scope"), "email") {
		t.Fatalf("authorize URL: %s", raw)
	}
}

func TestCompleteAuth_HappyPath(t *testing.T) {
	idp := idptest.New(t)
	key := ecKey(t)
	idp.Issue("code-1", map[string]any{"sub": "apple-sub-001", "email": "alice@icloud.com", "email_verified": "true", "is_private_email": "false", "nonce": "nonce-1"})
	ext, err := provider(t, config(t, idp, key)).CompleteAuth(context.Background(), &oauth.CompleteRequest{
		Code: "code-1", Nonce: "nonce-1", FormBody: url.Values{"user": {`{"name":{"firstName":"Alice","lastName":"Iverson"}}`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ext.ProviderAccountID != "apple-sub-001" || ext.Email != "alice@icloud.com" || !ext.EmailVerified || ext.IsAliasedEmail || ext.Name != "Alice Iverson" {
		t.Fatalf("%+v", ext)
	}
	// The client secret is an ES256 JWT of the .p8 key, for this client.
	forms := idp.Forms()
	if len(forms) != 1 || forms[0].Get("client_id") != "com.example.web" {
		t.Fatalf("the token request: %v", forms)
	}
	claims := jwt.MapClaims{}
	tok, err := jwt.ParseWithClaims(forms[0].Get("client_secret"), claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
		jwt.WithValidMethods([]string{"ES256"}), jwt.WithAudience("https://appleid.apple.com"), jwt.WithIssuer("TEAM123456"), jwt.WithSubject("com.example.web"))
	if err != nil || tok.Header["kid"] != "KEY1234567" {
		t.Fatalf("the client secret: %v %v", err, tok)
	}
}

func TestCompleteAuth_PrivateRelayMapsToAliased(t *testing.T) {
	idp := idptest.New(t)
	idp.Issue("code-2", map[string]any{"sub": "apple-sub-002", "email": "abc123@privaterelay.appleid.com", "email_verified": "true", "is_private_email": "true", "nonce": "n"})
	ext, err := provider(t, config(t, idp, ecKey(t))).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-2", Nonce: "n"})
	if err != nil || !ext.IsAliasedEmail || !ext.EmailVerified {
		t.Fatalf("a relay address is verified but aliased: %+v %v", ext, err)
	}
}

func TestCompleteAuth_BoolEmailVerified(t *testing.T) {
	idp := idptest.New(t)
	idp.Issue("code-bool", map[string]any{"sub": "apple-sub-003", "email": "x@example.com", "email_verified": true, "is_private_email": false})
	ext, err := provider(t, config(t, idp, ecKey(t))).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-bool"})
	if err != nil || !ext.EmailVerified || ext.IsAliasedEmail {
		t.Fatalf("boolean claims: %+v %v", ext, err)
	}
}

func TestCompleteAuth_SecondLoginNoUserField(t *testing.T) {
	idp := idptest.New(t)
	idp.Issue("code-3", map[string]any{"sub": "apple-sub-004", "email": "x@example.com", "email_verified": "true"})
	for _, form := range []url.Values{{}, {"user": {"{not json"}}} {
		ext, err := provider(t, config(t, idp, ecKey(t))).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-3", FormBody: form})
		if err != nil || ext.Name != "" || ext.ProviderAccountID != "apple-sub-004" {
			t.Fatalf("%v: %+v %v", form, ext, err)
		}
	}
}

func TestCompleteAuth_RejectsNonceMismatch(t *testing.T) {
	idp := idptest.New(t)
	idp.Issue("code-4", map[string]any{"sub": "x", "email": "x@example.com", "email_verified": "true", "nonce": "issued-nonce"})
	_, err := provider(t, config(t, idp, ecKey(t))).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-4", Nonce: "another"})
	if err == nil || !strings.Contains(err.Error(), "nonce mismatch") {
		t.Fatalf("a nonce mismatch: %v", err)
	}
}

func TestCompleteAuth_TokenExchangeFailure(t *testing.T) {
	idp := idptest.New(t)
	_, err := provider(t, config(t, idp, ecKey(t))).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "no-code"})
	if err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Fatalf("an unknown code: %v", err)
	}
}

func TestClientSecret_ConcurrentSignsOnce(t *testing.T) {
	idp := idptest.New(t)
	p := provider(t, config(t, idp, ecKey(t)))
	idp.Issue("code", map[string]any{"sub": "x", "email": "x@example.com", "email_verified": "true"})
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if _, err := p.CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	secrets := map[string]bool{}
	for _, f := range idp.Forms() {
		secrets[f.Get("client_secret")] = true
	}
	if len(secrets) != 1 {
		t.Fatalf("%d distinct client secrets for 50 sign-ins: it is signed once and reused", len(secrets))
	}
}
