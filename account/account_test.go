package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/middleware"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

const testSigningKey = "this-is-a-test-signing-key-32bytes!"

func init() { gin.SetMode(gin.TestMode) }

// --- helpers ---

func setupModule(t *testing.T, opts ...Option) (*Module, *gin.Engine) {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background(), gdb, Table()); err != nil {
		t.Fatal(err)
	}

	defaults := []Option{WithSigningKey(testSigningKey)}
	defaults = append(defaults, opts...)

	m, err := New(gdb, log.Empty(), defaults...)
	if err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	m.RegisterRoutes(r)
	return m, r
}

func doJSON(r *gin.Engine, method, path string, body any, token ...string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if len(token) > 0 && token[0] != "" {
		req.Header.Set("Authorization", "Bearer "+token[0])
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeToken(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp tokenResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("expected non-empty token")
	}
	return resp.Token
}

// --- Register tests ---

func TestRegister_Success(t *testing.T) {
	_, r := setupModule(t)

	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
		"name":     "Alice",
	})

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	decodeToken(t, w)
}

func TestRegister_EmptyName_UsesmaskedEmail(t *testing.T) {
	m, r := setupModule(t)

	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	user, err := m.store.Get(context.Background(), store.Where(where.WithFilter("email", "alice@test.com")))
	if err != nil {
		t.Fatal(err)
	}
	if user.Name != "a***e@t**t.com" {
		t.Fatalf("expected masked email name, got %q", user.Name)
	}
}

func TestMaskEmail(t *testing.T) {
	tests := []struct {
		email string
		want  string
	}{
		{"a@test.com", "a*@t**t.com"},
		{"ab@test.com", "a*b@t**t.com"},
		{"alice@test.com", "a***e@t**t.com"},
		{"john.doe@example.com", "j******e@e*****e.com"},
		{"alice@mail.test.co.jp", "a***e@m**l.t**t.co.jp"},
		{"bob@qq.com", "b*b@q*q.com"},
	}
	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			got := maskEmail(tt.email)
			if got != tt.want {
				t.Fatalf("maskEmail(%q) = %q, want %q", tt.email, got, tt.want)
			}
		})
	}
}

