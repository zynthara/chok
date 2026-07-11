package account_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/account"
	"github.com/zynthara/chok/v2/account/internal/testfake"
	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/internal/clientip"
	"github.com/zynthara/chok/v2/internal/testschema"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/middleware"
	"github.com/zynthara/chok/v2/store"
	"github.com/zynthara/chok/v2/store/where"
)

func userByEmail(email string) store.Locator {
	return store.Where(where.WithFilter("email", email))
}

const moduleYAML = `
db:
  driver: sqlite
  sqlite:
    path: ":memory:"
account:
  signing_key: this-is-a-test-signing-key-32bytes!
`

func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestModule_MountsAuthSurface_RegisterLoginFlow(t *testing.T) {
	tk := choktest.NewTestKernel(t, moduleYAML, db.Module(), account.Module())

	reg, ok := tk.Router.Handler(http.MethodPost, "/auth/register")
	if !ok {
		t.Fatal("POST /auth/register not mounted under the /auth group")
	}
	w := postJSON(t, reg, "/auth/register", map[string]string{
		"email": "alice@test.com", "password": "password123", "name": "Alice",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", w.Code, w.Body.String())
	}

	login, _ := tk.Router.Handler(http.MethodPost, "/auth/login")
	w = postJSON(t, login, "/auth/login", map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Token == "" {
		t.Fatalf("login response missing token: %s", w.Body.String())
	}
}

func TestModule_TablesCreatedAtMigrate_BackfillIncluded(t *testing.T) {
	component := account.Module()
	tk := choktest.NewTestKernel(t, moduleYAML, db.Module(), component)
	testschema.AssertOwnership(t, db.From(tk), component)
}

func TestModule_MigrateOff_SchemaUntouched(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
db:
  driver: sqlite
  migrate: off
  sqlite:
    path: ":memory:"
account:
  signing_key: this-is-a-test-signing-key-32bytes!
`, db.Module(), account.Module())
	gdb := db.From(tk).Unsafe(t.Context())
	if gdb.Migrator().HasTable("users") || gdb.Migrator().HasTable("identities") {
		t.Fatal("migrate off must leave the account schema untouched")
	}
}

func TestModule_ReadOnlyDBFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account-readonly.db")
	h, err := db.Open(db.Options{Driver: "sqlite", SQLite: db.SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	_, err = choktest.StartKernel(t, fmt.Sprintf(`
db:
  driver: sqlite
  read_only: true
  sqlite: {path: %q}
account:
  signing_key: this-is-a-test-signing-key-32bytes!
`, path), db.Module(), account.Module())
	if err == nil || !strings.Contains(err.Error(), "account requires a writable database") {
		t.Fatalf("want account read-only fail-fast, got %v", err)
	}
}

func TestModule_Disabled_NotVisible_AuthnPanics(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
db:
  driver: sqlite
  sqlite:
    path: ":memory:"
account:
  enabled: false
`, db.Module(), account.Module())

	if _, ok := kernel.Get[*account.Component](tk, "account"); ok {
		t.Fatal("disabled account must not be reachable via kernel.Get")
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("account.Authn must panic with guidance when the module is disabled")
		} else if !strings.Contains(fmt.Sprint(r), "account.Module()") {
			t.Fatalf("panic should carry assembly guidance, got: %v", r)
		}
	}()
	_ = account.Authn(tk)
}

func TestModule_ShortSigningKey_FailsStartup(t *testing.T) {
	_, err := choktest.StartKernel(t, `
db:
  driver: sqlite
  sqlite:
    path: ":memory:"
account:
  signing_key: too-short
`, db.Module(), account.Module())
	if err == nil {
		t.Fatal("expected startup failure for a short signing key")
	}
	if !strings.Contains(err.Error(), "signing_key") {
		t.Fatalf("error should name signing_key, got: %v", err)
	}
}

// --- provider assembly × config matrix ----------------------------------

func TestModule_ProviderEnabledButNotAssembled_FailsStartup(t *testing.T) {
	_, err := choktest.StartKernel(t, moduleYAML+`
  oauth_callback_frontend_url: https://app.example.test/auth/finish
  providers:
    google:
      enabled: true
`, db.Module(), account.Module())
	if err == nil {
		t.Fatal("expected startup failure: enabled provider with no assembled spec")
	}
	if !strings.Contains(err.Error(), "not assembled") {
		t.Fatalf("error should say the provider is not assembled, got: %v", err)
	}
}

