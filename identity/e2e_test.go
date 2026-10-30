package identity_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/choktest"
	"github.com/zynthara/chok/identity"
	"github.com/zynthara/chok/identity/google"
	"github.com/zynthara/chok/identity/internal/idptest"
	"github.com/zynthara/chok/internal/dbtest"
)

// TestGoogle_SignInEndToEnd runs a real provider package through
// identity's routes, against an IdP in process: start, the provider's
// callback, the exchange, then the token at work.
func TestGoogle_SignInEndToEnd(t *testing.T) {
	idp := idptest.New(t)
	cfg := identity.Config{
		SigningKey:               strings.Repeat("k", 32),
		OAuthCallbackFrontendURL: "https://app.example.test/finish",
		Providers: map[string]identity.ProviderConfig{"google": {
			ClientID: "client-abc", ClientSecret: "shh", Endpoint: idp.URL(),
			RedirectURL: "https://api.example.test/auth/google/callback",
		}},
	}
	auth := identity.MustNew(cfg, dbtest.Open(t), identity.Providers(google.New(cfg.Providers["google"])))
	c := choktest.App(t, chok.Base{}, func(app *chok.App) { app.Use(auth) })

	start := c.Do(httptest.NewRequest(http.MethodGet, "/auth/google/start?redirect_back=/home", nil))
	loc, _ := url.Parse(start.Header.Get("Location"))
	if start.StatusCode != http.StatusFound || !strings.HasPrefix(loc.String(), idp.URL()+idptest.AuthorizePath) {
		t.Fatalf("start = %d %s", start.StatusCode, loc)
	}
	// The IdP signs in the user and sends the browser back with a code;
	// the ID token answers the nonce the start sent.
	idp.Issue("code-1", map[string]any{"sub": "g-1", "email": "alice@gmail.test", "email_verified": true, "name": "Alice", "nonce": loc.Query().Get("nonce")})
	req := httptest.NewRequest(http.MethodGet, "/auth/google/callback?code=code-1&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	for _, ck := range start.Cookies() {
		req.AddCookie(ck)
	}
	cb := c.Do(req)
	back, _ := url.Parse(cb.Header.Get("Location"))
	if cb.StatusCode != http.StatusFound || back.Query().Get("code") == "" {
		t.Fatalf("callback = %d %s", cb.StatusCode, back)
	}
	// The PKCE verifier went to the token endpoint.
	if forms := idp.Forms(); len(forms) != 1 || forms[0].Get("code_verifier") == "" {
		t.Fatalf("token request: %v", forms)
	}
	ex := httptest.NewRequest(http.MethodPost, "/auth/exchange", strings.NewReader(`{"code":"`+back.Query().Get("code")+`"}`))
	ex.Header.Set("Content-Type", "application/json")
	for _, ck := range cb.Cookies() {
		ex.AddCookie(ck)
	}
	resp := c.Do(ex)
	var tok struct {
		Token        string `json:"token"`
		RedirectBack string `json:"redirect_back"`
	}
	decode(t, resp, &tok)
	if resp.StatusCode != http.StatusOK || tok.Token == "" || tok.RedirectBack != "/home" {
		t.Fatalf("exchange = %d %+v", resp.StatusCode, tok)
	}
	var me struct {
		Email         string `json:"email"`
		Name          string `json:"name"`
		EmailVerified bool   `json:"email_verified"`
	}
	if code := c.WithBearer(tok.Token).GetJSON("/auth/me", &me); code != http.StatusOK || me.Email != "alice@gmail.test" || me.Name != "Alice" || !me.EmailVerified {
		t.Fatalf("me = %d %+v", code, me)
	}
}

// TestProviders_RefusedConfigFailsNew: a provider's own checks surface
// from identity.New, with the provider's name.
func TestProviders_RefusedConfigFailsNew(t *testing.T) {
	cfg := identity.Config{
		SigningKey: strings.Repeat("k", 32), OAuthCallbackFrontendURL: "https://app.example.test/finish",
		Providers: map[string]identity.ProviderConfig{"google": {ClientID: "only-id"}},
	}
	_, err := identity.New(cfg, dbtest.Open(t), identity.Providers(google.New(cfg.Providers["google"])))
	if err == nil || !strings.Contains(err.Error(), "provider google: google: client_secret is required") {
		t.Fatalf("a refused provider config: %v", err)
	}
}
