package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zynthara/chok/v3/choktest"
)

// pwIdentity is an Identity with passwordless sign-in, mailing through a
// mockSender.
func pwIdentity(t *testing.T, cfg Config) (*Identity, *mockSender) {
	t.Helper()
	cfg.Passwordless.Enabled = true
	sender := newMockSender()
	return newIdentity(t, cfg, Sender(sender.send)), sender
}

// startCode asks for a code for email and returns the mail it came in.
func startCode(t *testing.T, c *choktest.Client, sender *mockSender, email string) Mail {
	t.Helper()
	if code := c.PostJSON("/auth/passwordless/start", map[string]string{"email": email}, nil); code != http.StatusAccepted {
		t.Fatalf("start %s = %d", email, code)
	}
	if !sender.wait(5 * time.Second) {
		t.Fatalf("no mail for %s", email)
	}
	return sender.last()
}

// verify posts body to verify and returns the status, the token body and
// the error envelope.
func verify(c *choktest.Client, body map[string]string) (int, tokenBody, envelope) {
	var raw map[string]any
	code := c.PostJSON("/auth/passwordless/verify", body, &raw)
	var tok tokenBody
	var e envelope
	if code == http.StatusOK {
		tok.Token, _ = raw["token"].(string)
		at, _ := raw["expires_at"].(string)
		tok.ExpiresAt, _ = time.Parse(time.RFC3339Nano, at)
	} else {
		e.Code, _ = raw["code"].(int)
		e.Reason, _ = raw["reason"].(string)
		e.Message, _ = raw["message"].(string)
		e.Metadata, _ = raw["metadata"].(map[string]any)
	}
	return code, tok, e
}

// loginCodeOf reads the row of email.
func loginCodeOf(t *testing.T, idn *Identity, email string) (loginCode, bool) {
	t.Helper()
	var row loginCode
	err := idn.d.Unsafe(t.Context()).Where("email = ?", email).Take(&row).Error
	return row, err == nil
}

var sixDigits = regexp.MustCompile(`^\d{6}$`)

