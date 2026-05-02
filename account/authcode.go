package account

import (
	"context"
	"errors"
	"time"
)

// AuthCodeData is the short-lived payload written to AuthCodeStore at
// the end of /auth/{name}/callback and consumed by /auth/exchange.
//
// The flow is intentionally split into two HTTP roundtrips so the JWT
// is never embedded in a redirect URL — see SPEC §7 "OAuth 回跳安全
// 模型". Browsers leak URL fragments through history, Referer, and
// access logs; an exchange-by-code step keeps the JWT in a JSON POST
// body where TLS protects it end-to-end.
type AuthCodeData struct {
	// UserID is the chok User RID resolved by ResolveOAuthIdentity.
	// /auth/exchange loads the User by this RID and issues a JWT.
	UserID string

	// RedirectBack is the validated post-login URL. Returned to the
	// front-end as part of the /auth/exchange response so the SPA
	// knows where to navigate after token storage.
	RedirectBack string

	// CreatedAt records when the code was issued. Stores may use this
	// for TTL bookkeeping; the recommended TTL is 5 seconds.
	CreatedAt time.Time
}

// AuthCodeStore mirrors OAuthSessionStore in shape but is intentionally
// a separate interface — different TTL (5s vs 5m), different payload,
// and conflating them would let an implementer accidentally serve an
// AuthCode read against a session sid.
//
// Take MUST be atomic-load-and-delete (same contract as
// OAuthSessionStore.Take). A non-atomic Take exposes /auth/exchange to
// replay attacks within the deletion window.
type AuthCodeStore interface {
	// Save persists data under code with the requested TTL. Module
	// passes ttl=5s.
	Save(ctx context.Context, code string, data *AuthCodeData, ttl time.Duration) error

	// Take atomically loads and deletes the entry at code. Missing or
	// already-consumed code returns ErrAuthCodeNotFound, which Module
	// maps to 410 Gone.
	Take(ctx context.Context, code string) (*AuthCodeData, error)
}

// ErrAuthCodeNotFound is the sentinel for AuthCodeStore.Take when the
// code is unknown, expired, or already consumed. Mapped to 410 Gone by
// the /auth/exchange handler.
var ErrAuthCodeNotFound = errors.New("account: oauth auth code not found, expired, or already consumed")
