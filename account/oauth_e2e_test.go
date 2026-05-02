package account_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zynthara/chok/account"
	"github.com/zynthara/chok/account/internal/testfake"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/log"
)

const e2eSigningKey = "this-is-a-test-signing-key-32bytes!"

func init() { gin.SetMode(gin.TestMode) }

// e2eFixture bundles the Module + router + provider + shared session
// store so each test can reach for the bits it needs.
type e2eFixture struct {
	m       *account.Module
	r       *gin.Engine
	p       *testfake.Provider
	mem     *account.MemorySessionStore
	store   account.OAuthSessionStore
	authCS  account.AuthCodeStore
}

func setupOAuthFixture(t *testing.T, providerName string, modOpts ...account.Option) *e2eFixture {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background(), gdb, account.Table(), account.IdentityTable()); err != nil {
		t.Fatal(err)
	}

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

	m, err := account.New(gdb, log.Empty(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	p := testfake.New(providerName)
	if err := m.RegisterProvider(p); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	m.RegisterRoutes(r)
	return &e2eFixture{
		m: m, r: r, p: p, mem: mem,
		store: mem, authCS: account.NewMemoryAuthCodeStore(mem),
	}
}

func send(r *gin.Engine, method, path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func sendJSON(r *gin.Engine, method, path string, body any, cookies []*http.Cookie) *httptest.ResponseRecorder {
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

	w = sendJSON(fx.r, "POST", "/auth/exchange", map[string]string{"code": authCode}, nil)
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

	if w := sendJSON(fx.r, "POST", "/auth/exchange", map[string]string{"code": authCode}, nil); w.Code != http.StatusOK {
		t.Fatalf("first exchange: %d %s", w.Code, w.Body.String())
	}
	if w := sendJSON(fx.r, "POST", "/auth/exchange", map[string]string{"code": authCode}, nil); w.Code != http.StatusGone {
		t.Fatalf("replay: expected 410, got %d %s", w.Code, w.Body.String())
	}
}

func TestOAuth_NoEmail_422(t *testing.T) {
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

	w := sendJSON(fx.r, "POST", "/login", map[string]string{
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
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background(), gdb, account.Table(), account.IdentityTable()); err != nil {
		t.Fatal(err)
	}
	mem := account.NewMemorySessionStore()
	defer mem.Close()
	m, err := account.New(gdb, log.Empty(),
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
