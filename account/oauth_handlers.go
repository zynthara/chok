package account

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/handler"
	"github.com/zynthara/chok/store"
)

// exchangeBindingCookieName is the HttpOnly cookie that binds an
// outstanding /auth/exchange call to the browser that initiated the
// OAuth flow. handleCallback writes a random 16-byte token here just
// before the 302 to the front-end; /auth/exchange reads it and verifies
// its SHA-256 against AuthCodeData.BindingHash.
//
// Without this binding the auth_code is a pure bearer token: anyone who
// scrapes it from the redirect URL (browser history / Referer / front-
// end JS / proxy logs) within the 5-second TTL could exchange it for a
// JWT. The cookie is the second factor that makes the exchange browser-
// scoped.
const exchangeBindingCookieName = "_chok_oauth_xchg"

// exchangeBindingMaxAge is how long the browser keeps the binding cookie.
// Long enough for a normal SPA to load and call /auth/exchange after the
// 302 (5s TTL on the auth_code itself is the actual hard cap), short
// enough to bound the post-flow window during which a stale cookie is
// still useful to an attacker.
const exchangeBindingMaxAge = 60

// handleBegin returns the gin handler for GET /{name}/start.
//
// Lifecycle: validate redirect_back → mint sid + state/nonce/PKCE →
// persist OAuthSession → issue sid carrier → invoke provider.BeginAuth →
// 302 redirect to IdP authorize URL. Any failure rolls back the session
// store entry so an attacker cannot exhaust capacity by triggering Issue
// errors.
func (m *Module) handleBegin(p AuthProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		redirectBack := c.Query("redirect_back")
		if err := m.validateRedirectBack(redirectBack); err != nil {
			handler.WriteResponse(c, 0, nil,
				apierr.ErrInvalidArgument.WithMessage("redirect_back not allowed: "+err.Error()))
			return
		}
		redirectTo, err := m.startOAuthFlow(c, p, redirectBack, "")
		if err != nil {
			handler.WriteResponse(c, 0, nil, apierr.FromError(err))
			return
		}
		c.Redirect(http.StatusFound, redirectTo)
	}
}

// startOAuthFlow is the shared core for /{name}/start and the link
// flow that POST /identities/link triggers. It mints sid + session,
// persists everything, issues the carrier cookie, and invokes
// provider.BeginAuth — returning the IdP authorize URL so the caller
// decides whether to 302 (login) or wrap in JSON (link).
//
// linkUserID, when non-empty, marks the OAuthSession as a link-intent
// flow. handleCallback inspects the field and routes to LinkIdentity
// rather than ResolveOAuthIdentity, guaranteeing the resulting Identity
// row attaches to the authenticated user.
func (m *Module) startOAuthFlow(c *gin.Context, p AuthProvider, redirectBack, linkUserID string) (string, error) {
	ctx := c.Request.Context()
	caps := p.Capabilities()
	sess := &OAuthSession{
		State:        randomID(),
		Provider:     p.Name(),
		RedirectBack: redirectBack,
		LinkUserID:   linkUserID,
		CreatedAt:    time.Now(),
	}
	if caps.RequiresNonce {
		sess.Nonce = randomID()
	}
	if caps.SupportsPKCE {
		sess.CodeVerifier = randomID()
	}

	sid := randomID()
	if err := m.sessionStore.Save(ctx, sid, sess, 5*time.Minute); err != nil {
		return "", apierr.ErrInternal.Wrap(err)
	}
	if err := m.sessionCarrier.Issue(c, sid); err != nil {
		_, _ = m.sessionStore.Take(context.WithoutCancel(ctx), sid)
		return "", apierr.ErrInternal.Wrap(err)
	}

	resp, err := p.BeginAuth(ctx, &BeginRequest{
		State:         sess.State,
		Nonce:         sess.Nonce,
		CodeChallenge: pkceChallenge(sess.CodeVerifier),
		RedirectBack:  sess.RedirectBack,
	})
	if err != nil {
		_, _ = m.sessionStore.Take(context.WithoutCancel(ctx), sid)
		return "", apierr.ErrInternal.Wrap(err)
	}
	return resp.RedirectTo, nil
}

