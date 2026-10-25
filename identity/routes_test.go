package identity

import (
	"net/http"
	"slices"
	"testing"
	"time"
)

func TestRegister_IssuesAToken(t *testing.T) {
	idn := newIdentity(t, Config{})
	c := serve(t, idn)
	var tok tokenBody
	code := c.PostJSON("/auth/register", map[string]string{"email": "alice@test.com", "password": "password123", "name": "Alice"}, &tok)
	if code != http.StatusCreated || tok.Token == "" || time.Until(tok.ExpiresAt) < time.Hour {
		t.Fatalf("register = %d %+v", code, tok)
	}
	u := userByEmail(t, idn, "alice@test.com")
	if !u.Active || !u.HasPassword || u.Name != "Alice" || u.EmailVerified || len(u.RID) < 5 || u.RID[:4] != "usr_" {
		t.Fatalf("the account: %+v", u)
	}
	if comparePassword(u.PasswordHash, "password123") != nil {
		t.Fatal("the password is stored as its bcrypt hash")
	}
}

func TestRegister_EmptyNameMasksTheEmail(t *testing.T) {
	idn := newIdentity(t, Config{})
	register(t, serve(t, idn), "alice@test.com", "password123")
	if u := userByEmail(t, idn, "alice@test.com"); u.Name != "a***e@t**t.com" {
		t.Fatalf("name = %q", u.Name)
	}
}

func TestRegister_DuplicateEmailConflicts(t *testing.T) {
	c := serve(t, newIdentity(t, Config{}))
	register(t, c, "Alice@Test.COM", "password123")
	for _, email := range []string{"alice@test.com", "ALICE@test.com", "alice@TEST.com"} {
		var e envelope
		code := c.PostJSON("/auth/register", map[string]string{"email": email, "password": "password123"}, &e)
		if code != http.StatusConflict || e.Reason != "Conflict" || e.Metadata["field"] != "email" || e.Message != "email already registered" {
			t.Fatalf("register %q again = %d %+v", email, code, e)
		}
	}
}

func TestRegister_Validation(t *testing.T) {
	c := serve(t, newIdentity(t, Config{}))
	cases := []struct {
		body  map[string]string
		field string
	}{
		{map[string]string{"email": "not-an-email", "password": "password123"}, "email"},
		{map[string]string{"email": "a@test.com", "password": "short"}, "password"},
		{map[string]string{"email": "a@test.com"}, "password"},
		{map[string]string{"email": "a@test.com", "password": "alllowercase"}, "password"}, // one character class
		{map[string]string{"email": "a@test.com", "password": "12345678"}, "password"},
	}
	for _, tc := range cases {
		var e envelope
		code := c.PostJSON("/auth/register", tc.body, &e)
		if code != http.StatusBadRequest || e.Reason != "Validation" || !hasKey(e.Metadata["fields"], tc.field) {
			t.Errorf("%v = %d %+v", tc.body, code, e)
		}
	}
}

func TestRegister_DisabledByConfig(t *testing.T) {
	idn := newIdentity(t, Config{DisableRegister: true})
	c := serve(t, idn)
	if code := c.PostJSON("/auth/register", map[string]string{"email": "a@test.com", "password": "password123"}, nil); code != http.StatusNotFound {
		t.Fatalf("register with disable_register = %d", code)
	}
	// Accounts come from the administration instead, and log in.
	if _, err := idn.Users().Create(t.Context(), "a@test.com", "password123", "A"); err != nil {
		t.Fatal(err)
	}
	if code, tok := login(t, c, "a@test.com", "password123"); code != http.StatusOK || tok == "" {
		t.Fatalf("login = %d", code)
	}
}

func TestLogin_IssuesAToken(t *testing.T) {
	c := serve(t, newIdentity(t, Config{}))
	register(t, c, "alice@test.com", "password123")
	code, tok := login(t, c, "alice@test.com", "password123")
	if code != http.StatusOK || tok == "" {
		t.Fatalf("login = %d", code)
	}
	var me meResponse
	if code := c.WithBearer(tok).GetJSON("/auth/me", &me); code != http.StatusOK || me.Email != "alice@test.com" {
		t.Fatalf("the token works: %d %+v", code, me)
	}
}

