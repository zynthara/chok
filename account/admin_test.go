package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/store"
	"github.com/zynthara/chok/v2/store/where"
)

// --- helper: create a registered user and return their RID + initial PV ---

func registerUser(t *testing.T, m *Module, r *gin.Engine, email, password string) (rid string, pv int) {
	t.Helper()
	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    email,
		"password": password,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("register %s: expected 201, got %d: %s", email, w.Code, w.Body.String())
	}
	user, err := m.userStore.Get(context.Background(), store.Where(where.WithFilter("email", email)))
	if err != nil {
		t.Fatalf("load registered user: %v", err)
	}
	return user.RID, user.PasswordVersion
}

// --- TestEmailVerifiedDefault ---

func TestEmailVerifiedDefault(t *testing.T) {
	m, r := setupModule(t)
	rid, _ := registerUser(t, m, r, "alice@test.com", "password123")
	user, err := m.userStore.Get(context.Background(), store.RID(rid))
	if err != nil {
		t.Fatal(err)
	}
	if user.EmailVerified {
		t.Fatal("expected EmailVerified=false on freshly registered user")
	}
}

// --- TestMarkEmailVerified ---

func TestMarkEmailVerified(t *testing.T) {
	m, r := setupModule(t)
	rid, pvBefore := registerUser(t, m, r, "alice@test.com", "password123")

	if err := m.MarkEmailVerified(context.Background(), rid); err != nil {
		t.Fatalf("MarkEmailVerified: %v", err)
	}
	user, err := m.userStore.Get(context.Background(), store.RID(rid))
	if err != nil {
		t.Fatal(err)
	}
	if !user.EmailVerified {
		t.Fatal("expected EmailVerified=true after MarkEmailVerified")
	}
	if user.PasswordVersion != pvBefore {
		t.Fatalf("MarkEmailVerified must NOT bump PV; got %d, want %d", user.PasswordVersion, pvBefore)
	}

	// Idempotent: second call no-op.
	if err := m.MarkEmailVerified(context.Background(), rid); err != nil {
		t.Fatalf("idempotent call: %v", err)
	}
}

func TestMarkEmailVerified_NotFound(t *testing.T) {
	m, _ := setupModule(t)
	err := m.MarkEmailVerified(context.Background(), "usr_nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown userID")
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != http.StatusNotFound {
		t.Fatalf("expected 404 apierr, got %T %v", err, err)
	}
}

// TestMarkEmailVerified_ConcurrentDoesNotSpurious404 exercises the
// re-read fallback that handles MySQL's default "rows changed" semantics.
// On SQLite the underlying UPDATE always reports RowsAffected=1 even when
// the value didn't change, so this test does not literally reproduce the
// production race — but it does pin the API contract: two goroutines
// flipping EmailVerified on the same user must both return nil, never
// 404. Any future implementation regression that drops the re-read
// fallback will, on MySQL, surface as one of the goroutines getting
// ErrNotFound here (under MaxOpenConns=1 the calls serialise but the
// no-op UPDATE path is still hit on the second call).
func TestMarkEmailVerified_ConcurrentDoesNotSpurious404(t *testing.T) {
	m, r := setupModule(t)
	sqlDB, err := m.userStore.DB().DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)

	rid, _ := registerUser(t, m, r, "alice@test.com", "password123")

	const N = 5
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.MarkEmailVerified(context.Background(), rid); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("MarkEmailVerified returned error under concurrent calls: %v", err)
	}
	user, _ := m.userStore.Get(context.Background(), store.RID(rid))
	if !user.EmailVerified {
		t.Fatal("expected EmailVerified=true after concurrent calls")
	}
}

// --- TestUpdateUserRoles_BumpsPV ---

func TestUpdateUserRoles_BumpsPV(t *testing.T) {
	m, r := setupModule(t)
	rid, pvBefore := registerUser(t, m, r, "alice@test.com", "password123")

	if err := m.UpdateUserRoles(context.Background(), rid, []string{"admin", "editor"}); err != nil {
		t.Fatalf("UpdateUserRoles: %v", err)
	}
	user, err := m.userStore.Get(context.Background(), store.RID(rid))
	if err != nil {
		t.Fatal(err)
	}
	if got := user.RoleList(); len(got) != 2 || got[0] != "admin" || got[1] != "editor" {
		t.Fatalf("roles = %v, want [admin editor]", got)
	}
	if user.PasswordVersion != pvBefore+1 {
		t.Fatalf("PV after UpdateUserRoles = %d, want %d", user.PasswordVersion, pvBefore+1)
	}
}

