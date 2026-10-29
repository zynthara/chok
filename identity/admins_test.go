package identity

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zynthara/chok/v3/internal/dbtest"
)

// adminsConfig lists root@test.com among the administrators, in another
// case, with the audit log on to count the grants.
func adminsConfig() Config {
	return Config{Admins: []string{"Root@Test.com"}, Audit: AuditConfig{Enabled: true}}
}

// verifiedUser creates email's account, its address verified.
func verifiedUser(t *testing.T, idn *Identity, email string) *User {
	t.Helper()
	u, err := idn.Users().Create(t.Context(), email, "password123", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := idn.Users().MarkEmailVerified(t.Context(), u.RID); err != nil {
		t.Fatal(err)
	}
	return userByEmail(t, idn, email)
}

// tokenRoles is the roles claim of an access token.
func tokenRoles(t *testing.T, idn *Identity, token string) []string {
	t.Helper()
	_, claims, err := idn.access.parse(token)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := claims["roles"].([]any)
	roles := make([]string, 0, len(raw))
	for _, r := range raw {
		s, ok := r.(string)
		if !ok {
			t.Fatalf("a role claim of %T", r)
		}
		roles = append(roles, s)
	}
	return roles
}

// grantRecords closes idn, which writes the queued audit events, and
// counts the grants recorded for userID.
func grantRecords(t *testing.T, idn *Identity, userID string) int64 {
	t.Helper()
	if err := idn.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	return count(t, idn, "action = ? AND resource = ? AND resource_id = ? AND actor_type = ?", "user.role.grant", "user", userID, ActorSystem)
}

var adminOnly = []string{"admin"}

func TestAdmins_LoginGrantsAVerifiedAddress(t *testing.T) {
	idn := newIdentity(t, adminsConfig())
	c := serve(t, idn)
	before := verifiedUser(t, idn, "root@test.com")
	code, tok := login(t, c, "root@test.com", "password123")
	if code != http.StatusOK {
		t.Fatalf("login = %d", code)
	}
	if got := tokenRoles(t, idn, tok); !slices.Equal(got, adminOnly) {
		t.Fatalf("the token's roles = %v", got)
	}
	// One write: the roles, and the row's version with them; the tokens
	// stay valid.
	after := userByEmail(t, idn, "root@test.com")
	if after.Roles != "admin" || after.Version != before.Version+1 || after.PasswordVersion != before.PasswordVersion {
		t.Fatalf("the account: roles %q, version %d → %d, password_version %d → %d", after.Roles, before.Version, after.Version, before.PasswordVersion, after.PasswordVersion)
	}
	// Signing in again finds the role there and writes nothing.
	if code, tok := login(t, c, "root@test.com", "password123"); code != http.StatusOK || !slices.Equal(tokenRoles(t, idn, tok), adminOnly) {
		t.Fatalf("second login = %d", code)
	}
	if again := userByEmail(t, idn, "root@test.com"); again.Version != after.Version || again.Roles != "admin" {
		t.Fatalf("a second sign-in wrote: version %d, roles %q", again.Version, again.Roles)
	}
	// Somebody else gets nothing.
	verifiedUser(t, idn, "alice@test.com")
	if code, tok := login(t, c, "alice@test.com", "password123"); code != http.StatusOK || len(tokenRoles(t, idn, tok)) != 0 {
		t.Fatalf("an unlisted user's login = %d", code)
	}
	if n := grantRecords(t, idn, before.RID); n != 1 {
		t.Fatalf("grant records = %d, want 1", n)
	}
}

func TestAdmins_UnverifiedGetsNothingUntilVerified(t *testing.T) {
	idn := newIdentity(t, adminsConfig())
	c := serve(t, idn)
	// Registration is open: anyone may take the listed address first,
	// and gains nothing by it.
	tok := register(t, c, "root@test.com", "password123")
	if got := tokenRoles(t, idn, tok); len(got) != 0 {
		t.Fatalf("register's token roles = %v", got)
	}
	if code, again := login(t, c, "root@test.com", "password123"); code != http.StatusOK || len(tokenRoles(t, idn, again)) != 0 {
		t.Fatalf("login = %d", code)
	}
	var me meResponse
	if code := c.WithBearer(tok).GetJSON("/auth/me", &me); code != http.StatusOK || len(me.Roles) != 0 {
		t.Fatalf("me = %d %+v", code, me)
	}
	u := userByEmail(t, idn, "root@test.com")
	if u.Roles != "" {
		t.Fatalf("an unverified address got roles %q", u.Roles)
	}
	// Once the address is proven, Authn grants the role on the next
	// request, and the token from before still works.
	if err := idn.Users().MarkEmailVerified(t.Context(), u.RID); err != nil {
		t.Fatal(err)
	}
	if code := c.WithBearer(tok).GetJSON("/auth/me", &me); code != http.StatusOK || !slices.Equal(me.Roles, adminOnly) {
		t.Fatalf("me after the verification = %d %+v", code, me)
	}
	if code := c.WithBearer(tok).GetJSON("/auth/me", nil); code != http.StatusOK {
		t.Fatalf("the grant revoked the token: %d", code)
	}
	if got := userByEmail(t, idn, "root@test.com"); got.Roles != "admin" || got.PasswordVersion != u.PasswordVersion {
		t.Fatalf("the account: roles %q, password_version %d → %d", got.Roles, u.PasswordVersion, got.PasswordVersion)
	}
	if n := grantRecords(t, idn, u.RID); n != 1 {
		t.Fatalf("grant records = %d, want 1", n)
	}
}

func TestAdmins_PasswordlessVerifyGrants(t *testing.T) {
	idn, sender := pwIdentity(t, adminsConfig())
	c := serve(t, idn)
	// An account registered with a password, its address unproven: the
	// mailed code proves it, and the token verify issues is an admin's.
	register(t, c, "root@test.com", "password123")
	m := startCode(t, c, sender, "root@test.com")
	code, tok, e := verify(c, map[string]string{"email": "root@test.com", "code": m.Code})
	if code != http.StatusOK {
		t.Fatalf("verify = %d %+v", code, e)
	}
	if got := tokenRoles(t, idn, tok.Token); !slices.Equal(got, adminOnly) {
		t.Fatalf("verify's token roles = %v", got)
	}
	if u := userByEmail(t, idn, "root@test.com"); !u.EmailVerified || u.Roles != "admin" {
		t.Fatalf("the account: %+v", u)
	}
}

func TestAdmins_OAuthExchangeGrants(t *testing.T) {
	fx := newOAuthFixture(t, Config{Admins: []string{"alice@idp.test"}}, nil)
	fx.p.seed("code-1", verified("acct-1", "alice@idp.test"))
	cb := fx.signIn(t, "code-1", "")
	if cb.StatusCode != http.StatusFound {
		t.Fatalf("callback = %d", cb.StatusCode)
	}
	resp := fx.sendJSON(t, "/auth/exchange", map[string]string{"code": query(t, cb.Header.Get("Location"), "code")}, cb.Cookies())
	var out exchangeResponse
	decodeBody(t, resp, &out)
	if resp.StatusCode != http.StatusOK || out.Token == "" {
		t.Fatalf("exchange = %d %+v", resp.StatusCode, out)
	}
	if got := tokenRoles(t, fx.idn, out.Token); !slices.Equal(got, adminOnly) {
		t.Fatalf("the exchanged token's roles = %v", got)
	}
}

func TestAdmins_RefreshTokenGrants(t *testing.T) {
	idn := newIdentity(t, adminsConfig())
	c := serve(t, idn)
	tok := register(t, c, "root@test.com", "password123")
	u := userByEmail(t, idn, "root@test.com")
	if err := idn.Users().MarkEmailVerified(t.Context(), u.RID); err != nil {
		t.Fatal(err)
	}
	var fresh tokenBody
	if code := c.WithBearer(tok).PostJSON("/auth/refresh-token", nil, &fresh); code != http.StatusOK || !slices.Equal(tokenRoles(t, idn, fresh.Token), adminOnly) {
		t.Fatalf("refresh = %d, roles %v", code, tokenRoles(t, idn, fresh.Token))
	}
}

func TestAdmins_RemovingAnAddressRevokesNothing(t *testing.T) {
	cfg := dbtest.Config(t)
	first := newIdentityOn(t, dbtest.OpenConfig(t, cfg), adminsConfig())
	verifiedUser(t, first, "root@test.com")
	if code, tok := login(t, serve(t, first), "root@test.com", "password123"); code != http.StatusOK || !slices.Equal(tokenRoles(t, first, tok), adminOnly) {
		t.Fatalf("login = %d", code)
	}
	// The application restarts with the address gone from the list: the
	// role stays until Users().SetRoles takes it.
	later := newIdentityOn(t, dbtest.OpenConfig(t, cfg), Config{})
	c := serve(t, later)
	code, tok := login(t, c, "root@test.com", "password123")
	if code != http.StatusOK || !slices.Equal(tokenRoles(t, later, tok), adminOnly) {
		t.Fatalf("login after the removal = %d", code)
	}
	u := userByEmail(t, later, "root@test.com")
	if err := later.Users().SetRoles(t.Context(), u.RID, nil); err != nil {
		t.Fatal(err)
	}
	if code, tok := login(t, c, "root@test.com", "password123"); code != http.StatusOK || len(tokenRoles(t, later, tok)) != 0 {
		t.Fatalf("login after SetRoles = %d", code)
	}
}

func TestAdmins_ConcurrentRequestsWriteOnce(t *testing.T) {
	idn := newIdentity(t, adminsConfig())
	c := serve(t, idn)
	tok := register(t, c, "root@test.com", "password123")
	u := userByEmail(t, idn, "root@test.com")
	if err := idn.Users().MarkEmailVerified(t.Context(), u.RID); err != nil {
		t.Fatal(err)
	}
	before := userByEmail(t, idn, "root@test.com")
	// Requests through Authn and logins, all at once.
	codes := make([]int, 16)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			if i%2 == 0 {
				codes[i] = c.WithBearer(tok).GetJSON("/auth/me", nil)
				return
			}
			codes[i] = c.PostJSON("/auth/login", map[string]string{"email": "root@test.com", "password": "password123"}, nil)
		})
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d = %d", i, code)
		}
	}
	after := userByEmail(t, idn, "root@test.com")
	if after.Roles != "admin" || after.Version != before.Version+1 || after.PasswordVersion != before.PasswordVersion {
		t.Fatalf("the account: roles %q, version %d → %d, password_version %d → %d", after.Roles, before.Version, after.Version, before.PasswordVersion, after.PasswordVersion)
	}
	if n := grantRecords(t, idn, u.RID); n != 1 {
		t.Fatalf("grant records = %d, want 1", n)
	}
}

