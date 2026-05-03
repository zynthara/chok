package account

import (
	"context"
	"errors"
	"time"

	"github.com/gin-gonic/gin"
)

// OAuthSession is the per-flow state that bridges /auth/{name}/start and
// /auth/{name}/callback. It is created on Begin, persisted via
// OAuthSessionStore for ~5 minutes (long enough to bounce through the
// IdP), and atomically consumed once on Callback.
type OAuthSession struct {
	// State is the CSRF token round-tripped through the IdP. Module
	// generates it in Begin, the IdP echoes it back in the callback URL,
	// and Module compares before invoking CompleteAuth.
	State string

	// Nonce is the OIDC nonce, populated only when the provider declares
	// Capabilities().RequiresNonce. Empty otherwise.
	Nonce string

	// CodeVerifier is the PKCE verifier, populated only when the provider
	// declares Capabilities().SupportsPKCE.
	CodeVerifier string

	// RedirectBack is the validated post-login URL the front-end will
	// dispatch to after /auth/exchange. Empty means "default landing
	// page". Already passed Module.validateRedirectBack at Begin time.
	RedirectBack string

	// Provider is the provider Name() the session belongs to. Used as a
	// sanity check during Callback when one Module hosts multiple
	// providers — the path /auth/{name}/callback must match Provider.
	Provider string

	// LinkUserID is the chok User RID that initiated a "bind another IdP
	// to my account" flow via POST /identities/link. Empty for ordinary
	// /auth/{name}/start logins. When non-empty, handleCallback skips
	// ResolveOAuthIdentity (which would log the caller in as the IdP
	// account's owner) and goes through LinkIdentity(LinkUserID, pi)
	// instead — guaranteeing the resulting Identity row attaches to the
	// authenticated user, not whoever the IdP returned.
	LinkUserID string

	// CreatedAt records when the session was issued. Stores may use this
	// for TTL bookkeeping; the cleanup policy is store-defined.
	CreatedAt time.Time
}

// SessionCarrier transports the session id (sid) between client and
// server across the IdP redirect. Cookie is the default implementation;
// query-string / header alternatives are possible but not bundled.
//
// Implementations are expected to be stateless or hold only a signing
// secret — server-side state lives in OAuthSessionStore.
//
// If a SessionCarrier needs cleanup (long-lived signing material rotation,
// background goroutines), it can implement io.Closer; Module.Close runs
// the type assertion and chains the close. Stateless carriers (the
// default CookieCarrier) skip the assertion and pay no cost.
type SessionCarrier interface {
	// Issue is called after sessionStore.Save in Begin. It writes sid
	// onto the response (e.g. Set-Cookie) so the browser presents it
	// back during Callback. Returning an error aborts Begin with 500;
	// Module.handleBegin rolls back the just-saved session in that case.
	Issue(c *gin.Context, sid string) error

	// Read is called at the top of Callback. It pulls sid off the request
	// (e.g. Cookie header) and returns it for sessionStore.Take. Errors
	// surface as 400 (sid missing or signature invalid).
	//
	// Cookie implementations should also overwrite the cookie with an
	// immediately-expired empty value so the browser stops re-sending it
	// after a successful exchange — defence-in-depth against sid leaks.
	Read(c *gin.Context) (sid string, err error)
}

// OAuthSessionStore is the server-side sid → OAuthSession repository.
// It must guarantee atomic take-and-delete on Take so a successful
// callback cannot replay the same sid. Recommended back-ends:
//
//   - MemorySessionStore  → sync.Map.LoadAndDelete (single-instance dev).
//   - Redis 6.2+          → GETDEL (atomic).
//   - Older Redis         → EVAL Lua: GET + DEL.
//   - SQL                 → BEGIN; SELECT ... FOR UPDATE; DELETE; COMMIT.
//
// If the underlying delete fails, Take MUST return (nil, error) — never
// (sess, nil) — or an attacker can replay the sid in the deletion
// failure window to consume the OAuth code multiple times.
type OAuthSessionStore interface {
	// Save persists sess under sid with the requested TTL. The Module
	// passes ttl=5m by default; implementations are free to round but
	// must not extend silently.
	Save(ctx context.Context, sid string, sess *OAuthSession, ttl time.Duration) error

	// Take atomically loads and deletes the session at sid. On a missing,
	// expired, or already-consumed sid it returns (nil, ErrSessionNotFound)
	// — Module surfaces that as 410 Gone.
	Take(ctx context.Context, sid string) (*OAuthSession, error)
}

// ErrSessionNotFound is the sentinel returned by OAuthSessionStore.Take
// when the requested sid does not resolve to a live session — either it
// never existed, expired, or was already consumed by a prior Take. The
// Module maps this to HTTP 410 Gone.
var ErrSessionNotFound = errors.New("account: oauth session not found or already consumed")
