package identity

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/choktest"
	"github.com/zynthara/chok/v3/identity/internal/oauth"
)

// assumedFixture is an Identity with two providers in process: fb, which
// only implies that its addresses are verified, as Facebook does, and g,
// which states them, as Google does; with a Sender, passwordless sign-in
// and link_by_email.
type assumedFixture struct {
	idn    *Identity
	fb, g  *fakeProvider
	sender *mockSender
	c      *choktest.Client
}

func newAssumedFixture(t *testing.T, cfg Config) *assumedFixture {
	t.Helper()
	fb, g := newFake("fb"), newFake("g")
	fb.caps.AssumesEmail = true
	cfg.OAuthCallbackFrontendURL = frontendURL
	cfg.LinkByEmail = true
	cfg.Passwordless.Enabled = true
	sender := newMockSender()
	idn := newIdentity(t, cfg, Providers(fb, g), Sender(sender.send))
	return &assumedFixture{idn: idn, fb: fb, g: g, sender: sender, c: serve(t, idn)}
}

// fbIdentity is fb's account with email, implied verified.
func fbIdentity(account, email string) *oauth.Identity {
	return &oauth.Identity{Provider: "fb", ProviderAccountID: account, Email: email, EmailVerified: true, EmailVerifiedAssumed: true, Name: "FB"}
}

// gIdentity is g's account with email, stated verified.
func gIdentity(account, email string) *oauth.Identity {
	return &oauth.Identity{Provider: "g", ProviderAccountID: account, Email: email, EmailVerified: true, Name: "G"}
}

// providerSignIn signs in through provider with code — start, callback,
// exchange — and returns the callback's status and the token, "" unless
// the callback sent the browser on with a code.
func providerSignIn(t *testing.T, c *choktest.Client, provider, code string) (int, string) {
	t.Helper()
	start := c.Do(httptest.NewRequest(http.MethodGet, "/auth/"+provider+"/start", nil))
	if start.StatusCode != http.StatusFound {
		t.Fatalf("%s start = %d", provider, start.StatusCode)
	}
	state := query(t, start.Header.Get("Location"), "state")
	req := httptest.NewRequest(http.MethodGet, "/auth/"+provider+"/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	for _, ck := range start.Cookies() {
		req.AddCookie(ck)
	}
	cb := c.Do(req)
	if cb.StatusCode != http.StatusFound {
		return cb.StatusCode, ""
	}
	xreq := jsonRequest(t, http.MethodPost, "/auth/exchange", map[string]string{"code": query(t, cb.Header.Get("Location"), "code")})
	for _, ck := range cb.Cookies() {
		xreq.AddCookie(ck)
	}
	resp := c.Do(xreq)
	var out exchangeResponse
	decodeBody(t, resp, &out)
	if resp.StatusCode != http.StatusOK || out.Token == "" {
		t.Fatalf("%s exchange = %d", provider, resp.StatusCode)
	}
	return cb.StatusCode, out.Token
}

// facebookAccount signs email up through fb, as a Facebook sign-up makes
// its account, and returns the account and its token.
func (fx *assumedFixture) facebookAccount(t *testing.T, account, email string) (*User, string) {
	t.Helper()
	fx.fb.seed(account, fbIdentity(account, email))
	code, tok := providerSignIn(t, fx.c, "fb", account)
	if code != http.StatusFound {
		t.Fatalf("the fb sign-up = %d", code)
	}
	u := userByEmail(t, fx.idn, email)
	if !u.EmailVerified || !u.EmailAssumed {
		t.Fatalf("the fb account's address: %+v", u)
	}
	return u, tok
}