// handleCallback returns the gin handler for GET or POST
// /{name}/callback.
//
// Reads sid from carrier → atomic Take from sessionStore → state check →
// extract code/state from query (GET) or form (POST/form_post) →
// provider.CompleteAuth → if session is a link-intent (LinkUserID != "")
// then LinkIdentity(LinkUserID, pi) and 302 to redirect_back?status=linked;
// otherwise ResolveOAuthIdentity → write one-shot AuthCode + browser-
// binding cookie → 302 to oauthCallbackFrontendURL?code=…
func (m *Module) handleCallback(p AuthProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		caps := p.Capabilities()

		sid, err := m.sessionCarrier.Read(c)
		if err != nil {
			handler.WriteResponse(c, 0, nil,
				apierr.ErrInvalidArgument.WithMessage("oauth session id missing or invalid"))
			return
		}
		sess, err := m.sessionStore.Take(ctx, sid)
		if err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				handler.WriteResponse(c, 0, nil, apierr.ErrGone.WithMessage("oauth session expired"))
				return
			}
			handler.WriteResponse(c, 0, nil, apierr.ErrInternal.Wrap(err))
			return
		}
		if sess.Provider != p.Name() {
			// sid bound to a different provider — likely tampering or
			// stale URL. Log server-side for ops; respond with the same
			// 410 as a normal expiry so we don't leak which provider
			// owned the sid.
			if m.logger != nil {
				m.logger.Warn("oauth callback provider mismatch",
					"expected", sess.Provider, "got", p.Name())
			}
			handler.WriteResponse(c, 0, nil, apierr.ErrGone.WithMessage("oauth session expired"))
			return
		}

		var formBody url.Values
		if caps.RequiresFormPost {
			if err := c.Request.ParseForm(); err != nil {
				handler.WriteResponse(c, 0, nil,
					apierr.ErrInvalidArgument.WithMessage("invalid form body"))
				return
			}
			formBody = c.Request.PostForm
		}
		getParam := func(key string) string {
			if caps.RequiresFormPost {
				return formBody.Get(key)
			}
			return c.Query(key)
		}

		gotState := getParam("state")
		if gotState != sess.State {
			handler.WriteResponse(c, 0, nil,
				apierr.ErrInvalidArgument.WithMessage("oauth state mismatch"))
			return
		}

		ident, err := p.CompleteAuth(ctx, &CompleteRequest{
			Code:         getParam("code"),
			State:        gotState,
			Nonce:        sess.Nonce,
			CodeVerifier: sess.CodeVerifier,
			FormBody:     formBody,
		})
		if err != nil {
			handler.WriteResponse(c, 0, nil, apierr.FromError(err))
			return
		}
		if ident.Provider == "" {
			ident.Provider = p.Name()
		}

		// Link-intent: the user was already authenticated and asked to bind
		// this IdP account to their existing chok user. Skip the login
		// decision tree — go straight to LinkIdentity so the resulting
		// Identity row attaches to LinkUserID, not to whichever user the
		// IdP returned.
		if sess.LinkUserID != "" {
			if _, err := m.LinkIdentity(ctx, sess.LinkUserID, ident); err != nil {
				handler.WriteResponse(c, 0, nil, apierr.FromError(err))
				return
			}
			redirectURL := buildLinkSuccessRedirect(m.oauthCallbackFrontendURL, sess.RedirectBack, p.Name())
			c.Redirect(http.StatusFound, redirectURL)
			return
		}

		user, _, err := m.ResolveOAuthIdentity(ctx, ident)
		if err != nil {
			handler.WriteResponse(c, 0, nil, apierr.FromError(err))
			return
		}

		// Browser-binding for the upcoming /auth/exchange call. Mint a
		// random 16-byte token, store its SHA-256 in AuthCodeData, set the
		// pre-image as a HttpOnly cookie. /auth/exchange recomputes the
		// hash from the cookie value and rejects on mismatch — auth_code
		// is no longer a pure bearer.
		bindToken := randomID()
		bindHash := sha256.Sum256([]byte(bindToken))
		authCode := randomID()
		if err := m.authCodeStore.Save(ctx, authCode, &AuthCodeData{
			UserID:       user.RID,
			RedirectBack: sess.RedirectBack,
			BindingHash:  hex.EncodeToString(bindHash[:]),
			CreatedAt:    time.Now(),
		}, 5*time.Second); err != nil {
			handler.WriteResponse(c, 0, nil, apierr.ErrInternal.Wrap(err))
			return
		}
		m.writeExchangeBindingCookie(c, bindToken)

		sep := "?"
		if hasQuery(m.oauthCallbackFrontendURL) {
			sep = "&"
		}
		c.Redirect(http.StatusFound, m.oauthCallbackFrontendURL+sep+"code="+url.QueryEscape(authCode))
	}
}

// hasQuery returns true if u already contains a "?" in the path —
// simple enough we don't need a full URL parse.
func hasQuery(u string) bool {
	for i := 0; i < len(u); i++ {
		if u[i] == '?' {
			return true
		}
	}
	return false
}

