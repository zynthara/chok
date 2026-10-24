package identity

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

var carrierSecret = []byte("test-secret-32-bytes-padding-ok!!")

func readCarrier(c *cookieCarrier, cookies ...*http.Cookie) (string, *httptest.ResponseRecorder, error) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	sid, err := c.read(w, req)
	return sid, w, err
}

func TestCookieCarrier_RoundTripAndClear(t *testing.T) {
	c := newCookieCarrier(carrierSecret, sessionCookie, false)
	w := httptest.NewRecorder()
	c.issue(w, "sid-abc")
	sid, rw, err := readCarrier(c, w.Result().Cookies()...)
	if err != nil || sid != "sid-abc" {
		t.Fatalf("read = %q %v", sid, err)
	}
	// Reading clears the cookie, so the browser stops sending it.
	cleared := false
	for _, ck := range rw.Result().Cookies() {
		cleared = cleared || (ck.Name == sessionCookie && ck.Value == "" && ck.MaxAge < 0)
	}
	if !cleared {
		t.Fatal("read must answer a delete-cookie")
	}
}

func TestCookieCarrier_RejectsForgedValues(t *testing.T) {
	c := newCookieCarrier(carrierSecret, sessionCookie, false)
	other := newCookieCarrier([]byte("another-secret-32-bytes-padding!!"), sessionCookie, false)
	w := httptest.NewRecorder()
	other.issue(w, "sid-abc")
	values := []string{"", "no-dot", ".missing-sid", "sid-only.", "sid-abc.bogus-signature", w.Result().Cookies()[0].Value}
	for _, v := range values {
		if _, _, err := readCarrier(c, &http.Cookie{Name: sessionCookie, Value: v}); err == nil {
			t.Errorf("%q passed", v)
		}
	}
	if _, _, err := readCarrier(c); err == nil {
		t.Error("no cookie passed")
	}
}

func TestCookieCarrier_Attributes(t *testing.T) {
	for _, dev := range []bool{false, true} {
		w := httptest.NewRecorder()
		newCookieCarrier(carrierSecret, sessionCookie, dev).issue(w, "sid")
		ck := w.Result().Cookies()[0]
		wantSecure, wantSite := true, http.SameSiteNoneMode
		if dev {
			wantSecure, wantSite = false, http.SameSiteLaxMode
		}
		if ck.Secure != wantSecure || ck.SameSite != wantSite || !ck.HttpOnly || ck.Path != "/" || ck.MaxAge != 300 {
			t.Errorf("dev=%v: %+v", dev, ck)
		}
	}
}

func TestMemoryStore_SaveAndTake(t *testing.T) {
	s := newMemoryStore(10, 0)
	defer s.close()
	s.saveSession("sid", &oauthSession{State: "st"}, time.Minute)
	if got, err := s.takeSession("sid"); err != nil || got.State != "st" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.takeSession("sid"); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("a second take: %v", err)
	}
	s.saveCode("code1", &authCode{UserID: "u1", RedirectBack: "/"}, time.Minute)
	if got, err := s.takeCode("code1"); err != nil || got.UserID != "u1" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.takeCode("code1"); !errors.Is(err, errCodeNotFound) {
		t.Fatalf("a second code take: %v", err)
	}
}

func TestMemoryStore_Expiry(t *testing.T) {
	s := newMemoryStore(10, 0)
	defer s.close()
	now := time.Now()
	s.now = func() time.Time { return now }
	s.saveSession("sid", &oauthSession{}, time.Minute)
	s.saveSession("live", &oauthSession{}, time.Hour)
	now = now.Add(2 * time.Minute)
	if _, err := s.takeSession("sid"); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("an expired session: %v", err)
	}
	s.saveSession("stale", &oauthSession{}, time.Minute)
	now = now.Add(2 * time.Minute)
	s.sweep()
	if len(s.entries) != 1 {
		t.Fatalf("the sweep keeps only the live entry: %d", len(s.entries))
	}
}

func TestMemoryStore_LRU(t *testing.T) {
	s := newMemoryStore(3, 0)
	defer s.close()
	for i := range 4 {
		s.saveSession("sid"+strconv.Itoa(i), &oauthSession{State: strconv.Itoa(i)}, time.Minute)
	}
	if _, err := s.takeSession("sid0"); !errors.Is(err, errSessionNotFound) {
		t.Fatal("the least recently used is evicted")
	}
	for _, id := range []string{"sid1", "sid2", "sid3"} {
		if _, err := s.takeSession(id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	// Saving a key again replaces its value and makes it the most recent.
	r := newMemoryStore(2, 0)
	defer r.close()
	r.saveSession("a", &oauthSession{State: "a1"}, time.Minute)
	r.saveSession("b", &oauthSession{State: "b1"}, time.Minute)
	r.saveSession("a", &oauthSession{State: "a2"}, time.Minute)
	r.saveSession("c", &oauthSession{State: "c1"}, time.Minute)
	if _, err := r.takeSession("b"); !errors.Is(err, errSessionNotFound) {
		t.Fatal("b was the least recent")
	}
	if got, err := r.takeSession("a"); err != nil || got.State != "a2" {
		t.Fatalf("a: %+v %v", got, err)
	}
}

func TestMemoryStore_TakeIsAtomic(t *testing.T) {
	s := newMemoryStore(10, 0)
	defer s.close()
	s.saveSession("sid", &oauthSession{State: "race"}, time.Minute)
	var winners sync.WaitGroup
	won := make(chan struct{}, 50)
	for range 50 {
		winners.Go(func() {
			if _, err := s.takeSession("sid"); err == nil {
				won <- struct{}{}
			}
		})
	}
	winners.Wait()
	if len(won) != 1 {
		t.Fatalf("%d takers won", len(won))
	}
}

func TestMemoryStore_BucketsAreSeparate(t *testing.T) {
	s := newMemoryStore(10, 0)
	defer s.close()
	s.saveSession("x", &oauthSession{State: "session"}, time.Minute)
	s.saveCode("x", &authCode{UserID: "u"}, time.Minute)
	if sess, err := s.takeSession("x"); err != nil || sess.State != "session" {
		t.Fatalf("session: %+v %v", sess, err)
	}
	if code, err := s.takeCode("x"); err != nil || code.UserID != "u" {
		t.Fatalf("code: %+v %v", code, err)
	}
}

func TestMemoryStore_CloseStopsTheSweep(t *testing.T) {
	s := newMemoryStore(10, 10*time.Millisecond)
	s.close()
	s.close() // idempotent
	select {
	case <-s.stop:
	default:
		t.Fatal("close stops the sweep")
	}
}
