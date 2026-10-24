package identity

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/choktest"
	"github.com/zynthara/chok/v3/identity/internal/oauth"
	"github.com/zynthara/chok/v3/internal/dbtest"
	"github.com/zynthara/chok/v3/store"
)

// fakeProvider is an identity provider in process: BeginAuth points at a
// made-up authorize URL carrying the request, CompleteAuth answers the
// identity seeded for a code.
type fakeProvider struct {
	name  string
	caps  oauth.Capabilities
	check error

	mu       sync.Mutex
	codes    map[string]*oauth.Identity
	begins   []oauth.BeginRequest
	complete []oauth.CompleteRequest
}

func newFake(name string) *fakeProvider {
	return &fakeProvider{name: name, caps: oauth.Capabilities{CallbackMethod: http.MethodGet}, codes: map[string]*oauth.Identity{}}
}

func (p *fakeProvider) seed(code string, ext *oauth.Identity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.codes[code] = ext
}

func (p *fakeProvider) Name() string                     { return p.name }
func (p *fakeProvider) Check() error                     { return p.check }
func (p *fakeProvider) Capabilities() oauth.Capabilities { return p.caps }

func (p *fakeProvider) BeginAuth(_ context.Context, req *oauth.BeginRequest) (string, error) {
	p.mu.Lock()
	p.begins = append(p.begins, *req)
	p.mu.Unlock()
	v := url.Values{"state": {req.State}}
	if req.Nonce != "" {
		v.Set("nonce", req.Nonce)
	}
	if req.CodeChallenge != "" {
		v.Set("code_challenge", req.CodeChallenge)
	}
	return "https://idp.test/" + p.name + "/authorize?" + v.Encode(), nil
}

var errUnknownCode = errors.New("fake: unknown code")

func (p *fakeProvider) CompleteAuth(_ context.Context, req *oauth.CompleteRequest) (*oauth.Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.complete = append(p.complete, *req)
	ext, ok := p.codes[req.Code]
	if !ok {
		return nil, errUnknownCode
	}
	cp := *ext
	cp.Raw = maps.Clone(ext.Raw)
	return &cp, nil
}

// oauthFixture is an Identity with one fake provider, served in process.
type oauthFixture struct {
	idn *Identity
	p   *fakeProvider
	c   *choktest.Client
}

const frontendURL = "https://app.example.test/auth/finish"

func newOAuthFixture(t *testing.T, cfg Config, p *fakeProvider) *oauthFixture {
	t.Helper()
	if p == nil {
		p = newFake("fake")
	}
	if cfg.OAuthCallbackFrontendURL == "" {
		cfg.OAuthCallbackFrontendURL = frontendURL
	}
	idn := newIdentity(t, cfg, Providers(p))
	return &oauthFixture{idn: idn, p: p, c: serve(t, idn)}
}

// get sends a GET with cookies and returns the raw response.
func (fx *oauthFixture) send(method, path string, cookies []*http.Cookie, form url.Values) *http.Response {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	return fx.c.Do(req)
}

func (fx *oauthFixture) sendJSON(t *testing.T, path string, body any, cookies []*http.Cookie, headers ...string) *http.Response {
	t.Helper()
	req := jsonRequest(t, http.MethodPost, path, body, headers...)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	return fx.c.Do(req)
}

func query(t *testing.T, raw, key string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get(key)
}

func cookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, ck := range cookies {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

// signIn runs start and callback for code and returns the callback's
// response.
func (fx *oauthFixture) signIn(t *testing.T, code, back string) *http.Response {
	t.Helper()
	path := "/auth/fake/start"
	if back != "" {
		path += "?redirect_back=" + url.QueryEscape(back)
	}
	start := fx.send(http.MethodGet, path, nil, nil)
	if start.StatusCode != http.StatusFound {
		t.Fatalf("start = %d", start.StatusCode)
	}
	state := query(t, start.Header.Get("Location"), "state")
	return fx.send(http.MethodGet, "/auth/fake/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), start.Cookies(), nil)
}

func verified(account, email string) *oauth.Identity {
	return &oauth.Identity{Provider: "fake", ProviderAccountID: account, Email: email, EmailVerified: true, Name: "Ext"}
}

func TestOAuth_BeginCallbackExchange(t *testing.T) {
	p := newFake("fake")
	p.caps = oauth.Capabilities{CallbackMethod: http.MethodGet, RequiresNonce: true, SupportsPKCE: true}
	fx := newOAuthFixture(t, Config{}, p)
	p.seed("code-1", verified("fake-acct-1", "alice@idp.test"))

	cb := fx.signIn(t, "code-1", "/dashboard")
	if cb.StatusCode != http.StatusFound || !strings.HasPrefix(cb.Header.Get("Location"), frontendURL+"?code=") {
		t.Fatalf("callback = %d %q", cb.StatusCode, cb.Header.Get("Location"))
	}
	// The provider saw a nonce and a PKCE challenge, then the verifier
	// whose S256 is that challenge.
	if len(p.begins) != 1 || p.begins[0].Nonce == "" || len(p.complete) != 1 || pkceChallenge(p.complete[0].CodeVerifier) != p.begins[0].CodeChallenge ||
		p.complete[0].Nonce != p.begins[0].Nonce {
		t.Fatalf("begin %+v complete %+v", p.begins, p.complete)
	}
	binding := cookie(cb.Cookies(), bindingCookie)
	if binding == nil || binding.Value == "" || !binding.HttpOnly || !binding.Secure || binding.SameSite != http.SameSiteNoneMode {
		t.Fatalf("the binding cookie: %+v", binding)
	}
	code := query(t, cb.Header.Get("Location"), "code")
	resp := fx.sendJSON(t, "/auth/exchange", map[string]string{"code": code}, cb.Cookies())
	var out exchangeResponse
	decodeBody(t, resp, &out)
	if resp.StatusCode != http.StatusOK || out.Token == "" || out.RedirectBack != "/dashboard" {
		t.Fatalf("exchange = %d %+v", resp.StatusCode, out)
	}
	if ck := cookie(resp.Cookies(), bindingCookie); ck == nil || ck.MaxAge >= 0 {
		t.Fatalf("exchange clears the binding cookie: %+v", ck)
	}
	// The token is the new user's: verified email, no password.
	var me meResponse
	if code := fx.c.WithBearer(out.Token).GetJSON("/auth/me", &me); code != http.StatusOK || me.Email != "alice@idp.test" || !me.EmailVerified {
		t.Fatalf("me = %d %+v", code, me)
	}
	u := userByEmail(t, fx.idn, "alice@idp.test")
	if u.HasPassword || u.Name != "Ext" {
		t.Fatalf("an account created through a provider: %+v", u)
	}
	// A second sign-in finds the link and the same user.
	cb = fx.signIn(t, "code-1", "")
	if cb.StatusCode != http.StatusFound {
		t.Fatalf("second sign-in = %d", cb.StatusCode)
	}
	var n int64
	fx.idn.d.Unsafe(t.Context()).Model(&User{}).Count(&n)
	if n != 1 {
		t.Fatalf("users = %d", n)
	}
}

func TestOAuth_ExchangeNeedsTheBindingCookie(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	fx.p.seed("c", verified("x", "x@idp.test"))
	for name, cookies := range map[string][]*http.Cookie{
		"no cookie":       nil,
		"another browser": {{Name: bindingCookie, Value: "definitely-not-the-right-token"}},
	} {
		cb := fx.signIn(t, "c", "")
		code := query(t, cb.Header.Get("Location"), "code")
		var e envelope
		resp := fx.sendJSON(t, "/auth/exchange", map[string]string{"code": code}, cookies)
		decodeBody(t, resp, &e)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(e.Message, "binding") {
			t.Errorf("%s = %d %+v", name, resp.StatusCode, e)
		}
	}
	// A malformed body and an empty code are 400s.
	for _, body := range []any{map[string]string{"code": ""}, map[string]any{"code": "x", "extra": 1}} {
		if resp := fx.sendJSON(t, "/auth/exchange", body, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d", body, resp.StatusCode)
		}
	}
}

func TestOAuth_CodeReplayIsGone(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	fx.p.seed("c", verified("x", "x@idp.test"))
	cb := fx.signIn(t, "c", "")
	code := query(t, cb.Header.Get("Location"), "code")
	if resp := fx.sendJSON(t, "/auth/exchange", map[string]string{"code": code}, cb.Cookies()); resp.StatusCode != http.StatusOK {
		t.Fatalf("first exchange = %d", resp.StatusCode)
	}
	var e envelope
	resp := fx.sendJSON(t, "/auth/exchange", map[string]string{"code": code}, cb.Cookies())
	decodeBody(t, resp, &e)
	if resp.StatusCode != http.StatusGone || e.Reason != "Gone" {
		t.Fatalf("replay = %d %+v", resp.StatusCode, e)
	}
}

func TestOAuth_RedirectBack(t *testing.T) {
	fx := newOAuthFixture(t, Config{AllowedRedirectBacks: []string{"https://app.example.test/"}}, nil)
	for back, ok := range map[string]bool{
		"/posts/123":                          true,
		"https://app.example.test/post-login": true,
		"https://evil.com/":                   false,
		"//evil.com/x":                        false,
		"https://app.example.test.evil.com/":  false,
		`/\evil.com`:                          false,
	} {
		resp := fx.send(http.MethodGet, "/auth/fake/start?redirect_back="+url.QueryEscape(back), nil, nil)
		if got := resp.StatusCode == http.StatusFound; got != ok {
			t.Errorf("redirect_back %q: %d", back, resp.StatusCode)
		}
	}
	// Without an allow-list, only relative paths pass.
	strict := newOAuthFixture(t, Config{}, nil)
	if resp := strict.send(http.MethodGet, "/auth/fake/start?redirect_back="+url.QueryEscape("https://app.example.test/x"), nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an absolute URL without an allow-list = %d", resp.StatusCode)
	}
}

func TestOAuth_StateMismatch(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	fx.p.seed("c", verified("x", "x@idp.test"))
	start := fx.send(http.MethodGet, "/auth/fake/start", nil, nil)
	resp := fx.send(http.MethodGet, "/auth/fake/callback?code=c&state=tampered", start.Cookies(), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a tampered state = %d", resp.StatusCode)
	}
	if len(fx.p.complete) != 0 {
		t.Fatal("the provider is never asked with a wrong state")
	}
}

func TestOAuth_SessionExpiredIsGone(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	fx.p.seed("c", verified("x", "x@idp.test"))
	if resp := fx.send(http.MethodGet, "/auth/fake/callback?code=c&state=anything", nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no session cookie = %d", resp.StatusCode)
	}
	forged := []*http.Cookie{{Name: sessionCookie, Value: "sid.not-the-signature"}}
	if resp := fx.send(http.MethodGet, "/auth/fake/callback?code=c&state=anything", forged, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a forged session cookie = %d", resp.StatusCode)
	}
	start := fx.send(http.MethodGet, "/auth/fake/start", nil, nil)
	state := query(t, start.Header.Get("Location"), "state")
	sid := cookie(start.Cookies(), sessionCookie).Value
	if _, err := fx.idn.oauth.store.takeSession(sid[:strings.LastIndex(sid, ".")]); err != nil {
		t.Fatal(err)
	}
	if resp := fx.send(http.MethodGet, "/auth/fake/callback?code=c&state="+url.QueryEscape(state), start.Cookies(), nil); resp.StatusCode != http.StatusGone {
		t.Fatalf("a used session = %d", resp.StatusCode)
	}
}

func TestOAuth_NewAccountNeedsAVerifiedEmail(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	cases := map[string]*oauth.Identity{
		"email":       {Provider: "fake", ProviderAccountID: "a"},
		"verified":    {Provider: "fake", ProviderAccountID: "b", Email: "b@idp.test"},
		"non_aliased": {Provider: "fake", ProviderAccountID: "c", Email: "c@privaterelay.test", EmailVerified: true, IsAliasedEmail: true},
	}
	for missing, ext := range cases {
		fx.p.seed(missing, ext)
		resp := fx.signIn(t, missing, "")
		var e envelope
		decodeBody(t, resp, &e)
		if resp.StatusCode != http.StatusBadRequest || e.Reason != "OAUTH_EMAIL_REQUIRED" || e.Metadata["missing"] != missing || e.Metadata["provider"] != "fake" {
			t.Errorf("%s: %d %+v", missing, resp.StatusCode, e)
		}
	}
}

func TestOAuth_AliasedEmailNeverLinks(t *testing.T) {
	fx := newOAuthFixture(t, Config{LinkByEmail: true}, nil)
	// A verified local account with the same address.
	u, err := fx.idn.Users().Create(t.Context(), "alice@idp.test", "password123", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.idn.Users().MarkEmailVerified(t.Context(), u.RID); err != nil {
		t.Fatal(err)
	}
	fx.p.seed("c", &oauth.Identity{Provider: "fake", ProviderAccountID: "acct", Email: "alice@idp.test", EmailVerified: true, IsAliasedEmail: true})
	resp := fx.signIn(t, "c", "")
	var e envelope
	decodeBody(t, resp, &e)
	if resp.StatusCode != http.StatusBadRequest || e.Metadata["missing"] != "non_aliased" {
		t.Fatalf("an aliased email = %d %+v", resp.StatusCode, e)
	}
}

func TestOAuth_LinkByEmail(t *testing.T) {
	fx := newOAuthFixture(t, Config{LinkByEmail: true}, nil)
	u, err := fx.idn.Users().Create(t.Context(), "alice@idp.test", "password123", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	// An unverified local email never links: the account stays someone
	// else's until proven, so the sign-in conflicts instead.
	fx.p.seed("c", verified("acct", "alice@idp.test"))
	if resp := fx.signIn(t, "c", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("an unverified local email = %d", resp.StatusCode)
	}
	if err := fx.idn.Users().MarkEmailVerified(t.Context(), u.RID); err != nil {
		t.Fatal(err)
	}
	if resp := fx.signIn(t, "c", ""); resp.StatusCode != http.StatusFound {
		t.Fatalf("a verified local email = %d", resp.StatusCode)
	}
	links, _ := fx.idn.oauth.links(t.Context(), u.RID)
	if len(links) != 1 || links[0].ProviderAccountID != "acct" {
		t.Fatalf("linked to the local account: %+v", links)
	}
	// Without link_by_email the same sign-in conflicts on the email.
	off := newOAuthFixture(t, Config{}, nil)
	if _, err := off.idn.Users().Create(t.Context(), "bob@idp.test", "password123", ""); err != nil {
		t.Fatal(err)
	}
	off.p.seed("c", verified("acct", "bob@idp.test"))
	resp := off.signIn(t, "c", "")
	var e envelope
	decodeBody(t, resp, &e)
	if resp.StatusCode != http.StatusConflict || e.Metadata["field"] != "email" {
		t.Fatalf("link_by_email off = %d %+v", resp.StatusCode, e)
	}
}

func TestOAuth_EmailNormalized(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	u, _, err := fx.idn.oauth.resolve(t.Context(), verified("acc-mixed", "  Alice@IDP.test "))
	if err != nil {
		t.Fatal(err)
	}
	links, _ := fx.idn.oauth.links(t.Context(), u.RID)
	if u.Email != "alice@idp.test" || len(links) != 1 || links[0].Email != "alice@idp.test" {
		t.Fatalf("user %q links %+v", u.Email, links)
	}
}

func TestOAuth_ResolveRollsBack(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	u, _, err := fx.idn.oauth.resolve(t.Context(), verified("acc-1", "alice@idp.test"))
	if err != nil {
		t.Fatal(err)
	}
	// Same email, another provider account: the user insert conflicts, and
	// the transaction leaves no link behind.
	if _, _, err := fx.idn.oauth.resolve(t.Context(), verified("acc-2", "alice@idp.test")); !errors.Is(err, chok.ErrConflict) {
		t.Fatalf("a second account for the email: %v", err)
	}
	var n int64
	fx.idn.d.Unsafe(t.Context()).Model(&linkedIdentity{}).Count(&n)
	if links, _ := fx.idn.oauth.links(t.Context(), u.RID); n != 1 || len(links) != 1 || links[0].ProviderAccountID != "acc-1" {
		t.Fatalf("links=%d %+v", n, links)
	}
}

func TestOAuth_OAuthOnlyAccountLogin(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	u, _, err := fx.idn.oauth.resolve(t.Context(), verified("acc", "alice@idp.test"))
	if err != nil || u.PasswordVersion != 0 || u.HasPassword {
		t.Fatalf("%+v %v", u, err)
	}
	var e envelope
	if code := fx.c.PostJSON("/auth/login", map[string]string{"email": "alice@idp.test", "password": "anything-wrong1"}, &e); code != http.StatusUnauthorized || e.Reason != "OAUTH_ONLY_ACCOUNT" {
		t.Fatalf("a password login on a provider account = %d %+v", code, e)
	}
	// A password reset gives the account a password of its own.
	if err := fx.idn.Users().ResetPassword(t.Context(), u.RID, "Password123"); err != nil {
		t.Fatal(err)
	}
	if code, _ := login(t, fx.c, "alice@idp.test", "Password123"); code != http.StatusOK {
		t.Fatalf("login after the reset = %d", code)
	}
}

func TestOAuth_LinkFlowBindsToTheCurrentUser(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	tok := register(t, fx.c, "alice@local.test", "Password!23")
	alice := userByEmail(t, fx.idn, "alice@local.test")
	// The provider account has another email: without the link intent the
	// callback would sign in a new user, carol.
	fx.p.seed("idp-code", verified("remote-acc-99", "carol@idp.test"))
	resp := fx.sendJSON(t, "/auth/identities/link", map[string]string{"provider": "fake", "redirect_back": "/settings"}, nil, bearerHeader(tok)...)
	var link linkResponse
	decodeBody(t, resp, &link)
	if resp.StatusCode != http.StatusOK || link.RedirectTo == "" {
		t.Fatalf("link = %d %+v", resp.StatusCode, link)
	}
	state := query(t, link.RedirectTo, "state")
	cb := fx.send(http.MethodGet, "/auth/fake/callback?code=idp-code&state="+url.QueryEscape(state), resp.Cookies(), nil)
	loc := cb.Header.Get("Location")
	if cb.StatusCode != http.StatusFound || !strings.HasPrefix(loc, "/settings?link_status=ok") || strings.Contains(loc, "code=") {
		t.Fatalf("callback = %d %q", cb.StatusCode, loc)
	}
	links, _ := fx.idn.oauth.links(t.Context(), alice.RID)
	if len(links) != 1 || links[0].ProviderAccountID != "remote-acc-99" {
		t.Fatalf("alice's links: %+v", links)
	}
	var n int64
	fx.idn.d.Unsafe(t.Context()).Model(&User{}).Count(&n)
	if n != 1 {
		t.Fatalf("users = %d: the link created one", n)
	}
	// The link start checks its input.
	for _, body := range []map[string]string{{"provider": "nope"}, {"provider": "fake", "redirect_back": "https://evil.com/"}} {
		if resp := fx.sendJSON(t, "/auth/identities/link", body, nil, bearerHeader(tok)...); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d", body, resp.StatusCode)
		}
	}
	if resp := fx.sendJSON(t, "/auth/identities/link", map[string]string{"provider": "fake"}, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("link without a token = %d", resp.StatusCode)
	}
}

func TestOAuth_PasswordUserSurvivesLink(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	tok := register(t, fx.c, "alice@local.test", "Password!23")
	u := userByEmail(t, fx.idn, "alice@local.test")
	if _, err := fx.idn.oauth.link(t.Context(), u.RID, verified("remote-1", "carol@idp.test")); err != nil {
		t.Fatal(err)
	}
	if code, _ := login(t, fx.c, "alice@local.test", "Password!23"); code != http.StatusOK {
		t.Fatalf("login after a link = %d", code)
	}
	var methods methodsResponse
	if code := fx.c.WithBearer(tok).GetJSON("/auth/identities", &methods); code != http.StatusOK || len(methods.Methods) != 2 ||
		methods.Methods[0].Type != "password" || methods.Methods[1].Type != "fake" || methods.Methods[1].IdentityID == "" {
		t.Fatalf("methods = %d %+v", code, methods)
	}
	// The password remains, so the link can go.
	if code := fx.c.WithBearer(tok).JSON(http.MethodDelete, "/auth/identities/"+methods.Methods[1].IdentityID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("unlink = %d", code)
	}
	if code, _ := login(t, fx.c, "alice@local.test", "Password!23"); code != http.StatusOK {
		t.Fatalf("login after the unlink = %d", code)
	}
}

func TestOAuth_UnlinkKeepsTheLastMethod(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	u, _, err := fx.idn.oauth.resolve(t.Context(), verified("acc-1", "alice@idp.test"))
	if err != nil {
		t.Fatal(err)
	}
	links, _ := fx.idn.oauth.links(t.Context(), u.RID)
	err = fx.idn.oauth.unlinkIdentity(t.Context(), u.RID, links[0].RID)
	var e *chok.Error
	if !errors.As(err, &e) || e.Code != http.StatusPreconditionFailed {
		t.Fatalf("unlinking the last method: %v", err)
	}
	if after, _ := fx.idn.oauth.links(t.Context(), u.RID); len(after) != 1 {
		t.Fatal("the last link survives")
	}
}

func TestOAuth_UnlinkIsTheOwnersOnly(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	a, _, _ := fx.idn.oauth.resolve(t.Context(), verified("acc-A", "a@idp.test"))
	b, _, _ := fx.idn.oauth.resolve(t.Context(), verified("acc-B", "b@idp.test"))
	if _, err := fx.idn.oauth.link(t.Context(), a.RID, verified("acc-A2", "a@idp.test")); err != nil {
		t.Fatal(err)
	}
	bLinks, _ := fx.idn.oauth.links(t.Context(), b.RID)
	if err := fx.idn.oauth.unlinkIdentity(t.Context(), a.RID, bLinks[0].RID); !errors.Is(err, chok.ErrNotFound) {
		t.Fatalf("a's unlink of b's link: %v", err)
	}
	if after, _ := fx.idn.oauth.links(t.Context(), b.RID); len(after) != 1 {
		t.Fatal("b's link survives")
	}
	// The provider account is b's: linking it to a conflicts.
	if _, err := fx.idn.oauth.link(t.Context(), a.RID, verified("acc-B", "a@idp.test")); !errors.Is(err, chok.ErrConflict) {
		t.Fatalf("linking b's account to a: %v", err)
	}
}

func TestOAuth_UnlinkRaceLeavesOneMethod(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	u, _, err := fx.idn.oauth.resolve(t.Context(), verified("acc-1", "alice@idp.test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.idn.oauth.link(t.Context(), u.RID, verified("acc-2", "alice@idp.test")); err != nil {
		t.Fatal(err)
	}
	links, _ := fx.idn.oauth.links(t.Context(), u.RID)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() { errs[i] = fx.idn.oauth.unlinkIdentity(context.Background(), u.RID, links[i].RID) })
	}
	wg.Wait()
	if errs[0] == nil && errs[1] == nil {
		t.Fatal("both unlinks passed the guard: the user has no way to sign in")
	}
	if after, _ := fx.idn.oauth.links(t.Context(), u.RID); len(after) != 1 {
		t.Fatalf("%d links left", len(after))
	}
}

func TestOAuth_DevModeFromLocalhostRedirect(t *testing.T) {
	p := newFake("fake")
	p.caps.RedirectURL = "http://localhost:8080/auth/fake/callback"
	fx := newOAuthFixture(t, Config{OAuthCallbackFrontendURL: "http://localhost:3000/auth/finish"}, p)
	p.seed("c", verified("x", "x@idp.test"))
	start := fx.send(http.MethodGet, "/auth/fake/start", nil, nil)
	sid := cookie(start.Cookies(), sessionCookie)
	if sid == nil || sid.Secure || sid.SameSite != http.SameSiteLaxMode || !sid.HttpOnly {
		t.Fatalf("the session cookie on http localhost: %+v", sid)
	}
	state := query(t, start.Header.Get("Location"), "state")
	cb := fx.send(http.MethodGet, "/auth/fake/callback?code=c&state="+url.QueryEscape(state), start.Cookies(), nil)
	if binding := cookie(cb.Cookies(), bindingCookie); binding == nil || binding.Secure || binding.SameSite != http.SameSiteLaxMode {
		t.Fatalf("the binding cookie follows the session cookie: %+v", binding)
	}
}

func TestOAuth_FormPostCallback(t *testing.T) {
	p := newFake("fake")
	p.caps = oauth.Capabilities{CallbackMethod: http.MethodPost, RequiresFormPost: true, RequiresNonce: true}
	fx := newOAuthFixture(t, Config{}, p)
	p.seed("c", verified("x", "x@idp.test"))
	start := fx.send(http.MethodGet, "/auth/fake/start", nil, nil)
	state := query(t, start.Header.Get("Location"), "state")
	if resp := fx.send(http.MethodGet, "/auth/fake/callback?code=c&state="+url.QueryEscape(state), start.Cookies(), nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on a form-post callback = %d", resp.StatusCode)
	}
	form := url.Values{"code": {"c"}, "state": {state}, "user": {`{"name":{"firstName":"Al"}}`}}
	cb := fx.send(http.MethodPost, "/auth/fake/callback", start.Cookies(), form)
	if cb.StatusCode != http.StatusFound || len(p.complete) != 1 || p.complete[0].FormBody.Get("user") == "" {
		t.Fatalf("form post = %d %+v", cb.StatusCode, p.complete)
	}
}

func TestOAuth_CloseStopsTheStore(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	if err := fx.idn.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := fx.idn.Close(t.Context()); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	select {
	case <-fx.idn.oauth.store.stop:
	default:
		t.Fatal("the store's sweep is stopped")
	}
}

func TestProviders_Registration(t *testing.T) {
	d := func() Config { return Config{SigningKey: testKey, OAuthCallbackFrontendURL: frontendURL} }
	broken := newFake("broken")
	broken.check = errors.New("client_id is required")
	cases := map[string]struct {
		cfg       Config
		providers []Provider
		want      string
	}{
		"no frontend": {Config{SigningKey: testKey}, []Provider{newFake("fake")}, "oauth_callback_frontend_url"},
		"a duplicate": {d(), []Provider{newFake("fake"), newFake("fake")}, "given twice"},
		"nil":         {d(), []Provider{nil}, "is nil"},
		"a bad name":  {d(), []Provider{newFake("Fake!")}, "lower-case"},
		"broken":      {d(), []Provider{broken}, "provider broken: client_id is required"},
		"configured, not linked": {
			Config{SigningKey: testKey, OAuthCallbackFrontendURL: frontendURL, Providers: map[string]ProviderConfig{"google": {}}},
			nil, "auth.providers.google is configured but not linked",
		},
	}
	for name, c := range cases {
		d := dbtest.Open(t)
		_, err := New(c.cfg, d, Providers(c.providers...))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
		// A refused provider leaves the database alone.
		if d.Unsafe(t.Context()).Migrator().HasTable("users") {
			t.Errorf("%s: New created tables before refusing", name)
		}
	}
}

func TestProviders_MountTheirRoutes(t *testing.T) {
	g := newFake("gitfake")
	a := newFake("applefake")
	a.caps = oauth.Capabilities{CallbackMethod: http.MethodPost, RequiresFormPost: true}
	idn := newIdentity(t, Config{OAuthCallbackFrontendURL: frontendURL}, Providers(g, a))
	c := serve(t, idn)
	for path, method := range map[string]string{
		"/auth/gitfake/start": http.MethodGet, "/auth/gitfake/callback": http.MethodGet,
		"/auth/applefake/start": http.MethodGet, "/auth/applefake/callback": http.MethodPost,
	} {
		resp := c.Do(httptest.NewRequest(method, path, nil))
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d", method, path, resp.StatusCode)
		}
	}
	// Without providers nothing OAuth is mounted.
	plain := serve(t, newIdentity(t, Config{}))
	if code := plain.PostJSON("/auth/exchange", map[string]string{"code": "x"}, nil); code != http.StatusNotFound {
		t.Fatalf("exchange without providers = %d", code)
	}
}

func TestOAuth_ProviderErrorsReachTheClient(t *testing.T) {
	fx := newOAuthFixture(t, Config{}, nil)
	// An unknown code: the provider's error is a 500 whose cause stays in
	// the log.
	resp := fx.signIn(t, "never-seeded", "")
	var e envelope
	decodeBody(t, resp, &e)
	if resp.StatusCode != http.StatusInternalServerError || strings.Contains(e.Message, "unknown code") {
		t.Fatalf("a provider failure = %d %+v", resp.StatusCode, e)
	}
	// A disabled account cannot sign in through its link.
	fx.p.seed("c", verified("acct", "dis@idp.test"))
	if resp := fx.signIn(t, "c", ""); resp.StatusCode != http.StatusFound {
		t.Fatalf("first sign-in = %d", resp.StatusCode)
	}
	u := userByEmail(t, fx.idn, "dis@idp.test")
	if err := fx.idn.Users().SetActive(t.Context(), u.RID, false); err != nil {
		t.Fatal(err)
	}
	if resp := fx.signIn(t, "c", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a disabled account's sign-in = %d", resp.StatusCode)
	}
	if err := fx.idn.users.Delete(t.Context(), store.RID(u.RID)); err != nil {
		t.Fatal(err)
	}
}
