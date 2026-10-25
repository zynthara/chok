package identity

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/zynthara/chok/v3/choktest"
)

// cookieIdentity is an Identity in cookie mode with passwordless sign-in.
func cookieIdentity(t *testing.T, cookie CookieConfig) (*Identity, *mockSender, *choktest.Client) {
	t.Helper()
	cookie.Enabled = true
	sender := newMockSender()
	idn := newIdentity(t, Config{Cookie: cookie, Passwordless: PasswordlessConfig{Enabled: true}}, Sender(sender))
	return idn, sender, serve(t, idn)
}

// post sends a JSON POST with the given cookies and header pairs.
func post(t *testing.T, c *choktest.Client, path string, body any, cookies []*http.Cookie, headers ...string) *http.Response {
	t.Helper()
	var req *http.Request
	if body != nil {
		req = jsonRequest(t, http.MethodPost, path, body, headers...)
	} else {
		req = httptest.NewRequest(http.MethodPost, path, nil)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	return c.Do(req)
}

// get sends a GET with the given cookies and header pairs.
func get(c *choktest.Client, path string, cookies []*http.Cookie, headers ...string) *http.Response {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	return c.Do(req)
}

// sessionOf is the session cookie of resp, checked for its attributes.
func sessionOf(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	ck := cookie(resp.Cookies(), "chok_token")
	if ck == nil {
		t.Fatalf("no session cookie: %v", resp.Header.Values("Set-Cookie"))
	}
	if ck.Value == "" || !ck.HttpOnly || !ck.Secure || ck.Path != "/" || ck.Domain != "" || ck.SameSite != http.SameSiteLaxMode || ck.MaxAge != 7200 {
		t.Fatalf("the session cookie: %q", resp.Header.Get("Set-Cookie"))
	}
	return ck
}

// bodyOf decodes a response body into a map.
func bodyOf(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	decodeBody(t, resp, &m)
	return m
}

// TestCookie_EveryIssuingRouteSetsIt: register, login, refresh-token and
// passwordless verify set the session cookie — the token of the body —
// and the cookie alone authenticates.
func TestCookie_EveryIssuingRouteSetsIt(t *testing.T) {
	_, sender, c := cookieIdentity(t, CookieConfig{})
	check := func(name string, resp *http.Response, status int) *http.Cookie {
		t.Helper()
		ck := sessionOf(t, resp)
		if body := bodyOf(t, resp); resp.StatusCode != status || body["token"] != ck.Value || body["expires_at"] == nil {
			t.Fatalf("%s = %d %v", name, resp.StatusCode, body)
		}
		return ck
	}
	check("register", post(t, c, "/auth/register", map[string]string{"email": "alice@test.com", "password": "password123"}, nil), http.StatusCreated)
	ck := check("login", post(t, c, "/auth/login", map[string]string{"email": "alice@test.com", "password": "password123"}, nil), http.StatusOK)
	ck = check("refresh-token", post(t, c, "/auth/refresh-token", nil, []*http.Cookie{ck}), http.StatusOK)
	m := startCode(t, c, sender, "bob@test.com")
	check("passwordless verify", post(t, c, "/auth/passwordless/verify", map[string]string{"email": "bob@test.com", "code": m.Code}, nil), http.StatusOK)
	var me meResponse
	resp := get(c, "/auth/me", []*http.Cookie{ck})
	decodeBody(t, resp, &me)
	if resp.StatusCode != http.StatusOK || me.Email != "alice@test.com" {
		t.Fatalf("me by cookie = %d %+v", resp.StatusCode, me)
	}
}

// TestCookie_OAuthExchangeSetsIt: the fifth issuing route, the OAuth
// exchange, sets the cookie too, and leaves the token out with only.
func TestCookie_OAuthExchangeSetsIt(t *testing.T) {
	for _, only := range []bool{false, true} {
		fx := newOAuthFixture(t, Config{Cookie: CookieConfig{Enabled: true, Only: only}}, nil)
		fx.p.seed("code-1", verified("acct-1", "alice@idp.test"))
		cb := fx.signIn(t, "code-1", "/home")
		resp := fx.sendJSON(t, "/auth/exchange", map[string]string{"code": query(t, cb.Header.Get("Location"), "code")}, cb.Cookies())
		ck := sessionOf(t, resp)
		body := bodyOf(t, resp)
		token, hasToken := body["token"]
		if resp.StatusCode != http.StatusOK || body["redirect_back"] != "/home" || hasToken == only || (!only && token != ck.Value) {
			t.Fatalf("exchange with only=%v = %d %v", only, resp.StatusCode, body)
		}
		if code := get(fx.c, "/auth/me", []*http.Cookie{ck}).StatusCode; code != http.StatusOK {
			t.Fatalf("me by the exchange's cookie = %d", code)
		}
	}
}

// TestCookie_Only: with only, a body that issues a token is {expires_at}
// alone; the session lives in the cookie, refreshed through it.
func TestCookie_Only(t *testing.T) {
	_, sender, c := cookieIdentity(t, CookieConfig{Only: true})
	m := startCode(t, c, sender, "alice@test.com")
	resp := post(t, c, "/auth/passwordless/verify", map[string]string{"email": "alice@test.com", "code": m.Code}, nil)
	ck := sessionOf(t, resp)
	if body := bodyOf(t, resp); resp.StatusCode != http.StatusOK || len(body) != 1 || body["expires_at"] == nil {
		t.Fatalf("verify with only = %d %v", resp.StatusCode, body)
	}
	for name, resp := range map[string]*http.Response{
		"register":      post(t, c, "/auth/register", map[string]string{"email": "bob@test.com", "password": "password123"}, nil),
		"login":         post(t, c, "/auth/login", map[string]string{"email": "bob@test.com", "password": "password123"}, nil),
		"refresh-token": post(t, c, "/auth/refresh-token", nil, []*http.Cookie{ck}),
	} {
		sessionOf(t, resp)
		if body := bodyOf(t, resp); resp.StatusCode >= 300 || len(body) != 1 || body["expires_at"] == nil {
			t.Fatalf("%s with only = %d %v", name, resp.StatusCode, body)
		}
	}
	// The document says so: the login answers {expires_at}.
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	c.GetJSON("/openapi.json", &doc)
	ref := doc.Paths["/auth/login"]["post"].Responses["200"].Content["application/json"].Schema.Ref
	props := doc.Components.Schemas[strings.TrimPrefix(ref, "#/components/schemas/")].Properties
	if _, hasToken := props["token"]; hasToken || props["expires_at"] == nil {
		t.Fatalf("the login's response %s: %v", ref, props)
	}
}

// TestCookie_AuthnReadsIt: without an Authorization header Authn reads
// the cookie; with one, the header decides, valid or not; a bad cookie is
// a 401 like a bad bearer token. Outside cookie mode the cookie is
// nothing.
func TestCookie_AuthnReadsIt(t *testing.T) {
	_, _, c := cookieIdentity(t, CookieConfig{})
	ck := sessionOf(t, post(t, c, "/auth/register", map[string]string{"email": "alice@test.com", "password": "password123"}, nil))
	bad := &http.Cookie{Name: "chok_token", Value: "not.a.token"}
	for name, tc := range map[string]struct {
		cookies []*http.Cookie
		headers []string
		code    int
		message string
	}{
		"the cookie":                    {[]*http.Cookie{ck}, nil, http.StatusOK, ""},
		"a bad cookie":                  {[]*http.Cookie{bad}, nil, http.StatusUnauthorized, "the token is invalid or expired"},
		"an empty cookie":               {[]*http.Cookie{{Name: "chok_token", Value: ""}}, nil, http.StatusUnauthorized, "a bearer token or the session cookie is required"},
		"nothing":                       {nil, nil, http.StatusUnauthorized, "a bearer token or the session cookie is required"},
		"a good header, a bad cookie":   {[]*http.Cookie{bad}, bearerHeader(ck.Value), http.StatusOK, ""},
		"a bad header, a good cookie":   {[]*http.Cookie{ck}, bearerHeader("not.a.token"), http.StatusUnauthorized, "the token is invalid or expired"},
		"another scheme, a good cookie": {[]*http.Cookie{ck}, []string{"Authorization", "Basic YTpi"}, http.StatusUnauthorized, "a bearer token is required"},
		"an oversized cookie":           {[]*http.Cookie{{Name: "chok_token", Value: strings.Repeat("a", 9<<10)}}, nil, http.StatusUnauthorized, "the token is invalid or expired"},
	} {
		resp := get(c, "/auth/me", tc.cookies, tc.headers...)
		var e envelope
		decodeBody(t, resp, &e)
		if resp.StatusCode != tc.code || (tc.message != "" && e.Message != tc.message) {
			t.Errorf("%s = %d %+v", name, resp.StatusCode, e)
		}
	}
	// Outside cookie mode, the cookie is not read.
	plain := serve(t, newIdentity(t, Config{}))
	tok := register(t, plain, "bob@test.com", "password123")
	resp := get(plain, "/auth/me", []*http.Cookie{{Name: "chok_token", Value: tok}})
	var e envelope
	decodeBody(t, resp, &e)
	if resp.StatusCode != http.StatusUnauthorized || e.Message != "a bearer token is required" {
		t.Fatalf("a cookie outside cookie mode = %d %+v", resp.StatusCode, e)
	}
	if resp := post(t, plain, "/auth/logout", nil, nil, bearerHeader(tok)...); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("logout outside cookie mode = %d", resp.StatusCode)
	}
}

