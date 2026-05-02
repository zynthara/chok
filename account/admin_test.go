package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
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