func TestLogin_EmailIsCaseInsensitive(t *testing.T) {
	c := serve(t, newIdentity(t, Config{}))
	register(t, c, "Alice@Test.COM", "password123")
	if code, _ := login(t, c, "alice@test.com", "password123"); code != http.StatusOK {
		t.Fatalf("login in another case = %d", code)
	}
	if code, _ := login(t, c, "  ALICE@test.com ", "password123"); code != http.StatusBadRequest {
		// The request validates the address before trimming: surrounding
		// spaces are not an email.
		t.Fatalf("login with spaces = %d", code)
	}
}

func TestLogin_RefusesBadCredentials(t *testing.T) {
	c := serve(t, newIdentity(t, Config{}))
	register(t, c, "alice@test.com", "password123")
	for _, creds := range [][2]string{{"alice@test.com", "wrongpassword"}, {"nobody@test.com", "password123"}} {
		var e envelope
		code := c.PostJSON("/auth/login", map[string]string{"email": creds[0], "password": creds[1]}, &e)
		// An unknown email and a wrong password read the same.
		if code != http.StatusUnauthorized || e.Message != "invalid email or password" || e.RequestID == "" {
			t.Fatalf("%v = %d %+v", creds, code, e)
		}
	}
}

func TestLogin_RefusesADisabledAccount(t *testing.T) {
	idn := newIdentity(t, Config{})
	c := serve(t, idn)
	register(t, c, "alice@test.com", "password123")
	if err := idn.Users().SetActive(t.Context(), userByEmail(t, idn, "alice@test.com").RID, false); err != nil {
		t.Fatal(err)
	}
	var e envelope
	if code := c.PostJSON("/auth/login", map[string]string{"email": "alice@test.com", "password": "password123"}, &e); code != http.StatusUnauthorized || e.Message != "account is disabled" {
		t.Fatalf("login when disabled = %d %+v", code, e)
	}
}

func TestRefreshToken_IssuesAFreshToken(t *testing.T) {
	c := serve(t, newIdentity(t, Config{}))
	tok := register(t, c, "alice@test.com", "password123")
	var fresh tokenBody
	if code := c.WithBearer(tok).PostJSON("/auth/refresh-token", nil, &fresh); code != http.StatusOK || fresh.Token == "" {
		t.Fatalf("refresh = %d %+v", code, fresh)
	}
	if code := c.WithBearer(fresh.Token).GetJSON("/auth/me", nil); code != http.StatusOK {
		t.Fatalf("the fresh token works: %d", code)
	}
}

func TestRefreshToken_NeedsAToken(t *testing.T) {
	c := serve(t, newIdentity(t, Config{}))
	var e envelope
	if code := c.PostJSON("/auth/refresh-token", nil, &e); code != http.StatusUnauthorized || e.Reason != "Unauthorized" {
		t.Fatalf("refresh without a token = %d %+v", code, e)
	}
}

