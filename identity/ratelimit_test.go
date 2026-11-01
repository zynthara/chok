package identity

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLimiter_CloseWaitsForCleanup: Close returns only once a background
// sweep has finished, and no sweep starts after it.
func TestLimiter_CloseWaitsForCleanup(t *testing.T) {
	l := newLoginLimiter(time.Hour, 5)
	for range 100 { // the hundredth record starts a sweep
		l.record(limiterKey{Name: "email", Value: "victim@example.com"})
	}
	done := make(chan struct{})
	go func() { l.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within 2s")
	}
	for range 200 {
		l.record(limiterKey{Name: "email", Value: "post-close@example.com"})
	}
	l.Close() // returns at once: nothing was started
}

func TestLimiter_KeysByDimension(t *testing.T) {
	l := newLoginLimiter(time.Hour, 2)
	t.Cleanup(l.Close)
	email := limiterKey{Name: "email", Value: "1.2.3.4"}
	ip := limiterKey{Name: "client_ip", Value: "1.2.3.4"}
	l.record(email)
	l.record(email)
	if ok, by := l.check(email); ok || by != "email" {
		t.Fatalf("the email's bucket is full: %v %q", ok, by)
	}
	if ok, _ := l.check(ip); !ok {
		t.Fatal("an address equal to an email's text has its own bucket")
	}
	if ok, _ := l.check(limiterKey{Name: "client_ip", Value: ""}); !ok {
		t.Fatal("an empty value is never limited")
	}
}

func TestLogin_Throttled(t *testing.T) {
	c := serve(t, newIdentity(t, Config{LoginRateWindow: 15 * time.Minute, LoginRateLimit: 3}))
	register(t, c, "victim@test.com", "password123")
	for i := range 3 {
		if code, _ := login(t, c, "victim@test.com", "wrong-"+strconv.Itoa(i)); code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d", i, code)
		}
	}
	// The fourth try answers 429 even with the right password, whatever
	// X-Forwarded-For claims: without trusted proxies the header is the
	// caller's own input, and the throttle keys on the direct peer.
	for i := range 3 {
		resp := c.Do(jsonRequest(t, http.MethodPost, "/auth/login", map[string]string{"email": "victim@test.com", "password": "password123"},
			"X-Forwarded-For", "203.0.113."+strconv.Itoa(i+10)))
		var e envelope
		decodeBody(t, resp, &e)
		if resp.StatusCode != http.StatusTooManyRequests || e.Reason != "RateLimited" || resp.Header.Get("Retry-After") != "900" {
			t.Fatalf("attempt with a forged address = %d %+v Retry-After %q", resp.StatusCode, e, resp.Header.Get("Retry-After"))
		}
	}
	// Another email from the same address is throttled too: the address
	// bucket filled up with the failures.
	if code, _ := login(t, c, "someone@test.com", "password123"); code != http.StatusTooManyRequests {
		t.Fatalf("another email from the throttled address = %d", code)
	}
	// An unknown email counts as a failure as well.
	c2 := serve(t, newIdentity(t, Config{LoginRateWindow: time.Minute, LoginRateLimit: 1}))
	login(t, c2, "nobody@test.com", "password123")
	if code, _ := login(t, c2, "nobody@test.com", "password123"); code != http.StatusTooManyRequests {
		t.Fatalf("after an unknown-email failure = %d", code)
	}
}