// TestAdmins_GrantCommitsWithItsRecord: the grant and its audit record
// commit together — the record is there when the response is — and a
// grant whose record cannot be written does not happen; the next request
// tries again.
func TestAdmins_GrantCommitsWithItsRecord(t *testing.T) {
	idn := newIdentity(t, adminsConfig())
	c := serve(t, idn)
	u := verifiedUser(t, idn, "root@test.com")
	rename := func(from, to string) {
		t.Helper()
		if err := idn.d.Unsafe(t.Context()).Exec("ALTER TABLE " + from + " RENAME TO " + to).Error; err != nil {
			t.Fatal(err)
		}
	}
	rename("audit_logs", "audit_logs_away")
	code, tok := login(t, c, "root@test.com", "password123")
	if code != http.StatusOK {
		t.Fatalf("login while the log cannot record = %d", code)
	}
	if got := tokenRoles(t, idn, tok); len(got) != 0 {
		t.Fatalf("the token's roles = %v without the grant's record", got)
	}
	if roles := userByEmail(t, idn, "root@test.com").Roles; roles != "" {
		t.Fatalf("granted %q without its record", roles)
	}
	rename("audit_logs_away", "audit_logs")
	if code, tok = login(t, c, "root@test.com", "password123"); code != http.StatusOK {
		t.Fatalf("login once the log records = %d", code)
	}
	if got := tokenRoles(t, idn, tok); !slices.Equal(got, adminOnly) {
		t.Fatalf("the token's roles = %v once the log records", got)
	}
	// Counted before Close flushes the queue: the record came with the grant.
	if n := count(t, idn, "action = ? AND resource_id = ?", "user.role.grant", u.RID); n != 1 {
		t.Fatalf("grant records = %d, want 1", n)
	}
}

