package identity

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zynthara/chok/v3"
)

// newUser registers email through the routes and returns the user and a
// token issued before any administration.
func newUser(t *testing.T, email string) (*Identity, *User, string, func(token string) int) {
	t.Helper()
	idn := newIdentity(t, Config{})
	c := serve(t, idn)
	tok := register(t, c, email, "password123")
	me := func(token string) int { return c.WithBearer(token).GetJSON("/auth/me", nil) }
	return idn, userByEmail(t, idn, email), tok, me
}

func TestUsers_CreateAndGet(t *testing.T) {
	idn := newIdentity(t, Config{})
	u, err := idn.Users().Create(t.Context(), " Bob@Test.com", "password123", "")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "bob@test.com" || u.Name != "b*b@t**t.com" || !u.Active || !u.HasPassword {
		t.Fatalf("created %+v", u)
	}
	got, err := idn.Users().Get(t.Context(), u.RID)
	if err != nil || got.Email != "bob@test.com" {
		t.Fatalf("get = %+v %v", got, err)
	}
	if _, err := idn.Users().Create(t.Context(), "bob@test.com", "password123", ""); !errors.Is(err, chok.ErrConflict) {
		t.Fatalf("a second account for the email: %v", err)
	}
	if _, err := idn.Users().Create(t.Context(), "carol@test.com", "weakweak", ""); !errors.Is(err, chok.ErrValidation) {
		t.Fatalf("a weak password: %v", err)
	}
}

func TestUsers_MarkEmailVerified(t *testing.T) {
	idn, u, tok, me := newUser(t, "alice@test.com")
	if u.EmailVerified {
		t.Fatal("a registered email starts unverified")
	}
	if err := idn.Users().MarkEmailVerified(t.Context(), u.RID); err != nil {
		t.Fatal(err)
	}
	after := userByEmail(t, idn, "alice@test.com")
	if !after.EmailVerified || after.PasswordVersion != u.PasswordVersion {
		t.Fatalf("verified=%v pv %d → %d: verification must not revoke tokens", after.EmailVerified, u.PasswordVersion, after.PasswordVersion)
	}
	if code := me(tok); code != http.StatusOK {
		t.Fatalf("the token survives verification: %d", code)
	}
	if err := idn.Users().MarkEmailVerified(t.Context(), u.RID); err != nil {
		t.Fatalf("again is a no-op: %v", err)
	}
}

func TestUsers_MarkEmailVerifiedConcurrently(t *testing.T) {
	idn, u, _, _ := newUser(t, "alice@test.com")
	const n = 5
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() { errs <- idn.Users().MarkEmailVerified(t.Context(), u.RID) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent verification failed (a no-op update must not read as missing): %v", err)
		}
	}
}

func TestUsers_SetRoles(t *testing.T) {
	idn, u, _, _ := newUser(t, "alice@test.com")
	if err := idn.Users().SetRoles(t.Context(), u.RID, []string{"admin", "editor"}); err != nil {
		t.Fatal(err)
	}
	got := userByEmail(t, idn, "alice@test.com")
	if !slices.Equal(got.RoleList(), []string{"admin", "editor"}) || got.PasswordVersion != u.PasswordVersion+1 {
		t.Fatalf("roles %v pv %d", got.RoleList(), got.PasswordVersion)
	}
	for _, clear := range [][]string{nil, {}} {
		if err := idn.Users().SetRoles(t.Context(), u.RID, clear); err != nil {
			t.Fatal(err)
		}
		if got := userByEmail(t, idn, "alice@test.com"); got.Roles != "" || got.RoleList() != nil {
			t.Fatalf("cleared roles: %q", got.Roles)
		}
	}
}