// TestForgotPassword_Throttled: every forgot-password call counts, by
// address and by client, whether the address has an account or not, in
// buckets apart from the login's: past login_rate_limit within the window
// a call is a 429 with retry_after, and mails nothing.
func TestForgotPassword_Throttled(t *testing.T) {
	sender := newMockSender()
	idn := newIdentity(t, Config{LoginRateWindow: time.Minute, LoginRateLimit: 3}, Sender(sender.send))
	h, logs := loggedApp(t, idn)
	if _, err := idn.Users().Create(t.Context(), "alice@test.com", "password123", "Alice"); err != nil {
		t.Fatal(err)
	}
	post := func(addr, path string, body any) *http.Response {
		t.Helper()
		req := jsonRequest(t, http.MethodPost, path, body)
		req.RemoteAddr = addr + ":40000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result()
	}
	forgot := func(addr, email string) int {
		t.Helper()
		return post(addr, "/auth/forgot-password", map[string]string{"email": email}).StatusCode
	}
	throttled := func(what, addr, email string) {
		t.Helper()
		resp := post(addr, "/auth/forgot-password", map[string]string{"email": email})
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("%s = %d, not 429", what, resp.StatusCode)
		}
		var e envelope
		decodeBody(t, resp, &e)
		if e.Reason != "RateLimited" || e.Metadata["retry_after"] != float64(60) || resp.Header.Get("Retry-After") != "60" {
			t.Fatalf("%s: %+v Retry-After %q", what, e, resp.Header.Get("Retry-After"))
		}
	}

	// One address from fresh clients, however it is spelled: three mails,
	// then the address's bucket is full and the fourth call mails nothing.
	for i, email := range []string{"alice@test.com", "Alice@Test.com", "ALICE@test.com"} {
		if code := forgot("198.51.100."+strconv.Itoa(i+1), email); code != http.StatusNoContent {
			t.Fatalf("call %d for the address = %d", i, code)
		}
		if !sender.wait(5 * time.Second) {
			t.Fatalf("call %d for the address mailed nothing", i)
		}
	}
	throttled("the fourth call for the address", "198.51.100.4", "alice@test.com")
	if sender.wait(200 * time.Millisecond) {
		t.Fatal("a throttled call mailed")
	}
	// An address without an account counts the same: three 204s, then the
	// same 429, so the throttle tells no more about the account.
	for i := range 3 {
		if code := forgot("203.0.113."+strconv.Itoa(i+1), "nobody@test.com"); code != http.StatusNoContent {
			t.Fatalf("call %d for an unknown address = %d", i, code)
		}
	}
	throttled("the fourth call for an unknown address", "203.0.113.4", "nobody@test.com")
	// One client rotating addresses fills the client's bucket: the fourth
	// address is refused though its own bucket is empty.
	for i := range 3 {
		if code := forgot("192.0.2.9", "user"+strconv.Itoa(i)+"@test.com"); code != http.StatusNoContent {
			t.Fatalf("call %d from the client = %d", i, code)
		}
	}
	throttled("a fourth address from the client", "192.0.2.9", "fresh@test.com")
	// Another address from another client is not touched.
	if code := forgot("192.0.2.10", "fresh@test.com"); code != http.StatusNoContent {
		t.Fatalf("another address from another client = %d", code)
	}
	// The login throttle keeps its own buckets: the address and the client
	// the resets filled up still log in.
	if resp := post("192.0.2.9", "/auth/login", map[string]string{"email": "alice@test.com", "password": "password123"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("a login from the throttled client for the throttled address = %d", resp.StatusCode)
	}
	// The warnings name the dimension that was full, never its value.
	var warnings []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "identity: password resets throttled") {
			warnings = append(warnings, line)
			if strings.Contains(line, "@test.com") || strings.Contains(line, "192.0.2.9") {
				t.Fatalf("the warning shows the value: %s", line)
			}
		}
	}
	if w := strings.Join(warnings, ""); len(warnings) != 3 || strings.Count(w, "triggered_by=forgot_email") != 2 || strings.Count(w, "triggered_by=forgot_ip") != 1 {
		t.Fatalf("the warnings:\n%s", w)
	}

	// With the throttle off (Config{} sets neither), every call mails.
	off := newMockSender()
	c := serve(t, newIdentity(t, Config{}, Sender(off.send)))
	register(t, c, "bob@test.com", "password123")
	for i := range 20 {
		if code := c.PostJSON("/auth/forgot-password", map[string]string{"email": "bob@test.com"}, nil); code != http.StatusNoContent {
			t.Fatalf("call %d with the throttle off = %d", i, code)
		}
	}
	eventually(t, "twenty mails with the throttle off", func() bool {
		off.mu.Lock()
		defer off.mu.Unlock()
		return len(off.mails) == 20
	})
}