// TestCookie_Logout: logout clears the cookie with its own attributes,
// answers 204 and revokes nothing; it needs a session.
func TestCookie_Logout(t *testing.T) {
	_, _, c := cookieIdentity(t, CookieConfig{Name: "sid", Domain: "example.test", Path: "/app", SameSite: "strict"})
	resp := post(t, c, "/auth/register", map[string]string{"email": "alice@test.com", "password": "password123"}, nil)
	ck := cookie(resp.Cookies(), "sid")
	if ck == nil || ck.Domain != "example.test" || ck.Path != "/app" || ck.SameSite != http.SameSiteStrictMode || !ck.Secure || !ck.HttpOnly {
		t.Fatalf("a configured cookie: %q", resp.Header.Get("Set-Cookie"))
	}
	out := post(t, c, "/auth/logout", nil, []*http.Cookie{ck})
	cleared := cookie(out.Cookies(), "sid")
	if out.StatusCode != http.StatusNoContent || cleared == nil || cleared.Value != "" || cleared.MaxAge >= 0 ||
		cleared.Domain != "example.test" || cleared.Path != "/app" || cleared.SameSite != http.SameSiteStrictMode || !cleared.HttpOnly || !cleared.Secure {
		t.Fatalf("logout = %d %q", out.StatusCode, out.Header.Get("Set-Cookie"))
	}
	if !strings.Contains(out.Header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("the browser drops it at once: %q", out.Header.Get("Set-Cookie"))
	}
	// Stateless tokens: the token still works until it expires.
	if code := get(c, "/auth/me", []*http.Cookie{ck}).StatusCode; code != http.StatusOK {
		t.Fatalf("the token after logout = %d", code)
	}
	if code := post(t, c, "/auth/logout", nil, nil).StatusCode; code != http.StatusUnauthorized {
		t.Fatalf("logout without a session = %d", code)
	}
}