func TestUsers_SetRolesRefusesBadRoles(t *testing.T) {
	idn, u, _, _ := newUser(t, "alice@test.com")
	var many []string
	for i := range 20 {
		many = append(many, strings.Repeat(string(rune('a'+i)), 30))
	}
	for name, roles := range map[string][]string{
		"a comma":       {"admin,editor"},
		"an empty role": {"admin", ""},
		"a long role":   {strings.Repeat("r", 33)},
		"over 500":      many,
	} {
		if err := idn.Users().SetRoles(t.Context(), u.RID, roles); !errors.Is(err, chok.ErrBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := userByEmail(t, idn, "alice@test.com"); got.Roles != "" || got.PasswordVersion != u.PasswordVersion {
		t.Fatalf("a refused change wrote: %+v", got)
	}
	if err := idn.Users().SetRoles(t.Context(), u.RID, []string{strings.Repeat("r", 32)}); err != nil {
		t.Fatalf("32 characters is the limit, not over it: %v", err)
	}
}

func TestUsers_ChangesRevokeTokens(t *testing.T) {
	changes := map[string]func(*Identity, *User) error{
		"SetRoles": func(idn *Identity, u *User) error {
			return idn.Users().SetRoles(t.Context(), u.RID, []string{"editor"})
		},
		"SetActive": func(idn *Identity, u *User) error { return idn.Users().SetActive(t.Context(), u.RID, true) },
		"ResetPassword": func(idn *Identity, u *User) error {
			return idn.Users().ResetPassword(t.Context(), u.RID, "Another-pass1")
		},
		"RevokeTokens": func(idn *Identity, u *User) error { return idn.Users().RevokeTokens(t.Context(), u.RID) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			idn, u, tok, me := newUser(t, "alice@test.com")
			if err := change(idn, u); err != nil {
				t.Fatal(err)
			}
			if got := userByEmail(t, idn, "alice@test.com"); got.PasswordVersion != u.PasswordVersion+1 {
				t.Fatalf("pv %d → %d", u.PasswordVersion, got.PasswordVersion)
			}
			if code := me(tok); code != http.StatusUnauthorized {
				t.Fatalf("a token from before %s = %d", name, code)
			}
			c := serve(t, idn)
			if code := c.WithBearer(tok).PostJSON("/auth/refresh-token", nil, nil); code != http.StatusUnauthorized {
				t.Fatalf("refreshing a revoked token = %d", code)
			}
			if code := c.WithBearer(tok).JSON(http.MethodPut, "/auth/change-password", map[string]string{"old_password": "password123", "new_password": "newpassword456"}, nil); code != http.StatusUnauthorized {
				t.Fatalf("changing the password with a revoked token = %d", code)
			}
		})
	}
}

func TestUsers_ResetPassword(t *testing.T) {
	idn, u, _, _ := newUser(t, "alice@test.com")
	c := serve(t, idn)
	if err := idn.Users().ResetPassword(t.Context(), u.RID, "short"); !errors.Is(err, chok.ErrValidation) {
		t.Fatalf("a short password: %v", err)
	}
	if err := idn.Users().ResetPassword(t.Context(), u.RID, "aaaaaaaaaaaa"); !errors.Is(err, chok.ErrValidation) {
		t.Fatalf("a weak password: %v", err)
	}
	if err := idn.Users().ResetPassword(t.Context(), u.RID, "Reset-pass42"); err != nil {
		t.Fatal(err)
	}
	if code, _ := login(t, c, "alice@test.com", "Reset-pass42"); code != http.StatusOK {
		t.Fatalf("login with the reset password = %d", code)
	}
}

func TestUsers_RevokeTokensCountsEveryCall(t *testing.T) {
	idn, u, _, _ := newUser(t, "alice@test.com")
	const n = 10
	for range n {
		if err := idn.Users().RevokeTokens(t.Context(), u.RID); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() { errs <- idn.Users().RevokeTokens(t.Context(), u.RID) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// The database adds one each time: concurrent calls lose nothing.
	if got := userByEmail(t, idn, "alice@test.com"); got.PasswordVersion != u.PasswordVersion+2*n {
		t.Fatalf("pv %d after %d revocations from %d", got.PasswordVersion, 2*n, u.PasswordVersion)
	}
}

func TestUsers_MissingUserIs404(t *testing.T) {
	idn := newIdentity(t, Config{})
	users := idn.Users()
	ctx := t.Context()
	_, getErr := users.Get(ctx, "usr_nonexistent")
	calls := map[string]error{
		"Get":               getErr,
		"SetActive":         users.SetActive(ctx, "usr_nonexistent", false),
		"SetRoles":          users.SetRoles(ctx, "usr_nonexistent", []string{"x"}),
		"ResetPassword":     users.ResetPassword(ctx, "usr_nonexistent", "Password123"),
		"RevokeTokens":      users.RevokeTokens(ctx, "usr_nonexistent"),
		"MarkEmailVerified": users.MarkEmailVerified(ctx, "usr_nonexistent"),
	}
	for name, err := range calls {
		var e *chok.Error
		if !errors.As(err, &e) || e.Code != http.StatusNotFound || e.Message != "user not found" {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"SetActive":    users.SetActive(ctx, "", false),
		"RevokeTokens": users.RevokeTokens(ctx, ""),
	} {
		if !errors.Is(err, chok.ErrBadRequest) {
			t.Errorf("%s with no id: %v", name, err)
		}
	}
}

func TestUsers_GetByEmail(t *testing.T) {
	idn := newIdentity(t, Config{})
	alice, err := idn.Users().Create(t.Context(), "alice@test.com", "password123", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := idn.Users().GetByEmail(t.Context(), "  Alice@TEST.com ")
	if err != nil || got.RID != alice.RID {
		t.Fatalf("by email = %+v %v", got, err)
	}
	_, err = idn.Users().GetByEmail(t.Context(), "nobody@test.com")
	_, getErr := idn.Users().Get(t.Context(), "usr_missing")
	if !errors.Is(err, chok.ErrNotFound) || err.Error() != getErr.Error() {
		t.Fatalf("a missing address is Get's 404: %v vs %v", err, getErr)
	}
	if _, err := idn.Users().GetByEmail(t.Context(), " "); !errors.Is(err, chok.ErrBadRequest) {
		t.Fatalf("no address: %v", err)
	}
}

func TestUsers_ListFromQuery(t *testing.T) {
	var _ chok.Lister[User] = (*UserService)(nil)
	idn := newIdentity(t, Config{})
	users := idn.Users()
	ctx := t.Context()
	for i := range 25 {
		u, err := users.Create(ctx, fmt.Sprintf("u%02d@test.com", i), "password123", fmt.Sprintf("user %02d", i))
		if err != nil {
			t.Fatal(err)
		}
		if i%5 == 0 {
			if err := users.MarkEmailVerified(ctx, u.RID); err != nil {
				t.Fatal(err)
			}
		}
		if i < 2 {
			if err := users.SetActive(ctx, u.RID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := users.QueryFields(); !slices.Equal(got, []string{"active", "created_at", "email", "email_assumed", "email_verified", "name"}) {
		t.Fatalf("query fields = %v", got)
	}
	// The list answers the admin role only (and what rbac grants
	// user/read): no principal is a 401, another one a 403.
	if _, err := users.ListFromQuery(ctx, url.Values{}); !errors.Is(err, chok.ErrUnauthorized) {
		t.Fatalf("without a principal: %v", err)
	}
	if _, err := users.ListFromQuery(chok.WithPrincipal(ctx, chok.Principal{UserID: "usr_x", Roles: []string{"editor"}}), url.Values{}); !errors.Is(err, chok.ErrForbidden) {
		t.Fatalf("without the admin role: %v", err)
	}
	admin := chok.WithPrincipal(ctx, chok.Principal{UserID: "usr_ops", Roles: []string{"admin"}})
	list := func(raw string) (*chok.Page[User], error) {
		t.Helper()
		q, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		return users.ListFromQuery(admin, q)
	}
	page, err := list("")
	if err != nil || len(page.Items) != 20 || page.Total != 25 || !page.HasMore {
		t.Fatalf("the default page: %d items, total %d, more %v, %v", len(page.Items), page.Total, page.HasMore, err)
	}
	if page, err = list("size=1000"); err != nil || len(page.Items) != 25 || page.Size != 100 {
		t.Fatalf("size is capped at 100: %d items, size %d, %v", len(page.Items), page.Size, err)
	}
	if page, err = list("page=2&size=10&order=email:desc"); err != nil || len(page.Items) != 10 || page.Items[0].Email != "u14@test.com" {
		t.Fatalf("page 2 by email desc: %+v %v", page, err)
	}
	for raw, want := range map[string]int64{
		"email=U03@Test.com":                    1,
		"email=u03@test.com&email=u04@test.com": 2,
		"email_verified=true":                   5,
		"email_verified=1":                      5,
		"active=false":                          2,
		"active=true&email_verified=false":      19,
		"name=user 07":                          1,
	} {
		page, err := list(raw)
		if err != nil || page.Total != want {
			t.Errorf("%s: total %v, want %d, %v", raw, page, want, err)
		}
	}
	for _, raw := range []string{"roles=admin", "id=usr_1", "updated_at=x", "order=roles", "active=maybe", "password_hash=x"} {
		if _, err := list(raw); !errors.Is(err, chok.ErrBadRequest) {
			t.Errorf("%s: %v, want a 400", raw, err)
		}
	}
	// The items are User's JSON: no roles, no active flag, no secrets.
	b, err := json.Marshal(page.Items[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"roles", "active", "password_hash", "password_version"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Fatalf("an item shows %s: %s", key, b)
		}
	}
	// One line serves it. Behind Authn alone it lists no one to a user
	// without the admin role.
	c := serve(t, idn, func(app *chok.App) {
		chok.List(app.Group("/admin", idn.Authn()), "/users", idn.Users())
	})
	tok := register(t, c, "ops@test.com", "password123")
	if code := c.WithBearer(tok).GetJSON("/admin/users", nil); code != http.StatusForbidden {
		t.Fatalf("GET /admin/users without the admin role = %d", code)
	}
	if err := users.SetRoles(ctx, userByEmail(t, idn, "ops@test.com").RID, []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	code, tok := login(t, c, "ops@test.com", "password123")
	if code != http.StatusOK {
		t.Fatalf("login = %d", code)
	}
	var got chok.Page[User]
	if code := c.WithBearer(tok).GetJSON("/admin/users?email=u05@test.com", &got); code != http.StatusOK || got.Total != 1 || got.Items[0].Email != "u05@test.com" {
		t.Fatalf("GET /admin/users as an admin = %d %+v", code, got)
	}
}