func TestResetPassword_VerifiesTheAddress(t *testing.T) {
	sender := newMockSender()
	cfg := adminsConfig()
	idn := newIdentity(t, cfg, Sender(sender.send))
	c := serve(t, idn)
	register(t, c, "root@test.com", "password123")
	if code := c.PostJSON("/auth/forgot-password", map[string]string{"email": "root@test.com"}, nil); code != http.StatusNoContent {
		t.Fatalf("forgot-password = %d", code)
	}
	if !sender.wait(5 * time.Second) {
		t.Fatal("no reset mail")
	}
	if code := c.PostJSON("/auth/reset-password", map[string]string{"token": sender.last().Token, "new_password": "newpassword456"}, nil); code != http.StatusNoContent {
		t.Fatalf("reset-password = %d", code)
	}
	if u := userByEmail(t, idn, "root@test.com"); !u.EmailVerified {
		t.Fatal("a reset through the mailed token verifies the address")
	}
	// A password login is now an administrator's, through auth.admins.
	if code, tok := login(t, c, "root@test.com", "newpassword456"); code != http.StatusOK || !slices.Equal(tokenRoles(t, idn, tok), adminOnly) {
		t.Fatalf("login after the reset = %d", code)
	}
	// An operator's reset and a change of password prove nothing.
	register(t, c, "bob@test.com", "password123")
	bob := userByEmail(t, idn, "bob@test.com")
	if err := idn.Users().ResetPassword(t.Context(), bob.RID, "newpassword456"); err != nil {
		t.Fatal(err)
	}
	_, tok := login(t, c, "bob@test.com", "newpassword456")
	if code := c.WithBearer(tok).JSON(http.MethodPut, "/auth/change-password", map[string]string{"old_password": "newpassword456", "new_password": "password789"}, nil); code != http.StatusNoContent {
		t.Fatalf("change-password = %d", code)
	}
	if u := userByEmail(t, idn, "bob@test.com"); u.EmailVerified {
		t.Fatal("UserService.ResetPassword or change-password verified the address")
	}
}
