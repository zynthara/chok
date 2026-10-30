package google_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/identity"
	"github.com/zynthara/chok/identity/google"
	"github.com/zynthara/chok/identity/internal/idptest"
	"github.com/zynthara/chok/identity/internal/oauth"
)

func config(idp *idptest.IdP) identity.ProviderConfig {
	return identity.ProviderConfig{
		ClientID: "client-abc", ClientSecret: "mock-secret",
		RedirectURL: "https://app.example.test/auth/google/callback", Endpoint: idp.URL(),
	}
}

func provider(t *testing.T, pc identity.ProviderConfig) identity.Provider {
	t.Helper()
	p := google.New(pc)
	if err := p.Check(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNew_RefusesBadConfig(t *testing.T) {
	ok := identity.ProviderConfig{ClientID: "x", ClientSecret: "x", RedirectURL: "https://a.test/cb"}
	if err := google.New(ok).Check(); err != nil {
		t.Fatalf("the minimal config: %v", err)
	}
	cases := map[string]func(*identity.ProviderConfig){
		"no client_id":        func(pc *identity.ProviderConfig) { pc.ClientID = "" },
		"no client_secret":    func(pc *identity.ProviderConfig) { pc.ClientSecret = "" },
		"no redirect_url":     func(pc *identity.ProviderConfig) { pc.RedirectURL = "" },
		"a relative redirect": func(pc *identity.ProviderConfig) { pc.RedirectURL = "/cb" },
		"a bad endpoint":      func(pc *identity.ProviderConfig) { pc.Endpoint = "not-a-url" },
		"apple's team_id":     func(pc *identity.ProviderConfig) { pc.TeamID = "1234567890" },
		"apple's key_id":      func(pc *identity.ProviderConfig) { pc.KeyID = "1234567890" },
	}
	for name, mutate := range cases {
		pc := ok
		mutate(&pc)
		p := google.New(pc)
		if err := p.Check(); err == nil || !strings.HasPrefix(err.Error(), "google: ") {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := p.BeginAuth(context.Background(), &oauth.BeginRequest{}); err == nil {
			t.Errorf("%s: a refused provider must not begin a sign-in", name)
		}
	}
}

func TestProvider_NameAndCapabilities(t *testing.T) {
	p := provider(t, identity.ProviderConfig{ClientID: "x", ClientSecret: "x", RedirectURL: "https://a.test/cb"})
	c := p.Capabilities()
	if p.Name() != "google" || c.CallbackMethod != http.MethodGet || !c.RequiresNonce || !c.SupportsPKCE || c.RequiresFormPost || c.RedirectURL != "https://a.test/cb" {
		t.Fatalf("%s %+v", p.Name(), c)
	}
}

func TestProvider_FromLoadedConfig(t *testing.T) {
	type app struct {
		chok.Base
		Auth identity.Config `yaml:"auth"`
	}
	t.Setenv("GTEST_AUTH_SIGNING_KEY", strings.Repeat("k", 32))
	t.Setenv("GTEST_CONFIG", writeYAML(t, `
auth:
  oauth_callback_frontend_url: https://app.example.test/finish
  providers:
    google:
      client_id: 1234.apps.googleusercontent.com
      client_secret: shh
      redirect_url: https://api.example.test/auth/google/callback
      hosted_domain: example.test
`))
	cfg, err := chok.Load[app]("gtest", chok.Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	pc := cfg.Auth.Providers["google"]
	if pc.ClientID != "1234.apps.googleusercontent.com" || pc.HostedDomain != "example.test" {
		t.Fatalf("%+v", pc)
	}
	if err := google.New(pc).Check(); err != nil {
		t.Fatal(err)
	}
}

func TestBeginAuth_BuildsAuthorizeURL(t *testing.T) {
	idp := idptest.New(t)
	p := provider(t, config(idp))
	raw, err := p.BeginAuth(context.Background(), &oauth.BeginRequest{State: "state-1", Nonce: "nonce-1", CodeChallenge: "challenge-1"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	if u.Path != idptest.AuthorizePath || q.Get("client_id") != "client-abc" || q.Get("state") != "state-1" || q.Get("nonce") != "nonce-1" ||
		q.Get("code_challenge") != "challenge-1" || q.Get("code_challenge_method") != "S256" || q.Get("access_type") != "online" {
		t.Fatalf("authorize URL: %s", raw)
	}
	for _, scope := range []string{"openid", "email", "profile"} {
		if !strings.Contains(q.Get("scope"), scope) {
			t.Errorf("scope %q lacks %s", q.Get("scope"), scope)
		}
	}
	if q.Get("hd") != "" {
		t.Fatal("no hosted domain unless configured")
	}
}

func TestBeginAuth_HostedDomainParam(t *testing.T) {
	idp := idptest.New(t)
	pc := config(idp)
	pc.HostedDomain = "example.com"
	raw, err := provider(t, pc).BeginAuth(context.Background(), &oauth.BeginRequest{State: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := url.Parse(raw); u.Query().Get("hd") != "example.com" {
		t.Fatalf("hd: %s", raw)
	}
}

func TestCompleteAuth_HappyPath(t *testing.T) {
	idp := idptest.New(t)
	p := provider(t, config(idp))
	idp.Issue("code-1", map[string]any{
		"sub": "google-sub-99", "email": "alice@example.com", "email_verified": true, "name": "Alice",
		"picture": "https://lh.example/alice.jpg", "nonce": "nonce-1", "locale": "en",
	})
	ext, err := p.CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-1", Nonce: "nonce-1", CodeVerifier: "verifier-1"})
	if err != nil {
		t.Fatal(err)
	}
	if ext.Provider != "google" || ext.ProviderAccountID != "google-sub-99" || ext.Email != "alice@example.com" || !ext.EmailVerified ||
		ext.Name != "Alice" || ext.Picture == "" || ext.IsAliasedEmail || ext.Raw["locale"] != "en" {
		t.Fatalf("%+v", ext)
	}
	forms := idp.Forms()
	if len(forms) != 1 || forms[0].Get("code_verifier") != "verifier-1" || forms[0].Get("redirect_uri") != "https://app.example.test/auth/google/callback" {
		t.Fatalf("the token request: %v", forms)
	}
}

func TestCompleteAuth_RejectsNonceMismatch(t *testing.T) {
	idp := idptest.New(t)
	idp.Issue("code-2", map[string]any{"sub": "x", "email": "x@example.com", "email_verified": true, "nonce": "issued-nonce"})
	_, err := provider(t, config(idp)).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-2", Nonce: "another-nonce"})
	if err == nil || !strings.Contains(err.Error(), "nonce mismatch") {
		t.Fatalf("a nonce mismatch: %v", err)
	}
}

func TestCompleteAuth_RejectsWrongAudience(t *testing.T) {
	idp := idptest.New(t)
	idp.Issue("code-3", map[string]any{"sub": "x", "email": "x@example.com", "email_verified": true, "aud": "different-client"})
	_, err := provider(t, config(idp)).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-3"})
	if err == nil || !strings.Contains(err.Error(), "id_token verify") {
		t.Fatalf("another client's token: %v", err)
	}
}

func TestCompleteAuth_RejectsWrongSignature(t *testing.T) {
	idp := idptest.New(t)
	forged := idp.Sign(t, idp.Complete(map[string]any{"sub": "x", "email": "x@example.com", "email_verified": true}, "client-abc"), true)
	idp.ServeToken(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"x","token_type":"Bearer","expires_in":3600,"id_token":"` + forged + `"}`))
	})
	_, err := provider(t, config(idp)).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "any"})
	if err == nil || !strings.Contains(err.Error(), "id_token verify") {
		t.Fatalf("a forged token: %v", err)
	}
}

func TestCompleteAuth_RejectsHostedDomainMismatch(t *testing.T) {
	idp := idptest.New(t)
	pc := config(idp)
	pc.HostedDomain = "company.com"
	idp.Issue("code-4", map[string]any{"sub": "x", "email": "x@othercorp.com", "email_verified": true, "hd": "othercorp.com"})
	idp.Issue("code-5", map[string]any{"sub": "y", "email": "y@gmail.com", "email_verified": true})
	for _, code := range []string{"code-4", "code-5"} {
		_, err := provider(t, pc).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: code})
		if err == nil || !strings.Contains(err.Error(), "hosted domain") {
			t.Fatalf("%s: %v", code, err)
		}
	}
}

func TestCompleteAuth_HostedDomainAccepted(t *testing.T) {
	idp := idptest.New(t)
	pc := config(idp)
	pc.HostedDomain = "company.com"
	idp.Issue("code-6", map[string]any{"sub": "x", "email": "alice@company.com", "email_verified": true, "hd": "company.com"})
	ext, err := provider(t, pc).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "code-6"})
	if err != nil || ext.Raw["hosted_domain"] != "company.com" {
		t.Fatalf("%+v %v", ext, err)
	}
}

func TestCompleteAuth_TokenExchangeFailure(t *testing.T) {
	idp := idptest.New(t)
	_, err := provider(t, config(idp)).CompleteAuth(context.Background(), &oauth.CompleteRequest{Code: "unknown"})
	if err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Fatalf("an unknown code: %v", err)
	}
	// An issuer that cannot be discovered fails the sign-in, not New.
	pc := config(idp)
	pc.Endpoint = "http://127.0.0.1:1"
	p := provider(t, pc)
	if _, err := p.BeginAuth(context.Background(), &oauth.BeginRequest{State: "s"}); err == nil || !strings.Contains(err.Error(), "discovery") {
		t.Fatalf("an unreachable issuer: %v", err)
	}
}