func TestUpdateUserRoles_ClearsRoles(t *testing.T) {
	m, r := setupModule(t)
	rid, _ := registerUser(t, m, r, "alice@test.com", "password123")
	if err := m.UpdateUserRoles(context.Background(), rid, []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateUserRoles(context.Background(), rid, nil); err != nil {
		t.Fatal(err)
	}
	user, err := m.userStore.Get(context.Background(), store.RID(rid))
	if err != nil {
		t.Fatal(err)
	}
	if got := user.RoleList(); len(got) != 0 {
		t.Fatalf("roles = %v, want empty", got)
	}
}

func TestUpdateUserRoles_InvalidatesOldToken(t *testing.T) {
	m, r := setupModule(t)
	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	token := decodeToken(t, w)

	user, err := m.userStore.Get(context.Background(), store.Where(where.WithFilter("email", "alice@test.com")))
	if err != nil {
		t.Fatal(err)
	}
	// Bump PV via UpdateUserRoles.
	if err := m.UpdateUserRoles(context.Background(), user.RID, []string{"admin"}); err != nil {
		t.Fatal(err)
	}

	protected := gin.New()
	protected.Use(m.AuthChain()...)
	protected.GET("/me", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	req := httptest.NewRequest("GET", "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()
	protected.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after PV bump, got %d: %s", resp.Code, resp.Body.String())
	}
}

// --- TestSetUserActive_BumpsPV ---

func TestSetUserActive_BumpsPV(t *testing.T) {
	m, r := setupModule(t)
	rid, pvBefore := registerUser(t, m, r, "alice@test.com", "password123")

	if err := m.SetUserActive(context.Background(), rid, false); err != nil {
		t.Fatalf("SetUserActive: %v", err)
	}
	user, err := m.userStore.Get(context.Background(), store.RID(rid))
	if err != nil {
		t.Fatal(err)
	}
	if user.Active {
		t.Fatal("expected Active=false")
	}
	if user.PasswordVersion != pvBefore+1 {
		t.Fatalf("PV = %d, want %d", user.PasswordVersion, pvBefore+1)
	}

	// Re-enable also bumps PV.
	if err := m.SetUserActive(context.Background(), rid, true); err != nil {
		t.Fatal(err)
	}
	user, _ = m.userStore.Get(context.Background(), store.RID(rid))
	if !user.Active {
		t.Fatal("expected Active=true after re-enable")
	}
	if user.PasswordVersion != pvBefore+2 {
		t.Fatalf("PV after re-enable = %d, want %d", user.PasswordVersion, pvBefore+2)
	}
}

// --- TestBumpPasswordVersion ---

func TestBumpPasswordVersion(t *testing.T) {
	m, r := setupModule(t)
	rid, pvBefore := registerUser(t, m, r, "alice@test.com", "password123")

	if err := m.BumpPasswordVersion(context.Background(), rid); err != nil {
		t.Fatalf("BumpPasswordVersion: %v", err)
	}
	user, err := m.userStore.Get(context.Background(), store.RID(rid))
	if err != nil {
		t.Fatal(err)
	}
	if user.PasswordVersion != pvBefore+1 {
		t.Fatalf("PV = %d, want %d", user.PasswordVersion, pvBefore+1)
	}
}

func TestBumpPasswordVersion_EmptyUserID(t *testing.T) {
	m, _ := setupModule(t)
	err := m.BumpPasswordVersion(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty userID")
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 apierr, got %T %v", err, err)
	}
}

// --- TestStore_RejectsForbiddenFields ---
//
// publicStore (returned by Module.Store()) MUST reject writes to
// password_hash / password_version / roles / active / email_verified —
// the framework-level enforcement of the PV-bump contract. Each forbidden
// field should produce store.ErrUnknownUpdateField, not silently update.
func TestStore_RejectsForbiddenFields(t *testing.T) {
	m, r := setupModule(t)
	rid, _ := registerUser(t, m, r, "alice@test.com", "password123")

	cases := []struct {
		field string
		value any
	}{
		{"password_hash", "$2a$bogus"},
		{"password_version", 99},
		{"roles", "admin"},
		{"active", false},
		{"email_verified", true},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			err := m.Store().Update(context.Background(), store.RID(rid),
				store.Set(map[string]any{tc.field: tc.value}))
			if !errors.Is(err, store.ErrUnknownUpdateField) {
				t.Fatalf("write %q: expected ErrUnknownUpdateField, got %v", tc.field, err)
			}
		})
	}
}