func TestModule_ProviderAssembledAndEnabled_MountsRoutes(t *testing.T) {
	tk := choktest.NewTestKernel(t, moduleYAML+`
  oauth_callback_frontend_url: https://app.example.test/auth/finish
  allowed_redirect_backs: ["https://app.example.test/"]
  providers:
    fake:
      enabled: true
`, db.Module(), account.Module(account.WithProviders(testfake.Spec("fake"))))

	start, ok := tk.Router.Handler(http.MethodGet, "/auth/fake/start")
	if !ok {
		t.Fatal("GET /auth/fake/start not mounted for the enabled provider")
	}
	rec := httptest.NewRecorder()
	start.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/fake/start", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("start = %d: %s", rec.Code, rec.Body.String())
	}
	// Allowlisted absolute redirect_back passes through (config landed).
	rec = httptest.NewRecorder()
	start.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/auth/fake/start?redirect_back=https://app.example.test/post-login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("allowlisted redirect_back rejected: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := tk.Router.Handler(http.MethodPost, "/auth/exchange"); !ok {
		t.Fatal("POST /auth/exchange not mounted despite an active provider")
	}
}

func TestModule_ProviderAssembledButNotEnabled_Skipped(t *testing.T) {
	tk := choktest.NewTestKernel(t, moduleYAML,
		db.Module(), account.Module(account.WithProviders(testfake.Spec("fake"))))

	if _, ok := tk.Router.Handler(http.MethodGet, "/auth/fake/start"); ok {
		t.Fatal("assembled-but-not-enabled provider must not mount routes (yaml kill switch)")
	}
}

// --- account.Authn end to end -------------------------------------------

func TestModule_Authn_ProtectsApplicationRoutes(t *testing.T) {
	tk := choktest.NewTestKernel(t, moduleYAML, db.Module(), account.Module())

	reg, _ := tk.Router.Handler(http.MethodPost, "/auth/register")
	w := postJSON(t, reg, "/auth/register", map[string]string{
		"email": "alice@test.com", "password": "password123",
	})
	var resp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	protected := account.Authn(tk)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	// No token: 401.
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", rec.Code)
	}

	// Valid token: through (ActiveCheck consulted the live user row).
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+resp.Token)
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated = %d: %s", rec.Code, rec.Body.String())
	}

	// Disable the user: the same token must now be rejected (the
	// AuthChain revocation semantics ride account.Authn).
	ac, _ := kernel.Get[*account.Component](tk, "account")
	user, err := ac.Service().Store().Get(t.Context(), userByEmail("alice@test.com"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.Service().SetUserActive(t.Context(), user.RID, false); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled user = %d, want 401 (ActiveCheck must gate)", rec.Code)
	}
}

// --- login rate limit: forged XFF cannot bypass (M2 clientip input) -------

func TestModule_LoginRateLimit_ForgedXFFCannotBypass(t *testing.T) {
	tk := choktest.NewTestKernel(t, moduleYAML+`
  login_rate_window: 15m
  login_rate_limit: 3
`, db.Module(), account.Module())

	login, _ := tk.Router.Handler(http.MethodPost, "/auth/login")

	// Production wiring: the web module resolves the client IP through
	// the trusted-proxy chain before the handler runs. With no trusted
	// proxies configured, X-Forwarded-For is attacker-controlled input
	// and must be ignored — the limiter keys on the direct peer.
	resolver, err := clientip.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	chain := middleware.ClientIP(resolver)(login)

	attempt := func(xff string) *httptest.ResponseRecorder {
		buf, _ := json.Marshal(map[string]string{"email": "ghost@test.com", "password": "wrong-password"})
		req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.7:4444" // same direct peer every time
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		return rec
	}

	// Three failures, each with a different forged XFF.
	for i := 0; i < 3; i++ {
		if rec := attempt(fmt.Sprintf("10.0.0.%d", i)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	// Fourth: 429 despite yet another forged XFF — the rotation bought
	// the attacker nothing.
	rec := attempt("10.9.9.9")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("post-threshold attempt = %d, want 429 (forged XFF must not bypass the limiter)", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
}

// --- §12.9: sensitive config redaction ------------------------------------

func TestModule_RedactedSettingsMaskSecrets(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
db:
  driver: sqlite
  sqlite:
    path: ":memory:"
account:
  signing_key: super-secret-signing-key-32bytes!!!
  oauth_callback_frontend_url: https://app.example.test/auth/finish
  providers:
    fake:
      enabled: true
      client_secret: provider-client-secret-value
`, db.Module(), account.Module(account.WithProviders(testfake.Spec("fake"))))

	dump := fmt.Sprintf("%v", tk.Config().RedactedSettings())
	if strings.Contains(dump, "super-secret-signing-key-32bytes!!!") {
		t.Fatalf("RedactedSettings leaked account.signing_key: %s", dump)
	}
	if strings.Contains(dump, "provider-client-secret-value") {
		t.Fatalf("RedactedSettings leaked the provider client_secret: %s", dump)
	}
}