// writeExchangeBindingCookie sets the browser-binding cookie. SameSite /
// Secure attributes mirror m.cookieDevMode so dev (HTTP localhost) and
// prod (HTTPS) deployments use the same posture as the sid carrier.
func (m *Module) writeExchangeBindingCookie(c *gin.Context, token string) {
	cookie := &http.Cookie{
		Name:     exchangeBindingCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   exchangeBindingMaxAge,
		HttpOnly: true,
	}
	if m.cookieDevMode {
		cookie.Secure = false
		cookie.SameSite = http.SameSiteLaxMode
	} else {
		cookie.Secure = true
		cookie.SameSite = http.SameSiteNoneMode
	}
	http.SetCookie(c.Writer, cookie)
}

// clearExchangeBindingCookie writes a delete-cookie response so the
// browser drops the binding token immediately after a successful
// exchange. Defence-in-depth — the cookie's MaxAge already caps it but
// proactive clearing closes a window where a leaked browser profile
// could be replayed offline.
func (m *Module) clearExchangeBindingCookie(c *gin.Context) {
	cookie := &http.Cookie{
		Name:     exchangeBindingCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	}
	if m.cookieDevMode {
		cookie.Secure = false
		cookie.SameSite = http.SameSiteLaxMode
	} else {
		cookie.Secure = true
		cookie.SameSite = http.SameSiteNoneMode
	}
	http.SetCookie(c.Writer, cookie)
}

// readExchangeBindingCookie pulls the binding token from the request.
// Empty string + non-nil error if the cookie is missing.
func (m *Module) readExchangeBindingCookie(c *gin.Context) (string, error) {
	ck, err := c.Request.Cookie(exchangeBindingCookieName)
	if err != nil {
		return "", err
	}
	return ck.Value, nil
}

// buildLinkSuccessRedirect picks the URL the link flow should 302 to
// when the user finishes binding a new IdP. Prefers sess.RedirectBack
// when supplied (already validated at start time); falls back to the
// configured frontend landing URL.
func buildLinkSuccessRedirect(frontendURL, redirectBack, provider string) string {
	target := redirectBack
	if target == "" {
		target = frontendURL
	}
	sep := "?"
	if hasQuery(target) {
		sep = "&"
	}
	return target + sep + "link_status=ok&provider=" + url.QueryEscape(provider)
}

// exchangeRequest is the JSON body of POST /auth/exchange.
type exchangeRequest struct {
	Code string `json:"code" binding:"required"`
}

// exchangeResponse is the success payload of POST /auth/exchange.
type exchangeResponse struct {
	Token        string    `json:"token"`
	ExpiresAt    time.Time `json:"expires_at"`
	RedirectBack string    `json:"redirect_back,omitempty"`
}

// handleExchange consumes a one-shot OAuth auth code (issued by
// handleCallback) and returns a freshly-signed JWT in the JSON body.
//
// SPEC §7 promises "code 必须配合已设置 cookie 的浏览器才能换 token" —
// implementation enforces this via the exchange-binding cookie that
// handleCallback writes alongside the 302. We compare SHA-256 of the
// cookie value against AuthCodeData.BindingHash; any mismatch (or
// missing cookie) returns 401, so a leaked code without the cookie is
// useless.
//
// Raw gin.HandlerFunc rather than handler.HandleRequest because we
// need *gin.Context to read the binding cookie — HandleRequest's
// signature is context.Context-only.
func (m *Module) handleExchange(c *gin.Context) {
	ctx := c.Request.Context()
	var req exchangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		handler.WriteResponse(c, 0, nil, apierr.ErrBind.Wrap(err))
		return
	}
	if req.Code == "" {
		handler.WriteResponse(c, 0, nil,
			apierr.ErrInvalidArgument.WithMessage("code is required"))
		return
	}

	bindToken, err := m.readExchangeBindingCookie(c)
	if err != nil || bindToken == "" {
		// No cookie → can't be the same browser that received the code.
		// 401 + opaque message so we don't leak which check failed.
		handler.WriteResponse(c, 0, nil,
			apierr.ErrUnauthenticated.WithMessage("oauth exchange binding missing"))
		return
	}

	data, err := m.authCodeStore.Take(ctx, req.Code)
	if err != nil {
		if errors.Is(err, ErrAuthCodeNotFound) {
			handler.WriteResponse(c, 0, nil,
				apierr.ErrGone.WithMessage("oauth auth code expired or already consumed"))
			return
		}
		handler.WriteResponse(c, 0, nil, apierr.ErrInternal.Wrap(err))
		return
	}

	// Constant-time comparison so an attacker cannot use timing to brute-
	// force the binding hash byte-by-byte. AuthCodeStore.Take has already
	// consumed the code, so even a successful match below is a one-shot
	// proof — no replay possible.
	gotHash := sha256.Sum256([]byte(bindToken))
	wantHash, decodeErr := hex.DecodeString(data.BindingHash)
	if decodeErr != nil || subtle.ConstantTimeCompare(gotHash[:], wantHash) != 1 {
		if m.logger != nil {
			m.logger.Warn("oauth exchange binding mismatch",
				"user_id", data.UserID)
		}
		handler.WriteResponse(c, 0, nil,
			apierr.ErrUnauthenticated.WithMessage("oauth exchange binding invalid"))
		return
	}

	user, err := m.userStore.Get(ctx, store.RID(data.UserID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			handler.WriteResponse(c, 0, nil,
				apierr.ErrUnauthenticated.WithMessage("account not found"))
			return
		}
		handler.WriteResponse(c, 0, nil, apierr.ErrInternal.Wrap(err))
		return
	}
	if !user.Active {
		handler.WriteResponse(c, 0, nil,
			apierr.ErrUnauthenticated.WithMessage("account is disabled"))
		return
	}

	tok, err := m.issueToken(user)
	if err != nil {
		handler.WriteResponse(c, 0, nil, apierr.FromError(err))
		return
	}
	m.clearExchangeBindingCookie(c)
	handler.WriteResponse(c, http.StatusOK, &exchangeResponse{
		Token:        tok.Token,
		ExpiresAt:    tok.ExpiresAt,
		RedirectBack: data.RedirectBack,
	}, nil)
}

