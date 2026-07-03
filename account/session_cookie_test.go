package account

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCookieCarrier_RoundTrip(t *testing.T) {
	c := NewCookieCarrier([]byte("test-secret-32-bytes-padding-ok!!"), "_chok_oauth_sid")

	w := httptest.NewRecorder()
	if err := c.Issue(w, httptest.NewRequest("GET", "/", nil), "sid-abc"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	setCookie := w.Header().Get("Set-Cookie")
	if !strings.HasPrefix(setCookie, "_chok_oauth_sid=") {
		t.Fatalf("expected Set-Cookie header, got %q", setCookie)
	}

	// Forward the cookie back to a fresh request, verify Read recovers sid.
	req := httptest.NewRequest("GET", "/", nil)
	for _, cv := range w.Result().Cookies() {
		req.AddCookie(cv)
	}
	got, err := c.Read(httptest.NewRecorder(), req)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "sid-abc" {
		t.Fatalf("Read sid = %q, want %q", got, "sid-abc")
	}
}

func TestCookieCarrier_RejectsTamperedSig(t *testing.T) {
	c := NewCookieCarrier([]byte("test-secret-32-bytes-padding-ok!!"), "_chok_oauth_sid")
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "_chok_oauth_sid", Value: "sid-abc.bogus-signature"})
	_, err := c.Read(httptest.NewRecorder(), req)
	if err == nil {
		t.Fatal("expected error for tampered signature")
	}
}

func TestCookieCarrier_RejectsMalformedValue(t *testing.T) {
	c := NewCookieCarrier([]byte("test-secret-32-bytes-padding-ok!!"), "_chok_oauth_sid")
	cases := []string{"", "no-dot", ".missing-sid", "sid-only."}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.AddCookie(&http.Cookie{Name: "_chok_oauth_sid", Value: v})
			if _, err := c.Read(httptest.NewRecorder(), req); err == nil {
				t.Fatalf("expected error for malformed value %q", v)
			}
		})
	}
}

func TestCookieCarrier_DevMode_SameSiteLax(t *testing.T) {
	c := NewCookieCarrier([]byte("test-secret-32-bytes-padding-ok!!"), "_chok_oauth_sid", WithDevMode())
	w := httptest.NewRecorder()
	if err := c.Issue(w, httptest.NewRequest("GET", "/", nil), "sid-x"); err != nil {
		t.Fatal(err)
	}
	setCookie := w.Header().Get("Set-Cookie")
	if strings.Contains(setCookie, "Secure") {
		t.Fatalf("dev mode cookie must not be Secure: %s", setCookie)
	}
	if !strings.Contains(setCookie, "SameSite=Lax") {
		t.Fatalf("dev mode cookie must be SameSite=Lax: %s", setCookie)
	}
}

func TestCookieCarrier_Production_SameSiteNoneSecure(t *testing.T) {
	c := NewCookieCarrier([]byte("test-secret-32-bytes-padding-ok!!"), "_chok_oauth_sid")
	w := httptest.NewRecorder()
	if err := c.Issue(w, httptest.NewRequest("GET", "/", nil), "sid-x"); err != nil {
		t.Fatal(err)
	}
	setCookie := w.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "Secure") {
		t.Fatalf("production cookie must be Secure: %s", setCookie)
	}
	if !strings.Contains(setCookie, "SameSite=None") {
		t.Fatalf("production cookie must be SameSite=None: %s", setCookie)
	}
}

func TestCookieCarrier_Read_ClearsCookie(t *testing.T) {
	c := NewCookieCarrier([]byte("test-secret-32-bytes-padding-ok!!"), "_chok_oauth_sid")

	// First Issue to get a valid signed cookie value.
	issueW := httptest.NewRecorder()
	_ = c.Issue(issueW, httptest.NewRequest("GET", "/", nil), "sid-abc")

	// Replay it on a fresh request and inspect Read's response cookies.
	req := httptest.NewRequest("GET", "/", nil)
	for _, cv := range issueW.Result().Cookies() {
		req.AddCookie(cv)
	}
	readW := httptest.NewRecorder()
	if _, err := c.Read(readW, req); err != nil {
		t.Fatal(err)
	}

	cleared := false
	for _, cv := range readW.Result().Cookies() {
		if cv.Name == "_chok_oauth_sid" && cv.Value == "" && cv.MaxAge == -1 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("Read should issue a delete-cookie response")
	}
}
