package account_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/account"
	"github.com/zynthara/chok/v2/account/internal/testfake"
	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store"
	"github.com/zynthara/chok/v2/store/where"
)

const e2eSigningKey = "this-is-a-test-signing-key-32bytes!"

// openE2EHandle opens an isolated in-memory database with the account
// schema migrated (SoftUnique email index included).
func openE2EHandle(t *testing.T) *db.DB {
	t.Helper()
	h, err := db.Open(db.Options{Driver: "sqlite", SQLite: db.SQLiteOptions{Path: ":memory:"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if err := h.Migrate(context.Background(), account.Table(), account.IdentityTable()); err != nil {
		t.Fatal(err)
	}
	return h
}

// e2eFixture bundles the Module + router + provider + shared session
// store so each test can reach for the bits it needs.
type e2eFixture struct {
	h      *db.DB
	m      *account.Service
	r      *choktest.ServeRouter
	p      *testfake.Provider
	mem    *account.MemorySessionStore
	store  account.OAuthSessionStore
	authCS account.AuthCodeStore
}

func setupOAuthFixture(t *testing.T, providerName string, modOpts ...account.Option) *e2eFixture {
	t.Helper()
	h := openE2EHandle(t)

	mem := account.NewMemorySessionStore()
	t.Cleanup(func() { _ = mem.Close() })

	opts := []account.Option{
		account.WithSigningKey(e2eSigningKey),
		account.WithOAuthCallbackFrontendURL("https://app.example.test/auth/finish"),
		account.WithSessionCarrier(account.NewCookieCarrier(
			[]byte("oauth-test-secret-32bytes-padded!!"), "_chok_oauth_sid", account.WithDevMode())),
		account.WithOAuthSessionStore(mem),
		account.WithAuthCodeStore(account.NewMemoryAuthCodeStore(mem)),
	}
	opts = append(opts, modOpts...)

	m, err := account.New(h, log.Empty(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	p := testfake.New(providerName)
	if err := m.RegisterProvider(p); err != nil {
		t.Fatal(err)
	}
	r := choktest.NewServeRouter()
	// Mount under /auth to match the account module's production
	// wiring — RegisterRoutes registers relative paths (/{name}/start,
	// /exchange, /identities) that combine with the group prefix to
	// yield the SPEC §7 absolute URLs.
	m.RegisterRoutes(r.Group("/auth"))
	return &e2eFixture{
		h: h, m: m, r: r, p: p, mem: mem,
		store: mem, authCS: account.NewMemoryAuthCodeStore(mem),
	}
}

func send(r http.Handler, method, path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func sendJSON(r http.Handler, method, path string, body any, cookies []*http.Cookie) *httptest.ResponseRecorder {
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func extractStateFromIdPLoc(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state")
}

func extractCodeFromFEloc(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("code")
}

func sidFromCookies(cookies []*http.Cookie) string {
	for _, ck := range cookies {
		if ck.Name == "_chok_oauth_sid" {
			dot := strings.LastIndex(ck.Value, ".")
			if dot > 0 {
				return ck.Value[:dot]
			}
		}
	}
	return ""
}

// hasCookie reports whether cookies contains a Set-Cookie write for the
// given name with a non-empty value (i.e. an active issue, not a clear).
func hasCookie(cookies []*http.Cookie, name string) bool {
	for _, ck := range cookies {
		if ck.Name == name && ck.Value != "" {
			return true
		}
	}
	return false
}

// hasClearedCookie reports whether cookies contains a Set-Cookie write
// for the given name with MaxAge<=0 (delete-cookie response).
func hasClearedCookie(cookies []*http.Cookie, name string) bool {
	for _, ck := range cookies {
		if ck.Name == name && ck.MaxAge < 0 {
			return true
		}
	}
	return false
}

// --- Tests ---

func TestOAuth_BeginCallbackExchange(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	fx.p.SeedIdentity("code-1", &account.ProviderIdentity{
		Provider:          "fake",
		ProviderAccountID: "fake-acct-1",
		Email:             "alice@idp.test",
		EmailVerified:     true,
		Name:              "Alice",
	})

	w := send(fx.r, "GET", "/auth/fake/start?redirect_back=/dashboard", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("start: expected 302, got %d: %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	state := extractStateFromIdPLoc(t, w.Header().Get("Location"))

	w = send(fx.r, "GET", "/auth/fake/callback?code=code-1&state="+url.QueryEscape(state), cookies)
	if w.Code != http.StatusFound {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	authCode := extractCodeFromFEloc(t, w.Header().Get("Location"))
	if authCode == "" {
		t.Fatal("no auth code")
	}

	// callback writes the exchange-binding cookie; forward it to the
	// exchange request to prove the same-browser invariant.
	xchgCookies := w.Result().Cookies()
	if !hasCookie(xchgCookies, "_chok_oauth_xchg") {
		t.Fatal("callback must Set-Cookie _chok_oauth_xchg")
	}

	w = sendJSON(fx.r, "POST", "/auth/exchange", map[string]string{"code": authCode}, xchgCookies)
	if w.Code != http.StatusOK {
		t.Fatalf("exchange: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Token        string    `json:"token"`
		ExpiresAt    time.Time `json:"expires_at"`
		RedirectBack string    `json:"redirect_back"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" || resp.RedirectBack != "/dashboard" {
		t.Fatalf("bad exchange response: %+v", resp)
	}
	// Successful exchange clears the binding cookie so a leaked browser
	// profile cannot replay it offline.
	if !hasClearedCookie(w.Result().Cookies(), "_chok_oauth_xchg") {
		t.Fatal("exchange must clear _chok_oauth_xchg on success")
	}
}

// TestOAuth_Exchange_RejectsMissingBinding covers the High #2 regression:
// an attacker who scrapes the auth_code from a redirect URL / Referer /
// access log MUST NOT be able to exchange it for a JWT without also
// possessing the HttpOnly _chok_oauth_xchg cookie that callback wrote.
func TestOAuth_Exchange_RejectsMissingBinding(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	fx.p.SeedIdentity("c", &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "x", Email: "x@idp.test", EmailVerified: true,
	})
	startResp := send(fx.r, "GET", "/auth/fake/start", nil)
	state := extractStateFromIdPLoc(t, startResp.Header().Get("Location"))
	cb := send(fx.r, "GET", "/auth/fake/callback?code=c&state="+url.QueryEscape(state), startResp.Result().Cookies())
	authCode := extractCodeFromFEloc(t, cb.Header().Get("Location"))

	// Submit exchange without forwarding the binding cookie: simulates a
	// stolen-code attacker who lacks the legitimate browser's cookie jar.
	w := sendJSON(fx.r, "POST", "/auth/exchange", map[string]string{"code": authCode}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (missing binding), got %d: %s", w.Code, w.Body.String())
	}
}

// TestOAuth_Exchange_RejectsTamperedBinding covers a related vector:
// attacker has BOTH the code AND a binding cookie value that doesn't
// match (e.g. their own browser's previous session). 401, not 200.
func TestOAuth_Exchange_RejectsTamperedBinding(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	fx.p.SeedIdentity("c", &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "x", Email: "x@idp.test", EmailVerified: true,
	})
	startResp := send(fx.r, "GET", "/auth/fake/start", nil)
	state := extractStateFromIdPLoc(t, startResp.Header().Get("Location"))
	cb := send(fx.r, "GET", "/auth/fake/callback?code=c&state="+url.QueryEscape(state), startResp.Result().Cookies())
	authCode := extractCodeFromFEloc(t, cb.Header().Get("Location"))

	tampered := []*http.Cookie{{Name: "_chok_oauth_xchg", Value: "definitely-not-the-right-token"}}
	w := sendJSON(fx.r, "POST", "/auth/exchange", map[string]string{"code": authCode}, tampered)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (tampered binding), got %d: %s", w.Code, w.Body.String())
	}
}

func TestOAuth_RedirectBack_RelativeOK(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	w := send(fx.r, "GET", "/auth/fake/start?redirect_back=/posts/123", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
}

func TestOAuth_RedirectBack_AbsoluteRejected(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	w := send(fx.r, "GET", "/auth/fake/start?redirect_back=https://evil.com/", nil)
	if w.Code == http.StatusFound {
		t.Fatal("open-redirect not blocked")
	}
}

func TestOAuth_RedirectBack_AllowlistOK(t *testing.T) {
	fx := setupOAuthFixture(t, "fake",
		account.WithAllowedRedirectBacks("https://app.example.test/"))
	w := send(fx.r, "GET", "/auth/fake/start?redirect_back=https://app.example.test/post-login", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("allowlisted absolute URL: expected 302, got %d", w.Code)
	}
}

func TestOAuth_RedirectBack_ProtocolRelativeRejected(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	w := send(fx.r, "GET", "/auth/fake/start?redirect_back=//evil.com/x", nil)
	if w.Code == http.StatusFound {
		t.Fatal("protocol-relative URL must be rejected")
	}
}

func TestOAuth_StateMismatch_400(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	fx.p.SeedIdentity("c", &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "x", Email: "x@idp.test", EmailVerified: true,
	})
	w := send(fx.r, "GET", "/auth/fake/start", nil)
	cookies := w.Result().Cookies()
	w = send(fx.r, "GET", "/auth/fake/callback?code=c&state=tampered", cookies)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("state mismatch: expected 400, got %d", w.Code)
	}
}

func TestOAuth_SessionExpired_410(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	fx.p.SeedIdentity("c", &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "x", Email: "x@idp.test", EmailVerified: true,
	})

	// Missing sid cookie → 400 (sid carrier read fails first).
	w := send(fx.r, "GET", "/auth/fake/callback?code=c&state=anything", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing sid: expected 400, got %d", w.Code)
	}

	// Valid sid but session pre-consumed by direct Take → 410.
	startResp := send(fx.r, "GET", "/auth/fake/start", nil)
	cookies := startResp.Result().Cookies()
	state := extractStateFromIdPLoc(t, startResp.Header().Get("Location"))
	sid := sidFromCookies(cookies)
	if _, err := fx.store.Take(context.Background(), sid); err != nil {
		t.Fatal(err)
	}

	w = send(fx.r, "GET", "/auth/fake/callback?code=c&state="+url.QueryEscape(state), cookies)
	if w.Code != http.StatusGone {
		t.Fatalf("expired session: expected 410, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOAuth_AuthCodeReplay_410(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	fx.p.SeedIdentity("c", &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "x", Email: "x@idp.test", EmailVerified: true,
	})

	startResp := send(fx.r, "GET", "/auth/fake/start", nil)
	cookies := startResp.Result().Cookies()
	state := extractStateFromIdPLoc(t, startResp.Header().Get("Location"))
	cb := send(fx.r, "GET", "/auth/fake/callback?code=c&state="+url.QueryEscape(state), cookies)
	authCode := extractCodeFromFEloc(t, cb.Header().Get("Location"))
	xchgCookies := cb.Result().Cookies()

	// First exchange: succeed, code is consumed.
	if w := sendJSON(fx.r, "POST", "/auth/exchange", map[string]string{"code": authCode}, xchgCookies); w.Code != http.StatusOK {
		t.Fatalf("first exchange: %d %s", w.Code, w.Body.String())
	}
	// Second exchange with the same binding cookie: code already taken
	// → 410 (regardless of whether the cookie is still valid). The
	// AuthCodeStore.Take is the gate; binding-check happens before but
	// for this scenario both are valid yet the code is already consumed.
	if w := sendJSON(fx.r, "POST", "/auth/exchange", map[string]string{"code": authCode}, xchgCookies); w.Code != http.StatusGone {
		t.Fatalf("replay: expected 410, got %d %s", w.Code, w.Body.String())
	}
}

// TestOAuth_NoEmail_InvalidArg verifies SPEC §6.2 / §8.1 's OAuth-only
// account creation gate: provider must supply non-empty + verified +
// non-aliased email or the callback returns 400 InvalidArgument with
// reason=OAUTH_EMAIL_REQUIRED. SPEC §8.1 originally documented 422
// Unprocessable Entity, but §6.2's pseudocode (canonical) returns
// ErrInvalidArgument(400) — the implementation follows §6.2 and the
// SPEC §8.1 422 mention is a documentation residue from earlier drafts.
func TestOAuth_NoEmail_InvalidArg(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	fx.p.SeedIdentity("c", &account.ProviderIdentity{Provider: "fake", ProviderAccountID: "x", Email: ""})

	startResp := send(fx.r, "GET", "/auth/fake/start", nil)
	cookies := startResp.Result().Cookies()
	state := extractStateFromIdPLoc(t, startResp.Header().Get("Location"))
	w := send(fx.r, "GET", "/auth/fake/callback?code=c&state="+url.QueryEscape(state), cookies)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "OAUTH_EMAIL_REQUIRED") {
		t.Fatalf("expected OAUTH_EMAIL_REQUIRED in body: %s", w.Body.String())
	}
}

func TestOAuth_AliasedEmail_NoAutoLink(t *testing.T) {
	fx := setupOAuthFixture(t, "fake", account.WithLinkByEmail(true))

	// Pre-create a verified-email local user. Use Module.Store() — public
	// surface; the email field is in publicStore's whitelist.
	if err := fx.m.Store().Create(context.Background(), &account.User{
		Email: "alice@idp.test", EmailVerified: true, PasswordHash: "x", Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	fx.p.SeedIdentity("c", &account.ProviderIdentity{
		Provider:          "fake",
		ProviderAccountID: "fake-acct-1",
		Email:             "alice@idp.test",
		EmailVerified:     true,
		IsAliasedEmail:    true,
	})

	startResp := send(fx.r, "GET", "/auth/fake/start", nil)
	cookies := startResp.Result().Cookies()
	state := extractStateFromIdPLoc(t, startResp.Header().Get("Location"))
	w := send(fx.r, "GET", "/auth/fake/callback?code=c&state="+url.QueryEscape(state), cookies)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("aliased email: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "non_aliased") {
		t.Fatalf("expected missing=non_aliased in details: %s", w.Body.String())
	}
}

func TestUnlinkIdentity_LastMethodRejected(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	user, _, err := fx.m.ResolveOAuthIdentity(context.Background(), &account.ProviderIdentity{
		Provider:          "fake",
		ProviderAccountID: "acc-1",
		Email:             "alice@idp.test",
		EmailVerified:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	idents, err := fx.m.ListIdentities(context.Background(), user.RID)
	if err != nil || len(idents) != 1 {
		t.Fatalf("expected 1 identity: %v %v", idents, err)
	}
	err = fx.m.UnlinkIdentity(context.Background(), user.RID, idents[0].RID)
	if err == nil {
		t.Fatal("expected error when unlinking last method")
	}
	// Identity still exists.
	idents2, _ := fx.m.ListIdentities(context.Background(), user.RID)
	if len(idents2) != 1 {
		t.Fatalf("identity should survive: %v", idents2)
	}
}

func TestUnlinkIdentity_OwnershipEnforced(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	userA, _, _ := fx.m.ResolveOAuthIdentity(context.Background(), &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "acc-A", Email: "a@idp.test", EmailVerified: true,
	})
	userB, _, _ := fx.m.ResolveOAuthIdentity(context.Background(), &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "acc-B", Email: "b@idp.test", EmailVerified: true,
	})
	// userA links a second identity (manual, with a synthetic provider id)
	// so it has 2 methods → can attempt to unlink without hitting the
	// last-method guard.
	if _, err := fx.m.LinkIdentity(context.Background(), userA.RID, &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "acc-A2", Email: "a@idp.test",
	}); err != nil {
		t.Fatal(err)
	}
	identsB, _ := fx.m.ListIdentities(context.Background(), userB.RID)
	err := fx.m.UnlinkIdentity(context.Background(), userA.RID, identsB[0].RID)
	if err == nil {
		t.Fatal("expected error when unlinking another user's identity")
	}
	identsB2, _ := fx.m.ListIdentities(context.Background(), userB.RID)
	if len(identsB2) != len(identsB) {
		t.Fatal("userB's identity must survive cross-user delete")
	}
}

func TestResolveOAuthIdentity_TransactionRollback(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	pi := &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "acc-1", Email: "alice@idp.test", EmailVerified: true,
	}
	user, _, err := fx.m.ResolveOAuthIdentity(context.Background(), pi)
	if err != nil || user == nil {
		t.Fatalf("setup: %v", err)
	}

	// Same email but a fresh (provider, acct) — User Create will fail
	// the unique-email constraint, transaction rolls back, no orphan
	// Identity row.
	_, _, err = fx.m.ResolveOAuthIdentity(context.Background(), &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "acc-2", Email: "alice@idp.test", EmailVerified: true,
	})
	if err == nil {
		t.Fatal("expected duplicate-email error")
	}
	// ListIdentities for the original user should show only acc-1.
	idents, _ := fx.m.ListIdentities(context.Background(), user.RID)
	if len(idents) != 1 || idents[0].ProviderAccountID != "acc-1" {
		t.Fatalf("rollback violated: %v", idents)
	}
}

func TestOAuthOnly_LoginRejected(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	user, _, err := fx.m.ResolveOAuthIdentity(context.Background(), &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "acc", Email: "alice@idp.test", EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if user.PasswordVersion != 0 {
		t.Fatalf("expected PV=0, got %d", user.PasswordVersion)
	}

	w := sendJSON(fx.r, "POST", "/auth/login", map[string]string{
		"email":    "alice@idp.test",
		"password": "anything-wrong",
	}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "OAUTH_ONLY_ACCOUNT") {
		t.Fatalf("expected OAUTH_ONLY_ACCOUNT in body: %s", w.Body.String())
	}
}

func TestProviderRegistration_RequiresFrontendURL(t *testing.T) {
	h := openE2EHandle(t)
	mem := account.NewMemorySessionStore()
	defer mem.Close()
	m, err := account.New(h, log.Empty(),
		account.WithSigningKey(e2eSigningKey),
		account.WithSessionCarrier(account.NewCookieCarrier(
			[]byte("oauth-test-secret-32bytes-padded!!"), "_chok_oauth_sid", account.WithDevMode())),
		account.WithOAuthSessionStore(mem),
		account.WithAuthCodeStore(account.NewMemoryAuthCodeStore(mem)),
	)
	if err != nil {
		t.Fatal(err)
	}
	// No WithOAuthCallbackFrontendURL → first RegisterProvider must fail.
	if err := m.RegisterProvider(testfake.New("fake")); err == nil {
		t.Fatal("expected error for missing OAuth callback frontend URL")
	}
}

func TestProviderRegistration_DuplicateRejected(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	if err := fx.m.RegisterProvider(testfake.New("fake")); err == nil {
		t.Fatal("expected error on duplicate provider name")
	}
}

func TestProviderRegistration_NilProviderRejected(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	if err := fx.m.RegisterProvider(nil); err == nil {
		t.Fatal("expected error for nil provider")
	}
}

// TestOAuth_LinkFlow_BindsToCurrentUser proves High #3: when an
// authenticated user calls POST /identities/link, the resulting Identity
// row attaches to *that* user, even if the IdP returns a different
// ProviderAccountID than any existing identity. Without the fix, the
// callback would run ResolveOAuthIdentity (creating a new chok user
// for the new IdP account) and exchange would issue a token for *that*
// user — silently switching the browser to a different account.
func TestOAuth_LinkFlow_BindsToCurrentUser(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	ctx := context.Background()

	// Bootstrap a password-mode user via /register and capture their token.
	regResp := sendJSON(fx.r, "POST", "/auth/register", map[string]string{
		"email":    "alice@local.test",
		"password": "Password!23",
		"name":     "Alice",
	}, nil)
	if regResp.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", regResp.Code, regResp.Body.String())
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(regResp.Body).Decode(&tok); err != nil {
		t.Fatal(err)
	}
	aliceUser, err := fx.m.Store().Get(ctx, store.Where(where.WithFilter("email", "alice@local.test")))
	if err != nil {
		t.Fatal(err)
	}

	// Seed the IdP's response — note the email differs from alice's local
	// email; without the fix the callback would create a new chok user
	// "carol" and switch alice's browser to her token.
	fx.p.SeedIdentity("idp-code", &account.ProviderIdentity{
		Provider:          "fake",
		ProviderAccountID: "remote-acc-99",
		Email:             "carol@idp.test",
		EmailVerified:     true,
	})

	// POST /auth/identities/link with alice's token. Returns redirect_to.
	linkReq := httptest.NewRequest("POST", "/auth/identities/link",
		bytes.NewReader([]byte(`{"provider":"fake","redirect_back":"/settings"}`)))
	linkReq.Header.Set("Content-Type", "application/json")
	linkReq.Header.Set("Authorization", "Bearer "+tok.Token)
	linkW := httptest.NewRecorder()
	fx.r.ServeHTTP(linkW, linkReq)
	if linkW.Code != http.StatusOK {
		t.Fatalf("link request: %d %s", linkW.Code, linkW.Body.String())
	}
	var linkResp struct {
		RedirectTo string `json:"redirect_to"`
	}
	if err := json.NewDecoder(linkW.Body).Decode(&linkResp); err != nil {
		t.Fatal(err)
	}
	startCookies := linkW.Result().Cookies()
	state := extractStateFromIdPLoc(t, linkResp.RedirectTo)

	// Browser bounces back to callback. Sid cookie set by /identities/link
	// carries LinkUserID = alice; the callback path goes through
	// LinkIdentity, NOT ResolveOAuthIdentity.
	cb := send(fx.r, "GET", "/auth/fake/callback?code=idp-code&state="+url.QueryEscape(state), startCookies)
	if cb.Code != http.StatusFound {
		t.Fatalf("callback: %d %s", cb.Code, cb.Body.String())
	}
	// Link flow does NOT issue an auth_code (no token exchange). The 302
	// goes to redirect_back (or frontend URL) with link_status=ok.
	loc := cb.Header().Get("Location")
	if !strings.Contains(loc, "link_status=ok") {
		t.Fatalf("link flow must redirect with link_status=ok, got %q", loc)
	}
	if strings.Contains(loc, "code=") {
		t.Fatalf("link flow must NOT mint an auth_code, got %q", loc)
	}

	// Verify the new Identity row is attached to alice, not to a brand
	// new "carol" user.
	idents, err := fx.m.ListIdentities(ctx, aliceUser.RID)
	if err != nil {
		t.Fatal(err)
	}
	if len(idents) != 1 || idents[0].ProviderAccountID != "remote-acc-99" {
		t.Fatalf("expected alice to have 1 identity (remote-acc-99), got %+v", idents)
	}
}

// TestUnlinkIdentity_RaceLeavesOneMethod is the High #4 regression:
// concurrent unlink calls on an OAuth-only user with two identities
// must not both succeed. The transaction + row-level lock guarantees
// at most one wins; the other returns FailedPrecondition.
//
// SQLite serializes writers within a transaction so on a single-
// connection :memory: backend the test reliably exhibits the
// serialization the production-grade lock provides on MySQL/PG.
func TestUnlinkIdentity_RaceLeavesOneMethod(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	ctx := context.Background()

	// OAuth-only user (PV=0) with two identities.
	user, _, err := fx.m.ResolveOAuthIdentity(ctx, &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "acc-1", Email: "alice@idp.test", EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.m.LinkIdentity(ctx, user.RID, &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "acc-2", Email: "alice@idp.test",
	}); err != nil {
		t.Fatal(err)
	}
	idents, _ := fx.m.ListIdentities(ctx, user.RID)
	if len(idents) != 2 {
		t.Fatalf("setup: expected 2 identities, got %d", len(idents))
	}

	// Concurrent unlinks: each goroutine targets a different identity.
	// At most one should succeed; otherwise the user would end up with
	// 0 login methods, an unrecoverable state.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = fx.m.UnlinkIdentity(ctx, user.RID, idents[idx].RID)
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, e := range errs {
		if e == nil {
			successes++
		}
	}
	if successes > 1 {
		t.Fatalf("UnlinkIdentity is not atomic: %d concurrent successes (expected ≤1)", successes)
	}
	final, _ := fx.m.ListIdentities(ctx, user.RID)
	if len(final) < 1 {
		t.Fatal("user left with 0 identities — last-method guard failed under concurrency")
	}
}

// TestOAuth_DevModeAutoDetect covers High #6: when the first registered
// AuthProvider implements RedirectURLProvider AND its URL is HTTP-on-
// localhost, RegisterProvider must propagate the hint into the default
// CookieCarrier so it picks SameSite=Lax / !Secure (otherwise browsers
// drop the sid cookie on plaintext localhost callbacks).
//
// We only verify the side effect that's reachable through the public
// API: a Set-Cookie header without "Secure" on the /auth/{name}/start
// response. The carrier's internal devMode bool is a private field —
// Set-Cookie attributes are the contract.
func TestOAuth_DevModeAutoDetect(t *testing.T) {
	h := openE2EHandle(t)
	// Note: no WithSessionCarrier — the service derives its default
	// carrier from signingKey + dev-mode flag from firstRedirectURL.
	m, err := account.New(h, log.Empty(),
		account.WithSigningKey(e2eSigningKey),
		account.WithOAuthCallbackFrontendURL("http://localhost:3000/auth/finish"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	p := testfake.New("fake").WithRedirectURL("http://localhost:8080/auth/fake/callback")
	if err := m.RegisterProvider(p); err != nil {
		t.Fatal(err)
	}
	r := choktest.NewServeRouter()
	m.RegisterRoutes(r.Group("/auth"))

	w := send(r, "GET", "/auth/fake/start", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("start: %d", w.Code)
	}
	setCookie := w.Header().Get("Set-Cookie")
	if strings.Contains(setCookie, "Secure") {
		t.Fatalf("dev-mode auto-detect failed: cookie still has Secure attribute: %s", setCookie)
	}
	if !strings.Contains(setCookie, "SameSite=Lax") {
		t.Fatalf("dev-mode auto-detect failed: cookie not SameSite=Lax: %s", setCookie)
	}
}

// TestOAuth_F1_PasswordUserSurvivesLink proves the F1 regression: a
// user who registered with /register MUST keep working with /login
// after they later bind an OAuth identity. Pre-fix the call chain
// `userHasPasswordHistory → PV>0 || len(idents)==0` flipped to false
// once an Identity row existed, so /login returned 401
// OAUTH_ONLY_ACCOUNT and ListLoginMethods dropped the password slot.
//
// User.HasPassword (set true at /register) is the explicit replacement
// signal. Test asserts:
//   - /login still returns 200 after link
//   - ListLoginMethods still includes the password slot
//   - UnlinkIdentity succeeds (last-method guard sees password + 0
//     remaining idents = 1 method left)
func TestOAuth_F1_PasswordUserSurvivesLink(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	ctx := context.Background()

	// Bootstrap a password user via /register so HasPassword is set
	// through the public route, not via direct store writes.
	regResp := sendJSON(fx.r, "POST", "/auth/register", map[string]string{
		"email":    "alice@local.test",
		"password": "Password!23",
	}, nil)
	if regResp.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", regResp.Code, regResp.Body.String())
	}
	user, err := fx.m.Store().Get(ctx, store.Where(where.WithFilter("email", "alice@local.test")))
	if err != nil {
		t.Fatal(err)
	}

	// Baseline: /login works before any link.
	loginPre := sendJSON(fx.r, "POST", "/auth/login", map[string]string{
		"email": "alice@local.test", "password": "Password!23",
	}, nil)
	if loginPre.Code != http.StatusOK {
		t.Fatalf("baseline /login pre-link: %d %s", loginPre.Code, loginPre.Body.String())
	}

	// Link an OAuth identity directly (covers the link flow's terminal
	// effect; the start→callback dance is exercised separately).
	if _, err := fx.m.LinkIdentity(ctx, user.RID, &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "remote-1", Email: "carol@idp.test",
	}); err != nil {
		t.Fatal(err)
	}

	// Post-link /login: still 200, NOT 401 OAUTH_ONLY_ACCOUNT.
	loginPost := sendJSON(fx.r, "POST", "/auth/login", map[string]string{
		"email": "alice@local.test", "password": "Password!23",
	}, nil)
	if loginPost.Code != http.StatusOK {
		t.Fatalf("post-link /login broken: %d %s — F1 not fixed",
			loginPost.Code, loginPost.Body.String())
	}

	// ListLoginMethods includes both password and OAuth.
	methods, err := fx.m.ListLoginMethods(ctx, user.RID)
	if err != nil {
		t.Fatal(err)
	}
	hasPassword := false
	hasOAuth := false
	for _, lm := range methods {
		switch lm.Type {
		case "password":
			hasPassword = true
		case "fake":
			hasOAuth = true
		}
	}
	if !hasPassword || !hasOAuth {
		t.Fatalf("methods missing entries; got %+v", methods)
	}

	// Last-method guard sees 2 methods → unlink the OAuth one and
	// the password slot is enough to keep the account recoverable.
	idents, _ := fx.m.ListIdentities(ctx, user.RID)
	if err := fx.m.UnlinkIdentity(ctx, user.RID, idents[0].RID); err != nil {
		t.Fatalf("unlink should succeed because password slot remains: %v", err)
	}
	// And /login still works after the unlink (full circle).
	loginAfter := sendJSON(fx.r, "POST", "/auth/login", map[string]string{
		"email": "alice@local.test", "password": "Password!23",
	}, nil)
	if loginAfter.Code != http.StatusOK {
		t.Fatalf("post-unlink /login: %d %s", loginAfter.Code, loginAfter.Body.String())
	}
}

// TestOAuth_F1_BackfillRescuesLegacyRows covers AccountComponent.Migrate's
// idempotent BackfillHasPassword: legacy rows that pre-date the
// has_password column (default false) but have a real password_hash
// and no Identity row must be flipped to has_password=true so they
// keep authenticating after the upgrade.
//
// We simulate "legacy" by directly writing has_password=false on a
// freshly-registered user, then call BackfillHasPassword and assert
// /login resumes working.
func TestOAuth_F1_BackfillRescuesLegacyRows(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	ctx := context.Background()

	// A normal /register sets has_password=true. Simulate "legacy"
	// state by writing has_password=false directly via the store.
	regResp := sendJSON(fx.r, "POST", "/auth/register", map[string]string{
		"email":    "legacy@local.test",
		"password": "Password!23",
	}, nil)
	if regResp.Code != http.StatusCreated {
		t.Fatalf("register: %d", regResp.Code)
	}
	gdb, err := fx.m.Store().Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(`UPDATE users SET has_password = FALSE WHERE email = ?`,
		"legacy@local.test").Error; err != nil {
		t.Fatal(err)
	}

	user, _ := fx.m.Store().Get(ctx, store.Where(where.WithFilter("email", "legacy@local.test")))
	if user.HasPassword {
		t.Fatal("setup invalid: legacy row should have has_password=false before backfill")
	}

	// Run backfill (this is what the account module's Migrate calls).
	if err := account.BackfillHasPassword(ctx, fx.h); err != nil {
		t.Fatal(err)
	}

	user2, err := fx.m.Store().Get(ctx, store.Where(where.WithFilter("email", "legacy@local.test")))
	if err != nil {
		t.Fatal(err)
	}
	if !user2.HasPassword {
		t.Fatal("backfill failed: legacy password user not flipped to has_password=true")
	}

	// Idempotent: second call must not error.
	if err := account.BackfillHasPassword(ctx, fx.h); err != nil {
		t.Fatal(err)
	}

	// And /login on the (now backfilled) legacy user works again.
	w := sendJSON(fx.r, "POST", "/auth/login", map[string]string{
		"email": "legacy@local.test", "password": "Password!23",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("post-backfill /login: %d %s", w.Code, w.Body.String())
	}
}

// TestOAuth_F1_BackfillSkipsOAuthOnlyUsers ensures the backfill does
// NOT bless users who have an OAuth Identity row — those are
// genuinely "no password history" accounts (or pre-fix linked
// password users; ambiguous and conservatively left at false). They
// recover via /forgot-password if needed.
func TestOAuth_F1_BackfillSkipsOAuthOnlyUsers(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	ctx := context.Background()

	// Create an OAuth-only user.
	user, _, err := fx.m.ResolveOAuthIdentity(ctx, &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "x", Email: "oauth@idp.test", EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if user.HasPassword {
		t.Fatal("OAuth-only Create should leave HasPassword=false")
	}

	// Run backfill — it must NOT touch this user (Identity row exists).
	if err := account.BackfillHasPassword(ctx, fx.h); err != nil {
		t.Fatal(err)
	}
	got, _ := fx.m.Store().Get(ctx, store.RID(user.RID))
	if got.HasPassword {
		t.Fatal("backfill incorrectly blessed OAuth-only user as having a password")
	}
}

// TestMigrateSchema_MigratesIdentityAndBackfills proves the canonical
// migration path (used by the account module's Migrator and any
// kernel-less embedder) migrates BOTH tables and runs the
// has_password backfill — legacy password users must not hit
// OAUTH_ONLY_ACCOUNT after upgrade.
func TestMigrateSchema_MigratesIdentityAndBackfills(t *testing.T) {
	h, err := db.Open(db.Options{Driver: "sqlite", SQLite: db.SQLiteOptions{Path: ":memory:"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	// Pre-create only the User table with a "legacy" password row that
	// pre-dates has_password. We seed via raw SQL to bypass the new
	// /register code path so has_password=0 (the legacy state).
	if err := h.Migrate(context.Background(), account.Table()); err != nil {
		t.Fatal(err)
	}
	if err := h.Unsafe(context.Background()).Exec(`
		INSERT INTO users (rid, email, password_hash, has_password, password_version, name, active, created_at, updated_at)
		VALUES ('usr_legacy01', 'legacy@local.test', '$2a$10$abcdefghijklmnopqrstuv', 0, 0, 'Legacy', 1, datetime('now'), datetime('now'))
	`).Error; err != nil {
		t.Fatal(err)
	}

	// Run the canonical path — it MUST migrate Identity AND flip
	// has_password=true on the legacy row.
	if err := account.MigrateSchema(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	m, err := account.New(h, log.Empty(), account.WithSigningKey(e2eSigningKey))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	// Identity table must now exist — listing on a non-existent table
	// errors out, so a successful (empty) list proves the migration ran.
	if _, err := m.ListIdentities(context.Background(), "usr_legacy01"); err != nil {
		t.Fatalf("Identity table missing after MigrateSchema: %v", err)
	}

	// Legacy password row must be backfilled to has_password=true.
	var got int
	if err := h.Unsafe(context.Background()).Raw(`SELECT has_password FROM users WHERE rid = 'usr_legacy01'`).
		Scan(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("BackfillHasPassword skipped: has_password=%d (expected 1)", got)
	}
}

// TestOAuth_DevModeMirroredFromCustomCarrier covers F3: when the
// caller supplies their own CookieCarrier with WithDevMode(), the
// /auth/exchange browser-binding cookie must mirror that posture so
// the cookie isn't dropped by browsers on HTTP localhost.
func TestOAuth_DevModeMirroredFromCustomCarrier(t *testing.T) {
	h := openE2EHandle(t)
	mem := account.NewMemorySessionStore()
	t.Cleanup(func() { _ = mem.Close() })
	m, err := account.New(h, log.Empty(),
		account.WithSigningKey(e2eSigningKey),
		account.WithOAuthCallbackFrontendURL("http://localhost:3000/auth/finish"),
		// Caller picks dev mode for the sid carrier; binding cookie
		// must follow regardless of what isLocalDevRedirect deduces.
		account.WithSessionCarrier(account.NewCookieCarrier(
			[]byte("oauth-test-secret-32bytes-padded!!"), "_chok_oauth_sid", account.WithDevMode())),
		account.WithOAuthSessionStore(mem),
		account.WithAuthCodeStore(account.NewMemoryAuthCodeStore(mem)),
	)
	if err != nil {
		t.Fatal(err)
	}
	p := testfake.New("fake")
	p.SeedIdentity("c", &account.ProviderIdentity{
		Provider: "fake", ProviderAccountID: "x", Email: "x@idp.test", EmailVerified: true,
	})
	if err := m.RegisterProvider(p); err != nil {
		t.Fatal(err)
	}
	r := choktest.NewServeRouter()
	m.RegisterRoutes(r.Group("/auth"))

	// Drive a full start → callback so callback writes the binding cookie.
	startW := send(r, "GET", "/auth/fake/start", nil)
	startCookies := startW.Result().Cookies()
	state := extractStateFromIdPLoc(t, startW.Header().Get("Location"))
	cb := send(r, "GET", "/auth/fake/callback?code=c&state="+url.QueryEscape(state), startCookies)
	if cb.Code != http.StatusFound {
		t.Fatalf("callback: %d %s", cb.Code, cb.Body.String())
	}

	var bindingCookieFound bool
	for _, h := range cb.Header().Values("Set-Cookie") {
		if !strings.HasPrefix(h, "_chok_oauth_xchg=") {
			continue
		}
		bindingCookieFound = true
		if strings.Contains(h, "Secure") {
			t.Errorf("dev-mode carrier set, but binding cookie still has Secure: %s", h)
		}
		if !strings.Contains(h, "SameSite=Lax") {
			t.Errorf("dev-mode carrier set, but binding cookie not SameSite=Lax: %s", h)
		}
	}
	if !bindingCookieFound {
		t.Fatal("binding cookie not issued")
	}
}

// TestModule_Close_ClosesAdapterStore covers Medium #1: when the user
// supplies WithOAuthSessionStore but not WithAuthCodeStore, Module
// constructs an internal MemorySessionStore behind a memoryAuthCodeAdapter.
// Module.Close must reach through the adapter to stop the cleanup
// goroutine, otherwise we leak one goroutine per Module instance.
//
// Calling Close twice asserts idempotency (sync.Once on the underlying
// MemorySessionStore.Close).
func TestModule_Close_ClosesAdapterStore(t *testing.T) {
	h := openE2EHandle(t)
	customStore := account.NewMemorySessionStore()
	m, err := account.New(h, log.Empty(),
		account.WithSigningKey(e2eSigningKey),
		account.WithOAuthCallbackFrontendURL("https://app.example.test/auth/finish"),
		account.WithOAuthSessionStore(customStore),
		// no WithAuthCodeStore → Module wraps an internal MemorySessionStore
		// in memoryAuthCodeAdapter; Close must reach it.
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterProvider(testfake.New("fake")); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent: second Close must not error.
	if err := m.Close(); err != nil {
		t.Fatalf("Close (second): %v", err)
	}
	// Closing the externally-supplied store after Module.Close must also
	// be safe (sync.Once guard inside MemorySessionStore.Close).
	if err := customStore.Close(); err != nil {
		t.Fatalf("custom store Close: %v", err)
	}
}

// TestOAuth_EmailNormalized covers Medium #2: ResolveOAuthIdentity
// normalizes (lower-case + trim) pi.Email so OAuth lookups share the
// same casing rule as /login, /register, /forgot-password. Without
// the fix, an IdP returning "Alice@idp.test" would create a chok user
// whose email differs from the lower-case form login expects.
func TestOAuth_EmailNormalized(t *testing.T) {
	fx := setupOAuthFixture(t, "fake")
	ctx := context.Background()

	user, _, err := fx.m.ResolveOAuthIdentity(ctx, &account.ProviderIdentity{
		Provider:          "fake",
		ProviderAccountID: "acc-mixed",
		Email:             "  Alice@IDP.test ",
		EmailVerified:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "alice@idp.test" {
		t.Fatalf("expected normalized email, got %q", user.Email)
	}
	idents, _ := fx.m.ListIdentities(ctx, user.RID)
	if len(idents) != 1 || idents[0].Email != "alice@idp.test" {
		t.Fatalf("identity email not normalized: %+v", idents)
	}
}
