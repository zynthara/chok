package account

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// CookieCarrier is the default SessionCarrier implementation: it stores
// the OAuth sid in an HttpOnly cookie signed with HMAC-SHA256 to detect
// tampering. The signing material is per-Module (HKDF-derived from
// account.SigningKey) so cookies issued by different deployments never
// collide.
//
// Cookie attributes default to production-grade (Secure + SameSite=None)
// because:
//   - Apple Sign-In completes via cross-site form_post — SameSite=Lax
//     drops the cookie on the response and breaks the callback. The
//     state parameter remains the primary CSRF defence; SameSite is
//     belt-and-braces.
//   - HTTPS-only deployments are the supported configuration; pair with
//     WithDevMode for local HTTP development.
type CookieCarrier struct {
	secret     []byte
	cookieName string
	cfg        cookieConfig
}

// cookieConfig is the internal config the CookieOption setters mutate.
type cookieConfig struct {
	devMode bool // SameSite=Lax + Secure=false for HTTP localhost dev
	maxAge  int  // seconds; default 300 (5 minutes)
}

// CookieOption tunes CookieCarrier behaviour. The only concrete option
// today is WithDevMode; future knobs (custom max-age, domain) are
// future-proofed via this functional-options pattern.
type CookieOption func(*cookieConfig)

// WithDevMode swaps cookie attributes to SameSite=Lax + Secure=false so
// the carrier works against an HTTP localhost back-end. Browsers refuse
// to send Secure cookies over plaintext, so production cookie defaults
// would silently drop in dev.
//
// MUST NOT be enabled in production: SameSite=Lax disables cross-site
// form_post (breaking Apple Sign-In) and lowers the CSRF bar.
//
// Module auto-detects the localhost HTTP case from the first registered
// provider's RedirectURL and applies WithDevMode automatically; callers
// only need this when constructing a custom CookieCarrier via
// WithSessionCarrier.
func WithDevMode() CookieOption {
	return func(c *cookieConfig) { c.devMode = true }
}

// WithCookieMaxAge overrides the default 5-minute cookie lifetime. The
// session itself lives in OAuthSessionStore with its own TTL — this is
// only the browser-side hint to drop the sid after the IdP roundtrip
// window.
func WithCookieMaxAge(d time.Duration) CookieOption {
	return func(c *cookieConfig) {
		c.maxAge = int(d.Seconds())
	}
}

// NewCookieCarrier builds a CookieCarrier with the given HMAC secret
// (32 bytes recommended) and cookie name. Module's default builder
// derives the secret from account.SigningKey via HKDF and uses
// "_chok_oauth_sid" as the name.
//
// Stateless — there is no Close to call, but the type's nil method set
// is fine to leave as-is (Module.Close runs an io.Closer type assertion
// and a missing Close is a no-op).
func NewCookieCarrier(secret []byte, cookieName string, opts ...CookieOption) *CookieCarrier {
	cfg := cookieConfig{maxAge: 300}
	for _, o := range opts {
		o(&cfg)
	}
	return &CookieCarrier{
		secret:     secret,
		cookieName: cookieName,
		cfg:        cfg,
	}
}

// Issue writes the signed sid as a Set-Cookie header. Format is
// "<sid>.<base64url-hmac>" — the sid is preserved as-is so server logs
// reading the cookie (without verification) still see a meaningful id.
func (c *CookieCarrier) Issue(g *gin.Context, sid string) error {
	signed := c.sign(sid)
	cookie := &http.Cookie{
		Name:     c.cookieName,
		Value:    signed,
		Path:     "/",
		MaxAge:   c.cfg.maxAge,
		HttpOnly: true,
	}
	if c.cfg.devMode {
		cookie.Secure = false
		cookie.SameSite = http.SameSiteLaxMode
	} else {
		cookie.Secure = true
		cookie.SameSite = http.SameSiteNoneMode
	}
	http.SetCookie(g.Writer, cookie)
	return nil
}

// Read pulls the cookie, verifies the HMAC, and returns sid. On any
// validation failure (cookie missing, malformed value, bad signature)
// it returns a non-nil error — Module surfaces these as 400 BadRequest
// without distinguishing the cause to avoid leaking which step failed
// to a probing attacker.
//
// On a successful read, Read also writes a delete-cookie response so
// the browser stops re-sending the sid after a successful exchange.
// This is defence-in-depth; OAuthSessionStore.Take has already
// invalidated the server-side session.
func (c *CookieCarrier) Read(g *gin.Context) (string, error) {
	cookie, err := g.Request.Cookie(c.cookieName)
	if err != nil {
		return "", errors.New("oauth sid cookie missing")
	}
	dot := strings.LastIndex(cookie.Value, ".")
	if dot <= 0 || dot == len(cookie.Value)-1 {
		return "", errors.New("oauth sid cookie malformed")
	}
	sid := cookie.Value[:dot]
	mac := cookie.Value[dot+1:]
	expected := c.sign(sid)
	expectedMac := expected[strings.LastIndex(expected, ".")+1:]
	if !hmac.Equal([]byte(mac), []byte(expectedMac)) {
		return "", errors.New("oauth sid cookie signature invalid")
	}
	c.deleteCookie(g)
	return sid, nil
}

// sign returns "<sid>.<base64url-hmac>".
func (c *CookieCarrier) sign(sid string) string {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(sid))
	tag := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return sid + "." + tag
}

func (c *CookieCarrier) deleteCookie(g *gin.Context) {
	cookie := &http.Cookie{
		Name:     c.cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	}
	if c.cfg.devMode {
		cookie.Secure = false
		cookie.SameSite = http.SameSiteLaxMode
	} else {
		cookie.Secure = true
		cookie.SameSite = http.SameSiteNoneMode
	}
	http.SetCookie(g.Writer, cookie)
}