// TestCookie_Config: same_site none needs secure; the defaults; a cookie
// the browser cannot carry fails.
func TestCookie_Config(t *testing.T) {
	off := false
	for name, c := range map[string]struct {
		cookie CookieConfig
		want   string
	}{
		"none without secure": {CookieConfig{SameSite: "none", Secure: &off}, "cookie.same_site none needs cookie.secure"},
		"a bad name":          {CookieConfig{Name: "a b"}, "cookie.name"},
		"a relative path":     {CookieConfig{Path: "app"}, "cookie.path"},
		"a bad domain":        {CookieConfig{Domain: "exa mple.test"}, "cookie.domain"},
	} {
		cfg := Config{SigningKey: testKey, Cookie: c.cookie}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
	on := true
	for _, ok := range []CookieConfig{{SameSite: "none"}, {SameSite: "none", Secure: &on}, {SameSite: "strict", Secure: &off}} {
		cfg := Config{SigningKey: testKey, Cookie: ok}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	if _, err := New(Config{SigningKey: testKey, Cookie: CookieConfig{SameSite: "sometimes"}}, newIdentity(t, Config{}).d); err == nil || !strings.Contains(err.Error(), "same_site") {
		t.Fatalf("an unknown same_site: %v", err)
	}
	idn := newIdentity(t, Config{})
	if k := idn.cfg.Cookie; k.Enabled || k.Name != "chok_token" || k.Path != "/" || k.SameSite != "lax" || k.Secure != nil || !k.secure() || k.Only {
		t.Fatalf("the defaults in code: %+v", k)
	}
	// Secure off, for plain http on localhost.
	_, _, c := cookieIdentity(t, CookieConfig{Secure: &off})
	resp := post(t, c, "/auth/register", map[string]string{"email": "alice@test.com", "password": "password123"}, nil)
	if ck := cookie(resp.Cookies(), "chok_token"); ck == nil || ck.Secure {
		t.Fatalf("secure: false: %q", resp.Header.Get("Set-Cookie"))
	}
}

// TestCookie_OpenAPI: cookie mode documents the cookie as cookieAuth, an
// apiKey in the cookie, next to the bearer token on every route not
// declared public.
func TestCookie_OpenAPI(t *testing.T) {
	_, _, c := cookieIdentity(t, CookieConfig{Name: "sid"})
	var raw map[string]any
	if code := c.GetJSON("/openapi.json", &raw); code != http.StatusOK {
		t.Fatalf("openapi = %d", code)
	}
	b, _ := json.Marshal(raw["components"].(map[string]any)["securitySchemes"], json.Deterministic(true))
	if string(b) != `{"bearerAuth":{"bearerFormat":"JWT","scheme":"bearer","type":"http"},"cookieAuth":{"in":"cookie","name":"sid","type":"apiKey"}}` {
		t.Fatalf("securitySchemes %s", b)
	}
	var s spec
	c.GetJSON("/openapi.json", &s)
	for _, route := range [][2]string{{"get", "/auth/me"}, {"post", "/auth/logout"}, {"post", "/auth/refresh-token"}} {
		o := s.Paths[route[1]][route[0]]
		if o == nil || len(o.Security) != 2 || o.Security[0]["bearerAuth"] == nil || o.Security[1]["cookieAuth"] == nil {
			t.Errorf("%v: %+v", route, o)
		}
	}
	if o := s.Paths["/auth/logout"]["post"]; o == nil || o.Summary != "Log out" || !slices.Equal(o.Tags, []string{"auth"}) {
		t.Errorf("logout: %+v", o)
	}
	for _, route := range [][2]string{{"post", "/auth/login"}, {"post", "/auth/passwordless/verify"}} {
		if o := s.Paths[route[1]][route[0]]; o == nil || o.Security == nil || len(o.Security) != 0 {
			t.Errorf("%v stays public: %+v", route, o)
		}
	}
}