func TestRegister_DuplicateEmail(t *testing.T) {
	_, r := setupModule(t)

	body := map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	}
	doJSON(r, "POST", "/register", body)
	w := doJSON(r, "POST", "/register", body)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRegister_InvalidEmail(t *testing.T) {
	_, r := setupModule(t)

	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "not-an-email",
		"password": "password123",
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRegister_ShortPassword(t *testing.T) {
	_, r := setupModule(t)

	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "short",
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Login tests ---

func TestLogin_Success(t *testing.T) {
	_, r := setupModule(t)

	doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})

	w := doJSON(r, "POST", "/login", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	decodeToken(t, w)
}

func TestLogin_EmailCaseInsensitive(t *testing.T) {
	_, r := setupModule(t)

	doJSON(r, "POST", "/register", map[string]string{
		"email":    "Alice@Test.COM",
		"password": "password123",
	})

	// Login with different case should work.
	w := doJSON(r, "POST", "/login", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (case-insensitive login), got %d: %s", w.Code, w.Body.String())
	}

	// Registering same email with different case should conflict.
	w = doJSON(r, "POST", "/register", map[string]string{
		"email":    "ALICE@test.com",
		"password": "password123",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 (duplicate case-insensitive), got %d: %s", w.Code, w.Body.String())
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	_, r := setupModule(t)

	doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})

	w := doJSON(r, "POST", "/login", map[string]string{
		"email":    "alice@test.com",
		"password": "wrongpassword",
	})

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestLogin_NonexistentUser(t *testing.T) {
	_, r := setupModule(t)

	w := doJSON(r, "POST", "/login", map[string]string{
		"email":    "nobody@test.com",
		"password": "password123",
	})

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Refresh Token tests ---

func TestRefreshToken_Success(t *testing.T) {
	_, r := setupModule(t)

	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	token := decodeToken(t, w)

	w = doJSON(r, "POST", "/refresh-token", nil, token)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	newToken := decodeToken(t, w)
	if newToken == "" {
		t.Fatal("expected new token")
	}
}

func TestRefreshToken_NoAuth(t *testing.T) {
	_, r := setupModule(t)

	w := doJSON(r, "POST", "/refresh-token", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Change Password tests ---

func TestChangePassword_Success(t *testing.T) {
	_, r := setupModule(t)

	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	token := decodeToken(t, w)

	w = doJSON(r, "PUT", "/change-password", map[string]string{
		"old_password": "password123",
		"new_password": "newpassword456",
	}, token)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Login with new password should work.
	w = doJSON(r, "POST", "/login", map[string]string{
		"email":    "alice@test.com",
		"password": "newpassword456",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("login with new password: expected 200, got %d", w.Code)
	}

	// Login with old password should fail.
	w = doJSON(r, "POST", "/login", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("login with old password: expected 401, got %d", w.Code)
	}
}

func TestChangePassword_WrongOldPassword(t *testing.T) {
	_, r := setupModule(t)

	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	token := decodeToken(t, w)

	w = doJSON(r, "PUT", "/change-password", map[string]string{
		"old_password": "wrongpassword",
		"new_password": "newpassword456",
	}, token)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Forgot / Reset Password tests ---

type mockSender struct {
	mu       sync.Mutex
	lastTo   string
	lastCode string
	called   chan struct{}
}

func newMockSender() *mockSender {
	return &mockSender{called: make(chan struct{}, 1)}
}

func (s *mockSender) Send(_ context.Context, to, code string) error {
	s.mu.Lock()
	s.lastTo = to
	s.lastCode = code
	s.mu.Unlock()
	select {
	case s.called <- struct{}{}:
	default:
	}
	return nil
}

// waitCalled waits for the async forgot-password dispatch (see Module)
// to reach Send. Returns true on signal, false on timeout.
func (s *mockSender) waitCalled(timeout time.Duration) bool {
	select {
	case <-s.called:
		return true
	case <-time.After(timeout):
		return false
	}
}

// snapshot returns the last Send arguments under lock.
func (s *mockSender) snapshot() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTo, s.lastCode
}

func TestForgotAndResetPassword(t *testing.T) {
	sender := newMockSender()
	_, r := setupModule(t, WithSender(sender))

	// Register a user.
	doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})

	// Request password reset.
	w := doJSON(r, "POST", "/forgot-password", map[string]string{
		"email": "alice@test.com",
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("forgot: expected 204, got %d: %s", w.Code, w.Body.String())
	}
	// Send dispatches on a background goroutine to flatten timing so
	// existence isn't leaked via response latency. Wait for it explicitly.
	if !sender.waitCalled(2 * time.Second) {
		t.Fatal("sender.Send was not invoked within 2s")
	}
	to, code := sender.snapshot()
	if to != "alice@test.com" {
		t.Fatalf("sender.to = %q, want alice@test.com", to)
	}
	if code == "" {
		t.Fatal("sender.code should not be empty")
	}

	// Reset with the token.
	w = doJSON(r, "POST", "/reset-password", map[string]string{
		"token":        code,
		"new_password": "resetpass789",
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("reset: expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Login with new password.
	w = doJSON(r, "POST", "/login", map[string]string{
		"email":    "alice@test.com",
		"password": "resetpass789",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("login after reset: expected 200, got %d", w.Code)
	}
}

func TestForgotPassword_UnknownEmail_NoLeak(t *testing.T) {
	sender := newMockSender()
	_, r := setupModule(t, WithSender(sender))

	w := doJSON(r, "POST", "/forgot-password", map[string]string{
		"email": "nobody@test.com",
	})
	// Should still return 204 to prevent enumeration.
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	// Sender must not be invoked for an unknown email — give the
	// background dispatcher a brief window to prove it stays silent.
	if sender.waitCalled(200 * time.Millisecond) {
		t.Fatal("sender should not be called for unknown email")
	}
	if to, _ := sender.snapshot(); to != "" {
		t.Fatal("sender should not be called for unknown email")
	}
}

func TestResetPassword_InvalidToken(t *testing.T) {
	_, r := setupModule(t, WithSender(newMockSender()))

	w := doJSON(r, "POST", "/reset-password", map[string]string{
		"token":        "invalid.jwt.token",
		"new_password": "newpassword123",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Routes not registered without Sender ---

func TestForgotPassword_NotRegistered_WithoutSender(t *testing.T) {
	_, r := setupModule(t) // no WithSender

	w := doJSON(r, "POST", "/forgot-password", map[string]string{
		"email": "alice@test.com",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (route not registered), got %d", w.Code)
	}
}

// --- Setup tests ---

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gdb
}

func TestSetup_Enabled(t *testing.T) {
	gdb := openTestDB(t)
	r := gin.New()

	opts := &config.AccountOptions{
		Enabled:         true,
		SigningKey:      testSigningKey,
		Expiration:      2 * time.Hour,
		ResetExpiration: 15 * time.Minute,
	}

	m, err := Setup(gdb, log.Empty(), opts, r)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected non-nil module when enabled")
	}

	// Routes should work — register a user.
	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSetup_Disabled(t *testing.T) {
	gdb := openTestDB(t)
	r := gin.New()

	m, err := Setup(gdb, log.Empty(), &config.AccountOptions{Enabled: false}, r)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatal("expected nil module when disabled")
	}
}

func TestSetup_NilOpts(t *testing.T) {
	gdb := openTestDB(t)
	r := gin.New()

	m, err := Setup(gdb, log.Empty(), nil, r)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatal("expected nil module for nil opts")
	}
}

func TestSetup_WithSender(t *testing.T) {
	gdb := openTestDB(t)
	r := gin.New()
	sender := newMockSender()

	opts := &config.AccountOptions{
		Enabled:         true,
		SigningKey:      testSigningKey,
		Expiration:      2 * time.Hour,
		ResetExpiration: 15 * time.Minute,
	}

	m, err := Setup(gdb, log.Empty(), opts, r, WithSender(sender))
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected non-nil module")
	}

	// Register, then forgot-password should call the sender.
	doJSON(r, "POST", "/register", map[string]string{
		"email":    "bob@test.com",
		"password": "password123",
	})
	w := doJSON(r, "POST", "/forgot-password", map[string]string{
		"email": "bob@test.com",
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if !sender.waitCalled(2 * time.Second) {
		t.Fatal("sender.Send was not invoked within 2s")
	}
	to, _ := sender.snapshot()
	if to != "bob@test.com" {
		t.Fatalf("sender.to = %q, want bob@test.com", to)
	}
}

// --- AccountOptions.Validate tests ---

func TestAccountOptions_Validate_Disabled(t *testing.T) {
	opts := &config.AccountOptions{Enabled: false}
	if err := opts.Validate(); err != nil {
		t.Fatalf("disabled should pass validation: %v", err)
	}
}

func TestAccountOptions_Validate_ShortKey(t *testing.T) {
	opts := &config.AccountOptions{
		Enabled:         true,
		SigningKey:      "short",
		Expiration:      2 * time.Hour,
		ResetExpiration: 15 * time.Minute,
	}
	if err := opts.Validate(); err == nil {
		t.Fatal("expected error for short signing key")
	}
}

func TestAccountOptions_Validate_OK(t *testing.T) {
	opts := &config.AccountOptions{
		Enabled:         true,
		SigningKey:      testSigningKey,
		Expiration:      2 * time.Hour,
		ResetExpiration: 15 * time.Minute,
	}
	if err := opts.Validate(); err != nil {
		t.Fatalf("valid options should pass: %v", err)
	}
}

func TestSetup_ValidationError(t *testing.T) {
	gdb := openTestDB(t)
	r := gin.New()

	opts := &config.AccountOptions{
		Enabled:    true,
		SigningKey: "too-short",
	}
	_, err := Setup(gdb, log.Empty(), opts, r)
	if err == nil {
		t.Fatal("expected validation error for short signing key")
	}
}

func TestSetup_ZeroExpiration_UsesDefaults(t *testing.T) {
	gdb := openTestDB(t)
	r := gin.New()

	opts := &config.AccountOptions{
		Enabled:    true,
		SigningKey: testSigningKey,
		// Expiration and ResetExpiration intentionally zero
	}
	m, err := Setup(gdb, log.Empty(), opts, r)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected non-nil module")
	}

	// Register and verify token is issued (proves defaults were applied).
	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// --- ActiveCheck tests ---

func TestActiveCheck_DisabledUser_Blocked(t *testing.T) {
	m, r := setupModule(t)

	// Register.
	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	token := decodeToken(t, w)

	// Disable the user directly in the store.
	user, err := m.store.Get(context.Background(), store.Where(where.WithFilter("email", "alice@test.com")))
	if err != nil {
		t.Fatal(err)
	}
	user.Active = false
	if err := m.store.Update(context.Background(), store.RID(user.RID), store.Fields(user, "active")); err != nil {
		t.Fatal(err)
	}

	// Set up a protected route with ActiveCheck.
	protected := gin.New()
	protected.Use(middleware.Authn(m.TokenParser(), m.PrincipalResolver()))
	protected.Use(m.ActiveCheck())
	protected.GET("/me", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest("GET", "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()
	protected.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for disabled user, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestActiveCheck_ActiveUser_Allowed(t *testing.T) {
	m, r := setupModule(t)

	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	token := decodeToken(t, w)

	protected := gin.New()
	protected.Use(middleware.Authn(m.TokenParser(), m.PrincipalResolver()))
	protected.Use(m.ActiveCheck())
	protected.GET("/me", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest("GET", "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()
	protected.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 for active user, got %d: %s", resp.Code, resp.Body.String())
	}
}

// TestLimiter_CloseWaitsForCleanup verifies the M2 fix: Close blocks
// until any in-flight background cleanup goroutine has finished, so
// the App's shutdown budget covers it instead of the goroutine being
// killed mid-iteration when the process exits. After Close returns,
// further record() calls must not spawn new cleanups.
func TestLimiter_CloseWaitsForCleanup(t *testing.T) {
	l := newLoginLimiter(time.Hour, 5)

	// Force a cleanup launch by hitting the every-100-calls path.
	for i := 0; i < 100; i++ {
		l.record(limiterKey{Name: "email", Value: "victim@example.com"})
	}

	// Close must drain the goroutine before returning.
	done := make(chan struct{})
	go func() {
		l.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within 2s — goroutine probably leaked")
	}

	// After Close, additional record() must NOT launch a cleanup.
	for i := 0; i < 200; i++ {
		l.record(limiterKey{Name: "email", Value: "post-close@example.com"})
	}
	// wg.Wait() in a fresh Close call returns immediately if no
	// goroutine was launched — proves the closed flag short-circuits
	// the launch path.
	l.Close()
}
