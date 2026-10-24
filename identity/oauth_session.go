package identity

import (
	"container/list"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// oauthSession bridges {prefix}/{name}/start and the provider's callback:
// saved at the start for oauthSessionTTL, taken — read and deleted at
// once — by the callback.
type oauthSession struct {
	// State is the CSRF token the provider echoes back; the callback
	// compares it before anything else.
	State string
	// Nonce is the OpenID Connect nonce, for providers that require one.
	Nonce string
	// CodeVerifier is the PKCE secret, for providers that support it.
	CodeVerifier string
	// RedirectBack is where the front end goes after the exchange,
	// checked at the start.
	RedirectBack string
	// Provider is the provider the session belongs to: a callback for
	// another provider fails.
	Provider string
	// LinkUserID, set by {prefix}/identities/link, makes the callback link
	// the provider's account to that user instead of signing in.
	LinkUserID string
	CreatedAt  time.Time
}

// authCode is what the callback leaves for {prefix}/exchange: the user it
// signed in, where the front end goes next, and the hash of the binding
// cookie only the signing-in browser holds.
type authCode struct {
	UserID       string
	RedirectBack string
	BindingHash  string // hex SHA-256 of the binding cookie's value
	CreatedAt    time.Time
}

// The lifetimes of the flow's state: a session lasts the round trip
// through the provider, a code the front end's call to exchange.
const (
	oauthSessionTTL = 5 * time.Minute
	authCodeTTL     = 5 * time.Second
)

var (
	errSessionNotFound = errors.New("identity: oauth session not found or already used")
	errCodeNotFound    = errors.New("identity: oauth code not found, expired or already used")
)

// cookieCarrier carries the session id across the provider's redirect in
// an HttpOnly cookie, signed with HMAC-SHA256 so it cannot be forged; the
// session itself stays on the server.
//
// The cookie is Secure and SameSite=None: Apple posts its callback from
// another site, which a Lax cookie would not survive, and the state
// parameter is the CSRF defence anyway. In development mode — the
// provider's callback is http on localhost — it is Lax and not Secure,
// since browsers drop Secure cookies over plain http.
type cookieCarrier struct {
	secret  []byte
	name    string
	devMode bool
	maxAge  int // seconds
}

func newCookieCarrier(secret []byte, name string, devMode bool) *cookieCarrier {
	return &cookieCarrier{secret: secret, name: name, devMode: devMode, maxAge: int(oauthSessionTTL / time.Second)}
}

// issue sets the signed cookie: "<sid>.<base64url HMAC>".
func (c *cookieCarrier) issue(w http.ResponseWriter, sid string) {
	http.SetCookie(w, c.cookie(c.sign(sid), c.maxAge))
}

// read returns the session id of a validly signed cookie and clears the
// cookie, so the browser stops sending it. A missing, malformed or forged
// cookie is an error that does not say which.
func (c *cookieCarrier) read(w http.ResponseWriter, r *http.Request) (string, error) {
	ck, err := r.Cookie(c.name)
	if err != nil {
		return "", errors.New("identity: oauth session cookie missing")
	}
	dot := strings.LastIndex(ck.Value, ".")
	if dot <= 0 || dot == len(ck.Value)-1 {
		return "", errors.New("identity: oauth session cookie malformed")
	}
	sid, mac := ck.Value[:dot], ck.Value[dot+1:]
	want := c.sign(sid)
	if !hmac.Equal([]byte(mac), []byte(want[strings.LastIndex(want, ".")+1:])) {
		return "", errors.New("identity: oauth session cookie forged")
	}
	http.SetCookie(w, c.cookie("", -1))
	return sid, nil
}

func (c *cookieCarrier) sign(sid string) string {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(sid))
	return sid + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (c *cookieCarrier) cookie(value string, maxAge int) *http.Cookie {
	return flowCookie(c.name, value, maxAge, c.devMode)
}

// flowCookie is an HttpOnly cookie on /, Secure and SameSite=None unless
// in development mode, where it is Lax over plain http.
func flowCookie(name, value string, maxAge int, devMode bool) *http.Cookie {
	ck := &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteNoneMode}
	if devMode {
		ck.Secure, ck.SameSite = false, http.SameSiteLaxMode
	}
	return ck
}