func TestRefreshToken_SessionEndsAfterRefreshTTL(t *testing.T) {
	idn := newIdentity(t, Config{RefreshTTL: time.Hour})
	c := serve(t, idn)
	register(t, c, "alice@test.com", "password123")
	u := userByEmail(t, idn, "alice@test.com")
	// A session that began two hours ago, in a token still valid.
	old, err := idn.issueToken(u, time.Now().Add(-2*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if code := c.WithBearer(old.Token).PostJSON("/auth/refresh-token", nil, &e); code != http.StatusUnauthorized || e.Message != "the session ended, please log in again" {
		t.Fatalf("refresh past refresh_ttl = %d %+v", code, e)
	}
	// The token itself still works until it expires.
	if code := c.WithBearer(old.Token).GetJSON("/auth/me", nil); code != http.StatusOK {
		t.Fatalf("me = %d", code)
	}
	// A refresh keeps the session's start: the chain ends at the same time.
	fresh, err := idn.issueToken(u, time.Now().Add(-30*time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	var next tokenBody
	if code := c.WithBearer(fresh.Token).PostJSON("/auth/refresh-token", nil, &next); code != http.StatusOK {
		t.Fatalf("refresh within refresh_ttl = %d", code)
	}
	_, claims, err := idn.access.parse(next.Token)
	if err != nil {
		t.Fatal(err)
	}
	if sat, _ := intClaim(claims, "sat"); time.Since(time.Unix(sat, 0)) < 29*time.Minute {
		t.Fatalf("the refreshed token restarted the session: sat=%v", time.Unix(sat, 0))
	}
}

func TestChangePassword_RotatesThePassword(t *testing.T) {
	c := serve(t, newIdentity(t, Config{}))
	tok := register(t, c, "alice@test.com", "password123")
	code := c.WithBearer(tok).JSON(http.MethodPut, "/auth/change-password", map[string]string{"old_password": "password123", "new_password": "newpassword456"}, nil)
	if code != http.StatusNoContent {
		t.Fatalf("change-password = %d", code)
	}
	if code, _ := login(t, c, "alice@test.com", "newpassword456"); code != http.StatusOK {
		t.Fatalf("login with the new password = %d", code)
	}
	if code, _ := login(t, c, "alice@test.com", "password123"); code != http.StatusUnauthorized {
		t.Fatalf("login with the old password = %d", code)
	}
	// The change revoked the tokens issued before it.
	if code := c.WithBearer(tok).GetJSON("/auth/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("the old token after the change = %d", code)
	}
}

func TestChangePassword_RefusesAWrongOldPassword(t *testing.T) {
	c := serve(t, newIdentity(t, Config{}))
	tok := register(t, c, "alice@test.com", "password123")
	var e envelope
	code := c.WithBearer(tok).JSON(http.MethodPut, "/auth/change-password", map[string]string{"old_password": "wrongpassword", "new_password": "newpassword456"}, &e)
	if code != http.StatusUnauthorized || e.Message != "old password is incorrect" {
		t.Fatalf("change-password = %d %+v", code, e)
	}
	// A weak new password is refused after the old one checks out.
	code = c.WithBearer(tok).JSON(http.MethodPut, "/auth/change-password", map[string]string{"old_password": "password123", "new_password": "aaaaaaaaaa"}, &e)
	if code != http.StatusBadRequest || !hasKey(e.Metadata["fields"], "new_password") {
		t.Fatalf("a weak new password = %d %+v", code, e)
	}
}

func TestForgotAndResetPassword(t *testing.T) {
	sender := newMockSender()
	c := serve(t, newIdentity(t, Config{}, Sender(sender)))
	register(t, c, "alice@test.com", "password123")
	if code := c.PostJSON("/auth/forgot-password", map[string]string{"email": "Alice@test.com"}, nil); code != http.StatusNoContent {
		t.Fatalf("forgot = %d", code)
	}
	if !sender.wait(2 * time.Second) {
		t.Fatal("the sender was not called within 2s")
	}
	m := sender.last()
	token := m.Token
	if m.Kind != "password-reset" || m.To != "alice@test.com" || token == "" || m.Code != "" || m.Link != "" || time.Until(m.ExpiresAt) < 14*time.Minute || m.ExpiresAt.Location() != time.UTC {
		t.Fatalf("mailed %+v", m)
	}
	if code := c.PostJSON("/auth/reset-password", map[string]string{"token": token, "new_password": "resetpass789"}, nil); code != http.StatusNoContent {
		t.Fatalf("reset = %d", code)
	}
	if code, _ := login(t, c, "alice@test.com", "resetpass789"); code != http.StatusOK {
		t.Fatalf("login after the reset = %d", code)
	}
	// The token worked once: the reset moved the version it was bound to.
	var e envelope
	if code := c.PostJSON("/auth/reset-password", map[string]string{"token": token, "new_password": "another789"}, &e); code != http.StatusBadRequest || e.Message != "invalid or expired reset token" {
		t.Fatalf("the reset token replayed = %d %+v", code, e)
	}
}

func TestForgotPassword_UnknownEmailDoesNotLeak(t *testing.T) {
	sender := newMockSender()
	c := serve(t, newIdentity(t, Config{}, Sender(sender)))
	if code := c.PostJSON("/auth/forgot-password", map[string]string{"email": "nobody@test.com"}, nil); code != http.StatusNoContent {
		t.Fatalf("forgot for an unknown email = %d", code)
	}
	if sender.wait(200 * time.Millisecond) {
		t.Fatal("nothing is mailed for an unknown email")
	}
}

func TestResetPassword_RefusesBadTokens(t *testing.T) {
	idn := newIdentity(t, Config{}, Sender(newMockSender()))
	c := serve(t, idn)
	register(t, c, "alice@test.com", "password123")
	u := userByEmail(t, idn, "alice@test.com")
	access, err := idn.issueToken(u, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	wrongPurpose, _, _ := idn.reset.sign(u.RID, map[string]any{"purpose": "other", "pv": u.PasswordVersion})
	for name, token := range map[string]string{"garbage": "invalid.jwt.token", "an access token": access.Token, "another purpose": wrongPurpose} {
		var e envelope
		if code := c.PostJSON("/auth/reset-password", map[string]string{"token": token, "new_password": "newpassword123"}, &e); code != http.StatusBadRequest || e.Message != "invalid or expired reset token" {
			t.Errorf("%s = %d %+v", name, code, e)
		}
	}
	// A disabled account's token reads as invalid too.
	valid, _, _ := idn.reset.sign(u.RID, map[string]any{"purpose": "reset", "pv": u.PasswordVersion + 1})
	if err := idn.Users().SetActive(t.Context(), u.RID, false); err != nil {
		t.Fatal(err)
	}
	if code := c.PostJSON("/auth/reset-password", map[string]string{"token": valid, "new_password": "newpassword123"}, nil); code != http.StatusBadRequest {
		t.Fatalf("a disabled account's reset = %d", code)
	}
}

func TestForgotPassword_NeedsASender(t *testing.T) {
	c := serve(t, newIdentity(t, Config{}))
	for _, path := range []string{"/auth/forgot-password", "/auth/reset-password"} {
		if code := c.PostJSON(path, map[string]string{"email": "alice@test.com"}, nil); code != http.StatusNotFound {
			t.Fatalf("%s without a sender = %d", path, code)
		}
	}
}

func TestMe_ReturnsTheSignedInUser(t *testing.T) {
	idn := newIdentity(t, Config{})
	c := serve(t, idn)
	register(t, c, "alice@test.com", "password123")
	u := userByEmail(t, idn, "alice@test.com")
	if err := idn.Users().SetRoles(t.Context(), u.RID, []string{"editor", "admin"}); err != nil {
		t.Fatal(err)
	}
	_, tok := login(t, c, "alice@test.com", "password123")
	var me meResponse
	if code := c.WithBearer(tok).GetJSON("/auth/me", &me); code != http.StatusOK {
		t.Fatalf("me = %d", code)
	}
	if me.ID != u.RID || me.Email != "alice@test.com" || me.Name == "" || !slices.Equal(me.Roles, []string{"editor", "admin"}) || me.CreatedAt.IsZero() {
		t.Fatalf("me = %+v", me)
	}
	var raw map[string]any
	c.WithBearer(tok).GetJSON("/auth/me", &raw)
	for _, secret := range []string{"password_hash", "password_version", "has_password", "active"} {
		if _, leaked := raw[secret]; leaked {
			t.Errorf("me must not show %s", secret)
		}
	}
}

func TestPrefix_MovesTheRoutes(t *testing.T) {
	c := serve(t, newIdentity(t, Config{Prefix: "/api/account"}))
	var tok tokenBody
	if code := c.PostJSON("/api/account/register", map[string]string{"email": "a@test.com", "password": "password123"}, &tok); code != http.StatusCreated {
		t.Fatalf("register under the prefix = %d", code)
	}
	if code := c.PostJSON("/auth/login", map[string]string{"email": "a@test.com", "password": "password123"}, nil); code != http.StatusNotFound {
		t.Fatalf("the default prefix is not mounted: %d", code)
	}
	if code := c.WithBearer(tok.Token).GetJSON("/api/account/me", nil); code != http.StatusOK {
		t.Fatalf("me under the prefix = %d", code)
	}
}