func TestStore_AllowsNameAndEmailWrites(t *testing.T) {
	m, r := setupModule(t)
	rid, _ := registerUser(t, m, r, "alice@test.com", "password123")

	err := m.Store().Update(context.Background(), store.RID(rid),
		store.Set(map[string]any{"name": "Alicia", "email": "alicia@test.com"}))
	if err != nil {
		t.Fatalf("write name+email through Store(): %v", err)
	}
	user, _ := m.userStore.Get(context.Background(), store.RID(rid))
	if user.Name != "Alicia" || user.Email != "alicia@test.com" {
		t.Fatalf("publicStore writes did not land: name=%q email=%q", user.Name, user.Email)
	}
}

// --- TestAuthChain ---

func TestAuthChain_RejectsDisabledUser(t *testing.T) {
	m, r := setupModule(t)

	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	token := decodeToken(t, w)

	user, _ := m.userStore.Get(context.Background(), store.Where(where.WithFilter("email", "alice@test.com")))
	if err := m.SetUserActive(context.Background(), user.RID, false); err != nil {
		t.Fatal(err)
	}

	protected := gin.New()
	protected.Use(m.AuthChain()...)
	protected.GET("/me", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest("GET", "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()
	protected.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for disabled user via AuthChain, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestAuthChain_AllowsActiveUser(t *testing.T) {
	m, r := setupModule(t)
	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	token := decodeToken(t, w)

	protected := gin.New()
	protected.Use(m.AuthChain()...)
	protected.GET("/me", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest("GET", "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()
	protected.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 for active user via AuthChain, got %d: %s", resp.Code, resp.Body.String())
	}
}

// --- Role-input validation (Medium #3 regression) ---

func TestUpdateUserRoles_RejectsCommaInRole(t *testing.T) {
	m, r := setupModule(t)
	rid, _ := registerUser(t, m, r, "alice@test.com", "password123")

	// "admin,evil" would round-trip through SetRoles/RoleList as
	// ["admin","evil"] — silent privilege escalation.
	err := m.UpdateUserRoles(context.Background(), rid, []string{"admin,evil"})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 apierr for comma in role, got %T %v", err, err)
	}
	user, _ := m.userStore.Get(context.Background(), store.RID(rid))
	if user.Roles != "" {
		t.Fatalf("rejected call should not have written roles, got %q", user.Roles)
	}
}

func TestUpdateUserRoles_RejectsEmptyRole(t *testing.T) {
	m, r := setupModule(t)
	rid, _ := registerUser(t, m, r, "alice@test.com", "password123")
	err := m.UpdateUserRoles(context.Background(), rid, []string{"admin", ""})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 apierr for empty role, got %T %v", err, err)
	}
}

func TestUpdateUserRoles_RejectsTooLongRole(t *testing.T) {
	m, r := setupModule(t)
	rid, _ := registerUser(t, m, r, "alice@test.com", "password123")
	long := strings.Repeat("a", roleMaxLen+1)
	err := m.UpdateUserRoles(context.Background(), rid, []string{long})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 apierr for over-length role, got %T %v", err, err)
	}
}

func TestUpdateUserRoles_RejectsCSVOverflow(t *testing.T) {
	m, r := setupModule(t)
	rid, _ := registerUser(t, m, r, "alice@test.com", "password123")
	// 20 × ("r" * 30 + ",") = 20 × 31 = 620 chars > 500.
	role := strings.Repeat("r", 30)
	roles := make([]string, 20)
	for i := range roles {
		roles[i] = role
	}
	err := m.UpdateUserRoles(context.Background(), rid, roles)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 apierr for CSV overflow, got %T %v", err, err)
	}
}