// linkedProviders is the providers linked to the user, in link order.
func linkedProviders(t *testing.T, idn *Identity, userID string) []string {
	t.Helper()
	links, err := idn.oauth.links(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range links {
		out = append(out, l.Provider)
	}
	return out
}

// otherCode is a code of the same length that is not code.
func otherCode(code string) string {
	last := (code[len(code)-1]-'0'+1)%10 + '0'
	return code[:len(code)-1] + string(rune(last))
}

// confirm posts code to confirm-email with tok and returns the status and
// the envelope of an error.
func confirm(c *choktest.Client, tok, code string) (int, envelope) {
	var e envelope
	status := c.WithBearer(tok).PostJSON("/auth/confirm-email", map[string]string{"code": code}, &e)
	return status, e
}

// startConfirm asks confirm-email/start for a code with tok and returns
// the mail.
func (fx *assumedFixture) startConfirm(t *testing.T, tok string) Mail {
	t.Helper()
	if code := fx.c.WithBearer(tok).PostJSON("/auth/confirm-email/start", nil, nil); code != http.StatusAccepted {
		t.Fatalf("confirm-email/start = %d", code)
	}
	if !fx.sender.wait(5 * time.Second) {
		t.Fatal("no confirmation mail")
	}
	return fx.sender.last()
}

// TestAssumedAddress_GetsNoAdminUntilConfirmed: an account Facebook made
// before auth.admins listed its address gets no admin role on Facebook's
// word. Its user confirms the address with a code mailed to it and typed
// in the same session; the session goes on, the Facebook link stays, and
// the admin role comes.
func TestAssumedAddress_GetsNoAdminUntilConfirmed(t *testing.T) {
	fx := newAssumedFixture(t, adminsConfig())
	// fb cannot make a listed address's account now: make it as fb did
	// before the address was listed.
	hash, err := hashPassword(randomSecret(), fx.idn.cost)
	if err != nil {
		t.Fatal(err)
	}
	u := &User{Email: "root@test.com", EmailVerified: true, EmailAssumed: true, PasswordHash: hash, Active: true}
	if err := fx.idn.users.Create(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.idn.oauth.linkTx(t.Context(), u.RID, fbIdentity("fb-root", "root@test.com")); err != nil {
		t.Fatal(err)
	}
	fx.fb.seed("c", fbIdentity("fb-root", "root@test.com"))
	code, tok := providerSignIn(t, fx.c, "fb", "c")
	if code != http.StatusFound {
		t.Fatalf("the fb sign-in = %d", code)
	}
	var me meResponse
	if code := fx.c.WithBearer(tok).GetJSON("/auth/me", &me); code != http.StatusOK || len(me.Roles) != 0 || !me.EmailVerified || !me.EmailAssumed {
		t.Fatalf("an assumed listed address = %d %+v", code, me)
	}
	m := fx.startConfirm(t, tok)
	if m.Kind != "confirm-email" || m.To != "root@test.com" || len(m.Code) != 6 || m.Token != "" || m.Link != "" {
		t.Fatalf("the mail: %+v", m)
	}
	if code, e := confirm(fx.c, tok, otherCode(m.Code)); code != http.StatusBadRequest || e.Reason != "InvalidCode" || e.Metadata["attempts_left"] != float64(4) {
		t.Fatalf("a wrong code = %d %+v", code, e)
	}
	if code, e := confirm(fx.c, tok, m.Code); code != http.StatusNoContent {
		t.Fatalf("the code = %d %+v", code, e)
	}
	if code := fx.c.WithBearer(tok).GetJSON("/auth/me", &me); code != http.StatusOK || !slices.Equal(me.Roles, []string{adminRole}) || !me.EmailVerified || me.EmailAssumed {
		t.Fatalf("the same session after the confirmation = %d %+v", code, me)
	}
	if got := linkedProviders(t, fx.idn, u.RID); !slices.Equal(got, []string{"fb"}) {
		t.Fatalf("links after the confirmation: %v", got)
	}
	if code, e := confirm(fx.c, tok, m.Code); code != http.StatusBadRequest || e.Reason != "CodeExpired" {
		t.Fatalf("the code again = %d %+v", code, e)
	}
}

// TestAssumedAddress_TakenOverFromOutside: whoever signed up through
// Facebook with an address need not own it. The first proof of the
// address from outside the account — a mailed reset token, a mailed code,
// a link_by_email sign-in through a provider that states it — takes the
// account over clean: the Facebook link goes, the tokens stop working,
// and the Facebook account no longer reaches it; the prover is signed in.
func TestAssumedAddress_TakenOverFromOutside(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prove func(t *testing.T, fx *assumedFixture, email string) string // the prover's token
		links []string                                                    // linked afterwards
	}{
		{"reset-password", func(t *testing.T, fx *assumedFixture, email string) string {
			if code := fx.c.PostJSON("/auth/forgot-password", map[string]string{"email": email}, nil); code != http.StatusNoContent {
				t.Fatalf("forgot-password = %d", code)
			}
			if !fx.sender.wait(5 * time.Second) {
				t.Fatal("no reset mail")
			}
			if code := fx.c.PostJSON("/auth/reset-password", map[string]string{"token": fx.sender.last().Token, "new_password": "ownerpass456"}, nil); code != http.StatusNoContent {
				t.Fatalf("reset-password = %d", code)
			}
			code, tok := login(t, fx.c, email, "ownerpass456")
			if code != http.StatusOK {
				t.Fatalf("login with the new password = %d", code)
			}
			return tok
		}, nil},
		{"passwordless", func(t *testing.T, fx *assumedFixture, email string) string {
			m := startCode(t, fx.c, fx.sender, email)
			code, out, e := verify(fx.c, map[string]string{"email": email, "code": m.Code})
			if code != http.StatusOK {
				t.Fatalf("verify = %d %+v", code, e)
			}
			return out.Token
		}, nil},
		{"link_by_email", func(t *testing.T, fx *assumedFixture, email string) string {
			fx.g.seed("g-code", gIdentity("g-owner", email))
			code, tok := providerSignIn(t, fx.c, "g", "g-code")
			if code != http.StatusFound {
				t.Fatalf("the g sign-in = %d", code)
			}
			return tok
		}, []string{"g"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newAssumedFixture(t, Config{})
			u, squatter := fx.facebookAccount(t, "fb-squatter", "alice@test.com")
			owner := tc.prove(t, fx, "alice@test.com")
			after := userByEmail(t, fx.idn, "alice@test.com")
			if after.RID != u.RID || !after.EmailVerified || after.EmailAssumed {
				t.Fatalf("the account after the proof: %+v", after)
			}
			if got := linkedProviders(t, fx.idn, u.RID); !slices.Equal(got, tc.links) {
				t.Fatalf("links after the proof: %v", got)
			}
			if code := fx.c.WithBearer(squatter).GetJSON("/auth/me", nil); code != http.StatusUnauthorized {
				t.Fatalf("the earlier token = %d", code)
			}
			var me meResponse
			if code := fx.c.WithBearer(owner).GetJSON("/auth/me", &me); code != http.StatusOK || me.ID != u.RID || me.EmailAssumed {
				t.Fatalf("the prover's token: %d %+v", code, me)
			}
			fx.fb.seed("again", fbIdentity("fb-squatter", "alice@test.com"))
			if code, _ := providerSignIn(t, fx.c, "fb", "again"); code != http.StatusConflict {
				t.Fatalf("the fb account after the proof = %d", code)
			}
		})
	}
}

// TestConfirmEmail_Mounted: the confirm-email routes come with a Sender
// and a provider that only implies its addresses are verified, behind
// Authn. A confirmation is a proof made in the session, so it keeps what
// a takeover would clear: an unverified password account confirms its
// address and logs in with its password still.
func TestConfirmEmail_Mounted(t *testing.T) {
	for _, tc := range []struct {
		name            string
		sender, assumes bool
		want            int
	}{
		{"a Sender and an assuming provider", true, true, http.StatusAccepted},
		{"no Sender", false, true, http.StatusNotFound},
		{"providers that state their addresses", true, false, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newFake("fake")
			p.caps.AssumesEmail = tc.assumes
			sender := newMockSender()
			opts := []Option{Providers(p)}
			if tc.sender {
				opts = append(opts, Sender(sender.send))
			}
			idn := newIdentity(t, Config{OAuthCallbackFrontendURL: frontendURL}, opts...)
			c := serve(t, idn)
			tok := register(t, c, "alice@test.com", "password123")
			if code := c.WithBearer(tok).PostJSON("/auth/confirm-email/start", nil, nil); code != tc.want {
				t.Fatalf("start = %d", code)
			}
			if tc.want != http.StatusAccepted {
				return
			}
			if code := c.PostJSON("/auth/confirm-email/start", nil, nil); code != http.StatusUnauthorized {
				t.Fatalf("start without a token = %d", code)
			}
			if !sender.wait(5 * time.Second) {
				t.Fatal("no confirmation mail")
			}
			if code, e := confirm(c, tok, sender.last().Code); code != http.StatusNoContent {
				t.Fatalf("confirm = %d %+v", code, e)
			}
			if u := userByEmail(t, idn, "alice@test.com"); !u.EmailVerified || u.EmailAssumed || !u.HasPassword {
				t.Fatalf("after the confirmation: %+v", u)
			}
			if code, _ := login(t, c, "alice@test.com", "password123"); code != http.StatusOK {
				t.Fatalf("the password after the confirmation = %d", code)
			}
		})
	}
}