// handleListIdentities returns the authenticated caller's login methods.
// Mounted under m.AuthChain() so a stale-PV token cannot snoop.
type identitiesResponse struct {
	Methods []LoginMethod `json:"methods"`
}

func (m *Module) handleListIdentities(ctx context.Context, _ *struct{}) (*identitiesResponse, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, apierr.ErrUnauthenticated
	}
	methods, err := m.ListLoginMethods(ctx, p.Subject)
	if err != nil {
		return nil, err
	}
	return &identitiesResponse{Methods: methods}, nil
}

// linkIdentityRequest is the JSON body of POST /identities/link.
//
// The flow:
//  1. Authenticated caller POSTs {"provider":"google","redirect_back":"/settings"}.
//  2. handleLinkIdentity validates → creates an OAuthSession with
//     LinkUserID=principal.Subject → issues sid carrier → calls
//     provider.BeginAuth → responds 200 with {"redirect_to":"https://idp..."}.
//  3. The SPA navigates the browser top-frame to redirect_to (a 302
//     response from a fetch() XHR would only redirect the XHR, not the
//     top frame, hence why the URL is returned in JSON).
//  4. After the IdP, the browser hits /{provider}/callback with the
//     same sid cookie. handleCallback sees sess.LinkUserID != "" and
//     routes through Module.LinkIdentity rather than ResolveOAuthIdentity.
type linkIdentityRequest struct {
	Provider     string `json:"provider"      binding:"required"`
	RedirectBack string `json:"redirect_back"`
}

// linkIdentityResponse is the JSON returned to the SPA so it can drive
// the browser top-frame navigation.
type linkIdentityResponse struct {
	RedirectTo string `json:"redirect_to"`
}

// handleLinkIdentity is the entrypoint for "bind another IdP to my
// account". Raw gin handler so we can write the carrier cookie.
func (m *Module) handleLinkIdentity(c *gin.Context) {
	ctx := c.Request.Context()
	principal, ok := auth.PrincipalFrom(ctx)
	if !ok {
		handler.WriteResponse(c, 0, nil, apierr.ErrUnauthenticated)
		return
	}
	var req linkIdentityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		handler.WriteResponse(c, 0, nil, apierr.ErrBind.Wrap(err))
		return
	}
	provider, ok := m.Provider(req.Provider)
	if !ok {
		handler.WriteResponse(c, 0, nil,
			apierr.ErrInvalidArgument.WithMessage("unknown provider: "+req.Provider))
		return
	}
	if err := m.validateRedirectBack(req.RedirectBack); err != nil {
		handler.WriteResponse(c, 0, nil,
			apierr.ErrInvalidArgument.WithMessage("redirect_back not allowed: "+err.Error()))
		return
	}
	redirectTo, err := m.startOAuthFlow(c, provider, req.RedirectBack, principal.Subject)
	if err != nil {
		handler.WriteResponse(c, 0, nil, apierr.FromError(err))
		return
	}
	handler.WriteResponse(c, http.StatusOK, &linkIdentityResponse{RedirectTo: redirectTo}, nil)
}

// unlinkIdentityRequest is the gin path-param variant.
type unlinkIdentityRequest struct {
	IdentityID string `uri:"id" binding:"required"`
}

func (m *Module) handleUnlinkIdentity(ctx context.Context, req *unlinkIdentityRequest) error {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return apierr.ErrUnauthenticated
	}
	return m.UnlinkIdentity(ctx, p.Subject, req.IdentityID)
}

