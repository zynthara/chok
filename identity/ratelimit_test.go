package identity

import (
	"net/http"
	"strconv"
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