// TestConfirmEmail_Codes: a confirmation code is the session's alone — a
// passwordless sign-in cannot use it — is mailed once per resend_after,
// survives max_attempts wrong tries and no more, and expires.
func TestConfirmEmail_Codes(t *testing.T) {
	fx := newAssumedFixture(t, Config{})
	_, tok := fx.facebookAccount(t, "fb-1", "alice@test.com")
	m := fx.startConfirm(t, tok)
	if code := fx.c.WithBearer(tok).PostJSON("/auth/confirm-email/start", nil, nil); code != http.StatusAccepted {
		t.Fatalf("a second start = %d", code)
	}
	if fx.sender.wait(200 * time.Millisecond) {
		t.Fatal("a second mail within resend_after")
	}
	if code, _, e := verify(fx.c, map[string]string{"email": "alice@test.com", "code": m.Code}); code != http.StatusUnauthorized || e.Reason != "CodeExpired" {
		t.Fatalf("the confirmation code at passwordless verify = %d %+v", code, e)
	}
	for left := 4; left >= 0; left-- {
		if code, e := confirm(fx.c, tok, otherCode(m.Code)); code != http.StatusBadRequest || e.Reason != "InvalidCode" || e.Metadata["attempts_left"] != float64(left) {
			t.Fatalf("wrong code, %d left = %d %+v", left, code, e)
		}
	}
	if code, e := confirm(fx.c, tok, m.Code); code != http.StatusBadRequest || e.Reason != "CodeExpired" {
		t.Fatalf("the right code after max_attempts = %d %+v", code, e)
	}
	// A fresh code, past resend_after, expires unused.
	fresh := newAssumedFixture(t, Config{Passwordless: PasswordlessConfig{ResendAfter: time.Millisecond}})
	u, tok := fresh.facebookAccount(t, "fb-2", "bob@test.com")
	m = fresh.startConfirm(t, tok)
	if err := fresh.idn.d.Unsafe(t.Context()).Model(&loginCode{}).Where("email = ?", u.RID).Update("expires_at", time.Now().Add(-time.Minute).UTC()).Error; err != nil {
		t.Fatal(err)
	}
	if code, e := confirm(fresh.c, tok, m.Code); code != http.StatusBadRequest || e.Reason != "CodeExpired" {
		t.Fatalf("an expired code = %d %+v", code, e)
	}
	if code, e := confirm(fresh.c, tok, "123456"); code != http.StatusBadRequest || e.Reason != "CodeExpired" {
		t.Fatalf("no code at all = %d %+v", code, e)
	}
	if userByEmail(t, fresh.idn, "bob@test.com").EmailAssumed == false {
		t.Fatal("confirmed without a code")
	}
}