// memoryStore keeps the flow's sessions and codes in this process: an LRU
// map whose entries expire, swept every cleanup period. Sessions and codes
// live under their own key prefixes. Every replica of an application has
// its own, so a sign-in's start, callback and exchange must reach the
// same process.
type memoryStore struct {
	mu       sync.Mutex
	entries  map[string]*list.Element // key → element in order
	order    *list.List               // front is the most recently used
	capacity int

	closeOnce sync.Once
	stop      chan struct{}
	now       func() time.Time
}

type memoryEntry struct {
	key       string
	value     any // *oauthSession or *authCode
	expiresAt time.Time
}

// newMemoryStore holds up to capacity live entries, evicting the least
// recently used: a flood of starts cannot push out a sign-in that is
// mid-flight unless it outlasts them all. A positive cleanup period runs
// a sweep of expired entries until close.
func newMemoryStore(capacity int, cleanup time.Duration) *memoryStore {
	s := &memoryStore{entries: map[string]*list.Element{}, order: list.New(), capacity: max(capacity, 1), stop: make(chan struct{}), now: time.Now}
	if cleanup > 0 {
		go s.sweepEvery(cleanup)
	}
	return s
}

func (s *memoryStore) saveSession(sid string, sess *oauthSession, ttl time.Duration) {
	s.put("session:"+sid, sess, ttl)
}

// takeSession loads and deletes the session at once: a callback cannot
// be replayed.
func (s *memoryStore) takeSession(sid string) (*oauthSession, error) {
	if v, ok := s.take("session:" + sid); ok {
		if sess, ok := v.(*oauthSession); ok {
			return sess, nil
		}
	}
	return nil, errSessionNotFound
}

func (s *memoryStore) saveCode(code string, data *authCode, ttl time.Duration) {
	s.put("code:"+code, data, ttl)
}

// takeCode loads and deletes the code at once: an exchange cannot be
// replayed.
func (s *memoryStore) takeCode(code string) (*authCode, error) {
	if v, ok := s.take("code:" + code); ok {
		if data, ok := v.(*authCode); ok {
			return data, nil
		}
	}
	return nil, errCodeNotFound
}

// close stops the sweep; it can be called more than once.
func (s *memoryStore) close() { s.closeOnce.Do(func() { close(s.stop) }) }

func (s *memoryStore) put(key string, value any, ttl time.Duration) {
	expiresAt := s.now().Add(ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.entries[key]; ok {
		e := el.Value.(*memoryEntry)
		e.value, e.expiresAt = value, expiresAt
		s.order.MoveToFront(el)
		return
	}
	if len(s.entries) >= s.capacity {
		if oldest := s.order.Back(); oldest != nil {
			s.remove(oldest)
		}
	}
	s.entries[key] = s.order.PushFront(&memoryEntry{key: key, value: value, expiresAt: expiresAt})
}

func (s *memoryStore) take(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*memoryEntry)
	s.remove(el)
	if s.now().After(e.expiresAt) {
		return nil, false
	}
	return e.value, true
}

func (s *memoryStore) remove(el *list.Element) {
	delete(s.entries, el.Value.(*memoryEntry).key)
	s.order.Remove(el)
}

func (s *memoryStore) sweepEvery(period time.Duration) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.sweep()
		case <-s.stop:
			return
		}
	}
}

// sweep drops the expired entries.
func (s *memoryStore) sweep() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for el := s.order.Back(); el != nil; {
		prev := el.Prev()
		if el.Value.(*memoryEntry).expiresAt.Before(now) {
			s.remove(el)
		}
		el = prev
	}
}