func TestUpdateUserRoles_AcceptsClear(t *testing.T) {
	m, r := setupModule(t)
	rid, _ := registerUser(t, m, r, "alice@test.com", "password123")
	if err := m.UpdateUserRoles(context.Background(), rid, []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	// Empty slice clears (validateRoles returns nil for nil/empty input).
	if err := m.UpdateUserRoles(context.Background(), rid, nil); err != nil {
		t.Fatalf("clearing roles should be allowed, got %v", err)
	}
}

// --- Atomic PV bump (High #1 regression) ---
//
// The fix replaces "Get → user.PasswordVersion++ → Update.NoLock()" with
// `UPDATE ... SET password_version = password_version + 1`, computed by
// the DB engine. The sequential test pins the per-call increment; the
// concurrent test exercises the same path under N goroutines.
//
// chok's test infra uses sqlite ":memory:" with a single connection,
// which serialises writes — so the concurrent test cannot directly
// observe a true race even on the buggy implementation. It still serves
// as a regression target: any future implementation that introduces a
// load-then-write window MUST keep the final PV equal to N regardless
// of execution order. Real concurrency validation belongs in a
// MySQL/Postgres integration test.

func TestBumpPasswordVersion_Sequential(t *testing.T) {
	m, r := setupModule(t)
	rid, pvBefore := registerUser(t, m, r, "alice@test.com", "password123")

	const N = 10
	for i := 0; i < N; i++ {
		if err := m.BumpPasswordVersion(context.Background(), rid); err != nil {
			t.Fatalf("BumpPasswordVersion #%d: %v", i, err)
		}
	}
	user, err := m.userStore.Get(context.Background(), store.RID(rid))
	if err != nil {
		t.Fatal(err)
	}
	if user.PasswordVersion != pvBefore+N {
		t.Fatalf("after %d bumps PV=%d, want %d", N, user.PasswordVersion, pvBefore+N)
	}
}

func TestBumpPasswordVersion_Concurrent(t *testing.T) {
	m, r := setupModule(t)
	// gorm's default pool would give each goroutine a fresh ":memory:"
	// SQLite (a separate database) — pin to one connection so all
	// goroutines share the migrated schema. With MaxOpenConns=1 the
	// goroutines serialise at the connection layer, so this test does
	// NOT exercise true parallelism; it is a smoke test that the API
	// path stays correct under repeated invocation. Real concurrency
	// validation belongs in a MySQL/Postgres integration test.
	sqlDB, err := m.userStore.DB().DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)

	rid, pvBefore := registerUser(t, m, r, "alice@test.com", "password123")

	const N = 10
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.BumpPasswordVersion(context.Background(), rid); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent BumpPasswordVersion: %v", err)
	}
	user, err := m.userStore.Get(context.Background(), store.RID(rid))
	if err != nil {
		t.Fatal(err)
	}
	if user.PasswordVersion != pvBefore+N {
		t.Fatalf("after %d concurrent bumps PV=%d, want %d (atomic UPDATE must not lose increments)", N, user.PasswordVersion, pvBefore+N)
	}
}

// --- Admin APIs on non-existent user must report 404 ---
//
// The atomic-UPDATE path relies on RowsAffected==0 → ErrNotFound mapping
// in finalizeUpdate. Without an explicit Get, an unknown userID would
// otherwise return nil silently.

func TestBumpPasswordVersion_NonExistent(t *testing.T) {
	m, _ := setupModule(t)
	err := m.BumpPasswordVersion(context.Background(), "usr_nonexistent")
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != http.StatusNotFound {
		t.Fatalf("expected 404 apierr, got %T %v", err, err)
	}
}

func TestSetUserActive_NonExistent(t *testing.T) {
	m, _ := setupModule(t)
	err := m.SetUserActive(context.Background(), "usr_nonexistent", false)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != http.StatusNotFound {
		t.Fatalf("expected 404 apierr, got %T %v", err, err)
	}
}

func TestUpdateUserRoles_NonExistent(t *testing.T) {
	m, _ := setupModule(t)
	err := m.UpdateUserRoles(context.Background(), "usr_nonexistent", []string{"admin"})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != http.StatusNotFound {
		t.Fatalf("expected 404 apierr, got %T %v", err, err)
	}
}

// --- /change-password under stale PV (High #2 regression) ---
//
// After admin BumpPasswordVersion, the user's existing token has stale
// pv. With AuthChain on the authed group, ActiveCheck rejects the
// stale token before changePassword can run; the PV-bump invariant
// holds for /change-password too, not just for business routes.

func TestChangePassword_RejectsStalePV(t *testing.T) {
	m, r := setupModule(t)
	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	token := decodeToken(t, w)

	user, err := m.userStore.Get(context.Background(), store.Where(where.WithFilter("email", "alice@test.com")))
	if err != nil {
		t.Fatal(err)
	}
	// Admin force-revokes outstanding tokens.
	if err := m.BumpPasswordVersion(context.Background(), user.RID); err != nil {
		t.Fatal(err)
	}

	// Holding the now-stale token + knowing the password must NOT permit
	// password change — the old token has been revoked.
	w = doJSON(r, "PUT", "/change-password", map[string]string{
		"old_password": "password123",
		"new_password": "newpassword456",
	}, token)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for stale-PV token, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRefreshToken_RejectsStalePV(t *testing.T) {
	m, r := setupModule(t)
	w := doJSON(r, "POST", "/register", map[string]string{
		"email":    "alice@test.com",
		"password": "password123",
	})
	token := decodeToken(t, w)

	user, err := m.userStore.Get(context.Background(), store.Where(where.WithFilter("email", "alice@test.com")))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.BumpPasswordVersion(context.Background(), user.RID); err != nil {
		t.Fatal(err)
	}

	w = doJSON(r, "POST", "/refresh-token", nil, token)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for stale-PV token, got %d: %s", w.Code, w.Body.String())
	}
}