// TestOAuth_LinkingTheStatedAddressConfirmsIt: linking, from the session,
// a provider that states the account's own address confirms an address
// Facebook only implied; another address, or one the provider only
// implies too, leaves it assumed.
func TestOAuth_LinkingTheStatedAddressConfirmsIt(t *testing.T) {
	fx := newAssumedFixture(t, Config{})
	u, _ := fx.facebookAccount(t, "fb-1", "alice@test.com")
	for _, ext := range []*oauth.Identity{gIdentity("g-other", "other@test.com"), fbIdentity("fb-2", "alice@test.com")} {
		if _, err := fx.idn.oauth.link(t.Context(), u.RID, ext); err != nil {
			t.Fatal(err)
		}
		if !userByEmail(t, fx.idn, "alice@test.com").EmailAssumed {
			t.Fatalf("linking %s %s confirmed the address", ext.Provider, ext.Email)
		}
	}
	if _, err := fx.idn.oauth.link(t.Context(), u.RID, gIdentity("g-1", "Alice@Test.com")); err != nil {
		t.Fatal(err)
	}
	if after := userByEmail(t, fx.idn, "alice@test.com"); !after.EmailVerified || after.EmailAssumed {
		t.Fatalf("after linking g with the address: %+v", after)
	}
	if got := linkedProviders(t, fx.idn, u.RID); len(got) != 4 {
		t.Fatalf("links: %v", got)
	}
}

// TestUsers_MarkEmailVerifiedLeavesAnAssumedAddress: the application's
// own verification cannot tell who opened its mail, so it neither takes an
// account Facebook made over — locking out its holder — nor settles the
// address for whoever holds the account. The list finds such accounts.
func TestUsers_MarkEmailVerifiedLeavesAnAssumedAddress(t *testing.T) {
	fx := newAssumedFixture(t, Config{})
	u, tok := fx.facebookAccount(t, "fb-1", "alice@test.com")
	verifiedUser(t, fx.idn, "bob@test.com")
	if err := fx.idn.Users().MarkEmailVerified(t.Context(), u.RID); err != nil {
		t.Fatal(err)
	}
	if after := userByEmail(t, fx.idn, "alice@test.com"); !after.EmailAssumed {
		t.Fatalf("after MarkEmailVerified: %+v", after)
	}
	if got := linkedProviders(t, fx.idn, u.RID); !slices.Equal(got, []string{"fb"}) {
		t.Fatalf("links after MarkEmailVerified: %v", got)
	}
	if code := fx.c.WithBearer(tok).GetJSON("/auth/me", nil); code != http.StatusOK {
		t.Fatalf("the token after MarkEmailVerified = %d", code)
	}
	ctx := chok.WithPrincipal(t.Context(), chok.Principal{UserID: "usr_ops", Roles: []string{adminRole}})
	page, err := fx.idn.Users().ListFromQuery(ctx, url.Values{"email_assumed": {"true"}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Email != "alice@test.com" {
		t.Fatalf("email_assumed=true: %+v %v", page, err)
	}
}
