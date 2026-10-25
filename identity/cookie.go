package identity

import (
	"context"
	"net/http"
	"time"

	"github.com/zynthara/chok/v3"
)

// sessionResponse is what a route that issues a token answers with
// cookie.only: the token is in the cookie alone.
type sessionResponse struct {
	ExpiresAt time.Time `json:"expires_at"`
}

type responseKey struct{}

// keepResponse lets the typed handler behind it set cookies: it keeps the
// ResponseWriter in the request's context, where responseFrom finds it.
func keepResponse(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), responseKey{}, w)))
	})
}

func responseFrom(ctx context.Context) http.ResponseWriter {
	w, _ := ctx.Value(responseKey{}).(http.ResponseWriter)
	return w
}

// tokenRoute registers POST pattern for fn, a handler that issues a token.
// In cookie mode the token goes into the session cookie as well, and
// with cookie.only it leaves the body, which documents and carries
// {expires_at} alone.
func tokenRoute[T any](idn *Identity, r chok.Router, pattern string, fn chok.HandlerFunc[T, *tokenResponse], opts ...chok.RouteOption) {
	c := idn.cfg.Cookie
	if !c.Enabled {
		chok.POST(r, pattern, fn, opts...)
		return
	}
	opts = append(opts, chok.Use(keepResponse))
	issue := func(ctx context.Context, req *T) (*tokenResponse, error) {
		tok, err := fn(ctx, req)
		if err == nil {
			if w := responseFrom(ctx); w != nil {
				http.SetCookie(w, idn.tokenCookie(tok.Token))
			}
		}
		return tok, err
	}
	if !c.Only {
		chok.POST(r, pattern, issue, opts...)
		return
	}
	chok.POST(r, pattern, func(ctx context.Context, req *T) (*sessionResponse, error) {
		tok, err := issue(ctx, req)
		if err != nil {
			return nil, err
		}
		return &sessionResponse{ExpiresAt: tok.ExpiresAt}, nil
	}, opts...)
}

// tokenCookie is the session cookie: it carries token for as long as a
// token lives, HttpOnly, with the configured name, path, domain, Secure
// and SameSite.
func (idn *Identity) tokenCookie(token string) *http.Cookie {
	c := idn.newCookie(token)
	c.MaxAge = max(int(idn.cfg.TokenTTL/time.Second), 1)
	return c
}

// expiredCookie clears the session cookie: the same attributes, expired.
func (idn *Identity) expiredCookie() *http.Cookie {
	c := idn.newCookie("")
	c.MaxAge = -1
	return c
}

func (idn *Identity) newCookie(value string) *http.Cookie {
	c := idn.cfg.Cookie
	sameSite := http.SameSiteLaxMode
	switch c.SameSite {
	case "strict":
		sameSite = http.SameSiteStrictMode
	case "none":
		sameSite = http.SameSiteNoneMode
	}
	return &http.Cookie{Name: c.Name, Value: value, Path: c.Path, Domain: c.Domain, Secure: c.secure(), HttpOnly: true, SameSite: sameSite}
}

// logout clears the session cookie and answers 204. The token stays valid
// until it expires — identity keeps no sessions; Users().RevokeTokens ends
// every token of a user at once.
func (idn *Identity) logout(ctx context.Context, _ *chok.None) (chok.None, error) {
	if w := responseFrom(ctx); w != nil {
		http.SetCookie(w, idn.expiredCookie())
	}
	return chok.None{}, nil
}