// TestPasswordless_StartAnswers202: a code and a magic-link token go out
// in the background, the table keeps only their HMACs, and a second start
// within resend_after changes nothing and mails nothing.
func TestPasswordless_StartAnswers202(t *testing.T) {
	idn, sender := pwIdentity(t, Config{})
	c := serve(t, idn)
	register(t, c, "alice@test.com", "password123")
	m := startCode(t, c, sender, "Alice@Test.com")
	if m.Kind != "login-code" || m.To != "alice@test.com" || !sixDigits.MatchString(m.Code) || len(m.Token) != 43 || m.Link != "" ||
		m.ExpiresAt.Location() != time.UTC || time.Until(m.ExpiresAt) < 9*time.Minute || time.Until(m.ExpiresAt) > 10*time.Minute {
		t.Fatalf("the mail: %+v", m)
	}
	row, ok := loginCodeOf(t, idn, "alice@test.com")
	if !ok || row.CodeHash != idn.mac(m.Code) || row.TokenHash != idn.mac(m.Token) || row.Attempts != 0 || len(row.CodeHash) != 64 {
		t.Fatalf("the row: %+v", row)
	}
	// Within resend_after: the same 202, no new code, no mail.
	if code := c.PostJSON("/auth/passwordless/start", map[string]string{"email": "alice@test.com"}, nil); code != http.StatusAccepted {
		t.Fatalf("start again = %d", code)
	}
	if sender.wait(200 * time.Millisecond) {
		t.Fatal("a second mail within resend_after")
	}
	if again, _ := loginCodeOf(t, idn, "alice@test.com"); again.CodeHash != row.CodeHash || again.ID != row.ID {
		t.Fatal("the code changed within resend_after")
	}
	// Once resend_after has passed, a new code replaces the old one.
	if err := idn.d.Unsafe(t.Context()).Model(&loginCode{}).Where("id = ?", row.ID).Update("created_at", time.Now().UTC().Add(-61*time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	m2 := startCode(t, c, sender, "alice@test.com")
	if m2.Code == m.Code && m2.Token == m.Token {
		t.Fatal("the same code twice")
	}
	if code, _, e := verify(c, map[string]string{"email": "alice@test.com", "code": m.Code}); code != http.StatusUnauthorized || e.Reason != "InvalidCode" {
		t.Fatalf("the replaced code = %d %+v", code, e)
	}
	// An address without an account gets a code too: the same answer.
	if nobody := startCode(t, c, sender, "nobody@test.com"); nobody.To != "nobody@test.com" || nobody.Kind != "login-code" {
		t.Fatalf("the unknown address's mail: %+v", nobody)
	}
}

// TestPasswordless_StartIsThrottledPerClient: starts from one client
// address are counted in the login throttle's table, on their own: over
// the limit they are 429 with retry_after, while another address and the
// password login carry on.
func TestPasswordless_StartIsThrottledPerClient(t *testing.T) {
	idn, _ := pwIdentity(t, Config{LoginRateWindow: time.Minute, LoginRateLimit: 3})
	h, _ := loggedApp(t, idn)
	from := func(addr string, path string, body any) *http.Response {
		req := jsonRequest(t, http.MethodPost, path, body)
		req.RemoteAddr = addr + ":40000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result()
	}
	for i := range 3 {
		if resp := from("198.51.100.7", "/auth/passwordless/start", map[string]string{"email": "user" + string(rune('a'+i)) + "@test.com"}); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("start %d = %d", i, resp.StatusCode)
		}
	}
	resp := from("198.51.100.7", "/auth/passwordless/start", map[string]string{"email": "another@test.com"})
	var e envelope
	decodeBody(t, resp, &e)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "60" || e.Reason != "RateLimited" || e.Metadata["retry_after"] != float64(60) {
		t.Fatalf("the fourth start = %d %+v %v", resp.StatusCode, e, resp.Header)
	}
	if resp := from("198.51.100.8", "/auth/passwordless/start", map[string]string{"email": "another@test.com"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("another client = %d", resp.StatusCode)
	}
	if resp := from("198.51.100.7", "/auth/login", map[string]string{"email": "x@test.com", "password": "password123"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a login from the throttled client = %d: the starts count apart from failed logins", resp.StatusCode)
	}
}

// TestPasswordless_VerifySignsIn: a registered address signs in and its
// email is verified; an unknown one gets an account first. The token is
// a login's, and the code is gone.
func TestPasswordless_VerifySignsIn(t *testing.T) {
	idn, sender := pwIdentity(t, Config{})
	c := serve(t, idn)
	if _, err := idn.Users().Create(t.Context(), "alice@test.com", "password123", "Alice"); err != nil {
		t.Fatal(err)
	}
	m := startCode(t, c, sender, "alice@test.com")
	code, tok, e := verify(c, map[string]string{"email": "ALICE@test.com", "code": m.Code})
	if code != http.StatusOK || tok.Token == "" || time.Until(tok.ExpiresAt) < time.Hour {
		t.Fatalf("verify = %d %+v %+v", code, tok, e)
	}
	var me meResponse
	if code := c.WithBearer(tok.Token).GetJSON("/auth/me", &me); code != http.StatusOK || me.Email != "alice@test.com" || me.Name != "Alice" || !me.EmailVerified {
		t.Fatalf("me = %d %+v", code, me)
	}
	if _, ok := loginCodeOf(t, idn, "alice@test.com"); ok {
		t.Fatal("the code outlives the sign-in")
	}
	if u := userByEmail(t, idn, "alice@test.com"); !u.HasPassword {
		t.Fatal("the password stays")
	}

	// An unknown address: the account comes with the first sign-in.
	m = startCode(t, c, sender, "bob@test.com")
	code, tok, e = verify(c, map[string]string{"email": "bob@test.com", "code": m.Code})
	if code != http.StatusOK || tok.Token == "" {
		t.Fatalf("verify an unknown address = %d %+v", code, e)
	}
	u := userByEmail(t, idn, "bob@test.com")
	if !u.Active || !u.EmailVerified || u.HasPassword || u.Name != "" || u.Roles != "" || !strings.HasPrefix(u.RID, "usr_") {
		t.Fatalf("the new account: %+v", u)
	}
	if code := c.WithBearer(tok.Token).GetJSON("/auth/me", &me); code != http.StatusOK || me.ID != u.RID || me.Name != "" || len(me.Roles) != 0 {
		t.Fatalf("me = %d %+v", code, me)
	}
	// It has no password to log in with; the reason says so.
	if code, _ := login(t, c, "bob@test.com", "anything123"); code != http.StatusUnauthorized {
		t.Fatalf("a password login = %d", code)
	}
}

// TestPasswordless_IgnoresDisableRegister: disable_register drops the
// register route only; a first sign-in by code still makes the account.
func TestPasswordless_IgnoresDisableRegister(t *testing.T) {
	idn, sender := pwIdentity(t, Config{DisableRegister: true})
	c := serve(t, idn)
	if code := c.PostJSON("/auth/register", map[string]string{"email": "bob@test.com", "password": "password123"}, nil); code != http.StatusNotFound {
		t.Fatalf("register with disable_register = %d", code)
	}
	m := startCode(t, c, sender, "bob@test.com")
	if code, tok, e := verify(c, map[string]string{"email": "bob@test.com", "code": m.Code}); code != http.StatusOK || tok.Token == "" {
		t.Fatalf("a first sign-in with disable_register = %d %+v", code, e)
	}
	if u := userByEmail(t, idn, "bob@test.com"); !u.EmailVerified || u.HasPassword {
		t.Fatalf("the account: %+v", u)
	}
}

// TestPasswordless_Only: with only, the password routes are not there —
// a 404 envelope, and nothing in the OpenAPI document — while the code
// sign-in, refresh-token, me and logout work, and the administration can
// still create accounts and set passwords.
func TestPasswordless_Only(t *testing.T) {
	sender := newMockSender()
	idn := newIdentity(t, Config{Passwordless: PasswordlessConfig{Enabled: true, Only: true}, Cookie: CookieConfig{Enabled: true}}, Sender(sender.send))
	c := serve(t, idn)
	gone := [][2]string{
		{http.MethodPost, "/auth/register"}, {http.MethodPost, "/auth/login"}, {http.MethodPost, "/auth/forgot-password"},
		{http.MethodPost, "/auth/reset-password"}, {http.MethodPut, "/auth/change-password"},
	}
	for _, route := range gone {
		var e envelope
		if code := c.JSON(route[0], route[1], map[string]string{"email": "alice@test.com", "password": "password123"}, &e); code != http.StatusNotFound || e.Reason != "NotFound" {
			t.Errorf("%s %s with only = %d %+v", route[0], route[1], code, e)
		}
	}
	m := startCode(t, c, sender, "alice@test.com")
	code, tok, e := verify(c, map[string]string{"email": "alice@test.com", "code": m.Code})
	if code != http.StatusOK || tok.Token == "" {
		t.Fatalf("verify = %d %+v", code, e)
	}
	authed := c.WithBearer(tok.Token)
	if code := authed.GetJSON("/auth/me", nil); code != http.StatusOK {
		t.Fatalf("me = %d", code)
	}
	if code := authed.PostJSON("/auth/refresh-token", nil, nil); code != http.StatusOK {
		t.Fatalf("refresh-token = %d", code)
	}
	if code := authed.PostJSON("/auth/logout", nil, nil); code != http.StatusNoContent {
		t.Fatalf("logout = %d", code)
	}
	// The administration still provisions accounts and sets passwords.
	u, err := idn.Users().Create(t.Context(), "ops@test.com", "password123", "Ops")
	if err != nil {
		t.Fatal(err)
	}
	if err := idn.Users().ResetPassword(t.Context(), userByEmail(t, idn, "alice@test.com").RID, "another-pass1"); err != nil {
		t.Fatalf("ResetPassword with only: %v", err)
	}
	if !userByEmail(t, idn, "alice@test.com").HasPassword || !u.HasPassword {
		t.Fatal("the administration's passwords")
	}
	var s spec
	if code := c.GetJSON("/openapi.json", &s); code != http.StatusOK {
		t.Fatalf("openapi = %d", code)
	}
	for _, route := range gone {
		if s.Paths[route[1]] != nil {
			t.Errorf("%s is documented: %v", route[1], s.Paths[route[1]])
		}
	}
	for _, path := range []string{"/auth/passwordless/start", "/auth/passwordless/verify", "/auth/refresh-token", "/auth/me", "/auth/logout"} {
		if s.Paths[path] == nil {
			t.Errorf("%s is not documented", path)
		}
	}
}

// TestPasswordless_InvalidCode: a wrong code is InvalidCode with
// attempts_left; after max_attempts the code is used up, the right one
// included.
func TestPasswordless_InvalidCode(t *testing.T) {
	idn, sender := pwIdentity(t, Config{Passwordless: PasswordlessConfig{MaxAttempts: 3, CodeLength: 8}})
	c := serve(t, idn)
	m := startCode(t, c, sender, "alice@test.com")
	if len(m.Code) != 8 {
		t.Fatalf("code_length 8: %q", m.Code)
	}
	wrong := "00000000"
	if m.Code == wrong {
		wrong = "11111111"
	}
	for left := 2; left >= 0; left-- {
		code, _, e := verify(c, map[string]string{"email": "alice@test.com", "code": wrong})
		if code != http.StatusUnauthorized || e.Reason != "InvalidCode" || e.Metadata["attempts_left"] != float64(left) || e.Message != "the code is not right" {
			t.Fatalf("a wrong code with %d left = %d %+v", left, code, e)
		}
	}
	if code, _, e := verify(c, map[string]string{"email": "alice@test.com", "code": m.Code}); code != http.StatusUnauthorized || e.Reason != "CodeExpired" {
		t.Fatalf("the right code after max_attempts = %d %+v", code, e)
	}
	if row, _ := loginCodeOf(t, idn, "alice@test.com"); row.Attempts != 3 {
		t.Fatalf("attempts %d", row.Attempts)
	}
}

// TestPasswordless_CodeExpired: every way a code is gone is CodeExpired —
// never asked for, expired, used, tried max_attempts times — by code and
// by token.
func TestPasswordless_CodeExpired(t *testing.T) {
	idn, sender := pwIdentity(t, Config{Passwordless: PasswordlessConfig{MaxAttempts: 1}})
	c := serve(t, idn)
	expired := func(how string, body map[string]string) {
		t.Helper()
		if code, _, e := verify(c, body); code != http.StatusUnauthorized || e.Reason != "CodeExpired" || !strings.Contains(e.Message, "ask for a new one") {
			t.Errorf("%s = %d %+v", how, code, e)
		}
	}
	expired("never asked for", map[string]string{"email": "nobody@test.com", "code": "123456"})
	expired("an unknown token", map[string]string{"token": strings.Repeat("A", 43)})

	m := startCode(t, c, sender, "late@test.com")
	if err := idn.d.Unsafe(t.Context()).Model(&loginCode{}).Where("email = ?", "late@test.com").Update("expires_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	expired("expired, by code", map[string]string{"email": "late@test.com", "code": m.Code})
	expired("expired, by token", map[string]string{"token": m.Token})

	m = startCode(t, c, sender, "used@test.com")
	if code, _, _ := verify(c, map[string]string{"email": "used@test.com", "code": m.Code}); code != http.StatusOK {
		t.Fatalf("the first use = %d", code)
	}
	expired("used, by code", map[string]string{"email": "used@test.com", "code": m.Code})
	expired("used, by token", map[string]string{"token": m.Token})

	m = startCode(t, c, sender, "tried@test.com")
	wrong := "000000"
	if m.Code == wrong {
		wrong = "111111"
	}
	if code, _, e := verify(c, map[string]string{"email": "tried@test.com", "code": wrong}); e.Reason != "InvalidCode" || e.Metadata["attempts_left"] != float64(0) {
		t.Fatalf("the one attempt = %d %+v", code, e)
	}
	expired("tried max_attempts times", map[string]string{"email": "tried@test.com", "code": m.Code})
}

// TestPasswordless_MagicLink: with magic_link_url the mail carries the
// sign-in page with the token; the token alone signs in, and decides when
// a code comes with it.
func TestPasswordless_MagicLink(t *testing.T) {
	idn, sender := pwIdentity(t, Config{Passwordless: PasswordlessConfig{MagicLinkURL: "https://app.example/sign-in"}})
	c := serve(t, idn)
	m := startCode(t, c, sender, "alice@test.com")
	if m.Link != "https://app.example/sign-in?token="+m.Token {
		t.Fatalf("link %q", m.Link)
	}
	wrong := "000000"
	if m.Code == wrong {
		wrong = "111111"
	}
	code, tok, e := verify(c, map[string]string{"token": m.Token, "email": "someone@test.com", "code": wrong})
	if code != http.StatusOK || tok.Token == "" {
		t.Fatalf("verify by token = %d %+v", code, e)
	}
	var me meResponse
	if c.WithBearer(tok.Token).GetJSON("/auth/me", &me); me.Email != "alice@test.com" || !me.EmailVerified {
		t.Fatalf("me %+v", me)
	}
	if code, _, e := verify(c, map[string]string{"email": "alice@test.com", "code": m.Code}); code != http.StatusUnauthorized || e.Reason != "CodeExpired" {
		t.Fatalf("the code after its token = %d %+v", code, e)
	}
}

// TestPasswordless_DisabledAccount: a disabled account is mailed no code,
// and one asked for before it was disabled is refused as login refuses
// it.
func TestPasswordless_DisabledAccount(t *testing.T) {
	idn, sender := pwIdentity(t, Config{})
	c := serve(t, idn)
	u, err := idn.Users().Create(t.Context(), "alice@test.com", "password123", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	m := startCode(t, c, sender, "alice@test.com")
	if err := idn.Users().SetActive(t.Context(), u.RID, false); err != nil {
		t.Fatal(err)
	}
	code, _, e := verify(c, map[string]string{"email": "alice@test.com", "code": m.Code})
	_, loginEnvelope := func() (int, envelope) {
		var le envelope
		return c.PostJSON("/auth/login", map[string]string{"email": "alice@test.com", "password": "password123"}, &le), le
	}()
	if code != http.StatusUnauthorized || e.Reason != loginEnvelope.Reason || e.Message != loginEnvelope.Message || e.Message != "account is disabled" {
		t.Fatalf("verify for a disabled account = %d %+v, login says %+v", code, e, loginEnvelope)
	}
	if err := idn.d.Unsafe(t.Context()).Model(&loginCode{}).Where("email = ?", "alice@test.com").Update("created_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if code := c.PostJSON("/auth/passwordless/start", map[string]string{"email": "alice@test.com"}, nil); code != http.StatusAccepted {
		t.Fatalf("start = %d", code)
	}
	if sender.wait(200 * time.Millisecond) {
		t.Fatal("a code mailed to a disabled account")
	}
}

// TestPasswordless_AutoRegisterOff: an unknown address gets the same 202
// and a row, but no mail; even its right code is InvalidCode. Accounts
// that exist sign in as before.
func TestPasswordless_AutoRegisterOff(t *testing.T) {
	off := false
	idn, sender := pwIdentity(t, Config{Passwordless: PasswordlessConfig{AutoRegister: &off}})
	c := serve(t, idn)
	if code := c.PostJSON("/auth/passwordless/start", map[string]string{"email": "nobody@test.com"}, nil); code != http.StatusAccepted {
		t.Fatalf("start = %d", code)
	}
	if sender.wait(200 * time.Millisecond) {
		t.Fatal("a mail to an address that cannot sign in")
	}
	if _, ok := loginCodeOf(t, idn, "nobody@test.com"); !ok {
		t.Fatal("the row is written all the same")
	}
	// Even a right code: store one we know.
	if err := idn.d.Unsafe(t.Context()).Exec("DELETE FROM auth_login_codes").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := idn.saveLoginCode(t.Context(), "nobody@test.com", "123456", "known-token", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if code, _, e := verify(c, map[string]string{"email": "nobody@test.com", "code": "123456"}); code != http.StatusUnauthorized || e.Reason != "InvalidCode" || e.Metadata["attempts_left"] != float64(4) {
		t.Fatalf("the right code of an unknown address = %d %+v", code, e)
	}
	if code, _, e := verify(c, map[string]string{"token": "known-token"}); code != http.StatusUnauthorized || e.Reason != "InvalidCode" {
		t.Fatalf("its token = %d %+v", code, e)
	}
	var n int64
	idn.d.Unsafe(t.Context()).Model(&User{}).Where("email = ?", "nobody@test.com").Count(&n)
	if n != 0 {
		t.Fatal("an account was made")
	}
	register(t, c, "alice@test.com", "password123")
	m := startCode(t, c, sender, "alice@test.com")
	if code, _, e := verify(c, map[string]string{"email": "alice@test.com", "code": m.Code}); code != http.StatusOK {
		t.Fatalf("an existing account = %d %+v", code, e)
	}
}

// TestPasswordless_VerifyIsThrottled: failed verifies count in the login
// throttle by address and by client, with failed logins.
func TestPasswordless_VerifyIsThrottled(t *testing.T) {
	idn, sender := pwIdentity(t, Config{LoginRateWindow: time.Minute, LoginRateLimit: 2})
	c := serve(t, idn)
	m := startCode(t, c, sender, "alice@test.com")
	wrong := "000000"
	if m.Code == wrong {
		wrong = "111111"
	}
	for range 2 {
		if code, _, e := verify(c, map[string]string{"email": "alice@test.com", "code": wrong}); e.Reason != "InvalidCode" {
			t.Fatalf("a wrong code = %d %+v", code, e)
		}
	}
	if code, _, e := verify(c, map[string]string{"email": "alice@test.com", "code": m.Code}); code != http.StatusTooManyRequests || e.Metadata["retry_after"] != float64(60) {
		t.Fatalf("over the limit = %d %+v", code, e)
	}
	if code, _ := login(t, c, "someone@test.com", "password123"); code != http.StatusTooManyRequests {
		t.Fatalf("a login from the same client = %d", code)
	}
}

// TestPasswordless_UsedOnceUnderRace: of concurrent verifies with one
// right code, exactly one signs in.
func TestPasswordless_UsedOnceUnderRace(t *testing.T) {
	idn, sender := pwIdentity(t, Config{Passwordless: PasswordlessConfig{MaxAttempts: 10}})
	c := serve(t, idn)
	m := startCode(t, c, sender, "alice@test.com")
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for range 8 {
		wg.Go(func() {
			code, _, _ := verify(c, map[string]string{"email": "alice@test.com", "code": m.Code})
			mu.Lock()
			codes[code]++
			mu.Unlock()
		})
	}
	wg.Wait()
	if codes[http.StatusOK] != 1 || codes[http.StatusUnauthorized] != 7 {
		t.Fatalf("statuses %v", codes)
	}
}

// TestPasswordless_Validation: verify needs email and code, or a token;
// start needs an address.
func TestPasswordless_Validation(t *testing.T) {
	idn, _ := pwIdentity(t, Config{})
	c := serve(t, idn)
	for body, fields := range map[*map[string]string][]string{
		{}:                          {"email", "code"},
		{"email": "alice@test.com"}: {"code"},
		{"code": "123456"}:          {"email"},
	} {
		code, _, e := verify(c, *body)
		if code != http.StatusBadRequest || e.Reason != "Validation" {
			t.Errorf("%v = %d %+v", *body, code, e)
		}
		for _, f := range fields {
			if !hasKey(e.Metadata["fields"], f) {
				t.Errorf("%v: fields lack %s: %+v", *body, f, e)
			}
		}
	}
	var e envelope
	if code := c.PostJSON("/auth/passwordless/start", map[string]string{"email": "not-an-address"}, &e); code != http.StatusBadRequest || !hasKey(e.Metadata["fields"], "email") {
		t.Fatalf("start with a bad address = %d %+v", code, e)
	}
}

// TestPasswordless_NeedsASenderAndEnabled: enabled without a Sender fails
// New; disabled, the routes are not mounted.
func TestPasswordless_NeedsASenderAndEnabled(t *testing.T) {
	cfg := Config{SigningKey: testKey, Passwordless: PasswordlessConfig{Enabled: true}}
	if _, err := New(cfg, newIdentity(t, Config{}).d); err == nil || !strings.Contains(err.Error(), "needs a Sender") {
		t.Fatalf("New without a Sender: %v", err)
	}
	c := serve(t, newIdentity(t, Config{}, Sender(newMockSender().send)))
	for _, path := range []string{"/auth/passwordless/start", "/auth/passwordless/verify"} {
		if code := c.PostJSON(path, map[string]string{"email": "a@test.com"}, nil); code != http.StatusNotFound {
			t.Fatalf("%s without passwordless = %d", path, code)
		}
	}
}

// TestPasswordless_MailFailureAndClose: a failed mail is a warning, the
// answer the same 202; Close waits for a mail in flight.
func TestPasswordless_MailFailureAndClose(t *testing.T) {
	sender := newMockSender()
	sender.err = errors.New("relay down")
	idn := newIdentity(t, Config{Passwordless: PasswordlessConfig{Enabled: true}}, Sender(sender.send))
	h, logs := loggedApp(t, idn)
	if rec := serveJSON(t, h, http.MethodPost, "/auth/passwordless/start", map[string]string{"email": "alice@test.com"}); rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("start = %d %q", rec.Code, rec.Body)
	}
	eventually(t, "the warning", func() bool { return strings.Contains(logs.String(), "identity: mail not sent") })
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "kind=login-code") || !strings.Contains(out, `error="relay down"`) {
		t.Fatalf("logs:\n%s", out)
	}

	blocking := newBlockingSender()
	idn = newIdentity(t, Config{Passwordless: PasswordlessConfig{Enabled: true}}, Sender(blocking.send))
	h, _ = loggedApp(t, idn)
	if rec := serveJSON(t, h, http.MethodPost, "/auth/passwordless/start", map[string]string{"email": "alice@test.com"}); rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d", rec.Code)
	}
	if m := <-blocking.started; m.Kind != "login-code" {
		t.Fatalf("mail %+v", m)
	}
	closed := make(chan error, 1)
	go func() { closed <- idn.Close(context.WithoutCancel(t.Context())) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned with a mail in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(blocking.release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

// TestPasswordless_Sweep: the sweep deletes the expired codes and keeps
// the others; Close stops it.
func TestPasswordless_Sweep(t *testing.T) {
	idn, _ := pwIdentity(t, Config{})
	now := time.Now().UTC()
	for i, email := range []string{"old@test.com", "older@test.com", "fresh@test.com"} {
		if _, err := idn.saveLoginCode(t.Context(), email, "123456", "token-"+email, now.Add(-time.Duration(2-i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := idn.d.Unsafe(t.Context()).Model(&loginCode{}).Where("email <> ?", "fresh@test.com").Update("expires_at", now.Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	n, err := idn.sweepLoginCodes(t.Context())
	if err != nil || n != 2 {
		t.Fatalf("swept %d: %v", n, err)
	}
	if _, ok := loginCodeOf(t, idn, "fresh@test.com"); !ok {
		t.Fatal("a live code was swept")
	}
	if idn.stopSweep == nil {
		t.Fatal("no sweep running")
	}
	if err := idn.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}
