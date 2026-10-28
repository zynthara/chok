package identity

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/internal/openapi"
	"github.com/zynthara/chok/v3/store"
)

// maxAuthorizationLen caps the Authorization header Authn reads: a token
// with ordinary claims is about 1 KB, and parsing runs HMAC over the
// whole input, so an 8 KB cap keeps one header from costing a core.
const maxAuthorizationLen = 8 << 10

// Authn is the middleware that admits a request with a valid access
// token: a Bearer JWT this Identity signed — in cookie mode, the session
// cookie's, when the request has no Authorization header — whose user
// still exists, is active and has not had its tokens revoked since (a
// password change, a role change, a disable, Users().RevokeTokens). It
// reads the user on every request, so a revocation takes effect at once.
// The request then carries the chok.Principal — UserID is the user's id
// (usr_…), which a store's owner scope writes to owner_id — and anything
// else is a 401 envelope.
//
// The OpenAPI document lists the routes behind it with the schemes it
// accepts: bearerAuth, and in cookie mode cookieAuth.
func (idn *Identity) Authn() chok.Middleware {
	return func(next http.Handler) http.Handler { return &authnHandler{idn: idn, next: next} }
}

// authnHandler is a route behind Authn.
type authnHandler struct {
	idn  *Identity
	next http.Handler
}

func (h *authnHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := h.idn.credential(r)
	if err != nil {
		chok.WriteError(w, r, err)
		return
	}
	ctx, err := h.idn.authenticate(r.Context(), raw)
	if err != nil {
		chok.WriteError(w, r, err)
		return
	}
	h.next.ServeHTTP(w, r.WithContext(ctx))
}

// OpenAPISecurity is what the route accepts: the bearer token, and the
// session cookie in cookie mode.
func (h *authnHandler) OpenAPISecurity() []openapi.Scheme {
	if h.idn.cfg.Cookie.Enabled {
		return []openapi.Scheme{openapi.BearerAuth, openapi.CookieAuth(h.idn.cfg.Cookie.Name)}
	}
	return []openapi.Scheme{openapi.BearerAuth}
}

// credential is the request's access token: the Authorization header's
// bearer token, or — in cookie mode, and only without that header — the
// session cookie's value. The header wins even when it is not a valid
// bearer token.
func (idn *Identity) credential(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header != "" || !idn.cfg.Cookie.Enabled {
		raw := bearer(header)
		if raw == "" {
			return "", errNoToken
		}
		return raw, nil
	}
	ck, err := r.Cookie(idn.cfg.Cookie.Name)
	if err != nil || ck.Value == "" {
		return "", errNoCredential
	}
	if len(ck.Value) > maxAuthorizationLen {
		return "", errBadBearer
	}
	return ck.Value, nil
}

// session is what Authn learned from the token, kept for the handlers
// behind it: refresh-token carries the session's start forward.
type session struct {
	user  *User
	start int64 // when the session's login happened, Unix seconds
}

type sessionKey struct{}

func sessionFrom(ctx context.Context) (*session, bool) {
	s, ok := ctx.Value(sessionKey{}).(*session)
	return s, ok
}

var (
	errNoToken      = chok.ErrUnauthorized.Msgf("a bearer token is required")
	errNoCredential = chok.ErrUnauthorized.Msgf("a bearer token or the session cookie is required")
	errBadBearer    = chok.ErrUnauthorized.Msgf("the token is invalid or expired")
	errNoAccount    = chok.ErrUnauthorized.Msgf("account not found")
	errDisabled     = chok.ErrUnauthorized.Msgf("account is disabled")
	errStaleToken   = chok.ErrUnauthorized.Msgf("token invalidated, please log in again")
	errSessionEnded = chok.ErrUnauthorized.Msgf("the session ended, please log in again")
)

// authenticate checks an access token and returns ctx carrying the
// principal and the session.
func (idn *Identity) authenticate(ctx context.Context, raw string) (context.Context, error) {
	sub, claims, err := idn.access.parse(raw)
	if err != nil {
		return nil, errBadBearer
	}
	user, err := idn.users.Get(ctx, store.RID(sub))
	if err != nil {
		if errors.Is(err, chok.ErrNotFound) {
			return nil, errNoAccount
		}
		return nil, err
	}
	if !user.Active {
		return nil, errDisabled
	}
	// pv moves whenever the password, the roles or the active flag
	// change: a token from before then must not pass.
	if pv, ok := intClaim(claims, "pv"); !ok || pv != int64(user.PasswordVersion) {
		return nil, errStaleToken
	}
	start, _ := intClaim(claims, "sat")
	ctx = chok.WithPrincipal(ctx, principalOf(user))
	return context.WithValue(ctx, sessionKey{}, &session{user: user, start: start}), nil
}

// principalOf is the chok.Principal of a user: the user id, the email,
// the roles and, in Extra, the display name.
func principalOf(u *User) chok.Principal {
	return chok.Principal{UserID: u.RID, Email: u.Email, Roles: u.RoleList(), Extra: map[string]string{"name": u.Name}}
}

// bearer returns the token of an Authorization: Bearer header, or "":
// the scheme is case-insensitive, the header capped in length.
func bearer(header string) string {
	if header == "" || len(header) > maxAuthorizationLen {
		return ""
	}
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}
