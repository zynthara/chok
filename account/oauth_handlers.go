package account

import (
	"context"
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

// handleBegin returns the gin handler for GET /auth/{name}/start.
//
// Lifecycle: validate redirect_back → mint sid + state/nonce/PKCE →
// persist OAuthSession → issue sid carrier → invoke provider.BeginAuth →
// 302 redirect to IdP authorize URL. Any failure rolls back the session
// store entry so an attacker cannot exhaust capacity by triggering Issue
// errors.
func (m *Module) handleBegin(p AuthProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		redirectBack := c.Query("redirect_back")
		if err := m.validateRedirectBack(c.Request, redirectBack); err != nil {
			handler.WriteResponse(c, 0, nil,
				apierr.ErrInvalidArgument.WithMessage("redirect_back not allowed: "+err.Error()))
			return
		}

		caps := p.Capabilities()
		sess := &OAuthSession{
			State:        randomID(),
			Provider:     p.Name(),
			RedirectBack: redirectBack,
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
			handler.WriteResponse(c, 0, nil, apierr.ErrInternal.Wrap(err))
			return
		}
		if err := m.sessionCarrier.Issue(c, sid); err != nil {
			// Roll back the saved session so capacity is reclaimed and
			// no orphan session lingers; ignore Take's error since we
			// are already on the failure path.
			_, _ = m.sessionStore.Take(context.WithoutCancel(ctx), sid)
			handler.WriteResponse(c, 0, nil, apierr.ErrInternal.Wrap(err))
			return
		}

		resp, err := p.BeginAuth(ctx, &BeginRequest{
			State:         sess.State,
			Nonce:         sess.Nonce,
			CodeChallenge: pkceChallenge(sess.CodeVerifier),
			RedirectBack:  sess.RedirectBack,
		})
		if err != nil {
			// Provider failed before we sent any redirect. Roll back so
			// the sid the carrier just issued is invalidated.
			_, _ = m.sessionStore.Take(context.WithoutCancel(ctx), sid)
			handler.WriteResponse(c, 0, nil, apierr.ErrInternal.Wrap(err))
			return
		}
		c.Redirect(http.StatusFound, resp.RedirectTo)
	}
}

// handleCallback returns the gin handler for GET or POST
// /auth/{name}/callback.
//
// Reads sid from carrier → atomic Take from sessionStore → state check →
// extract code/state from query (GET) or form (POST/form_post) →
// provider.CompleteAuth → ResolveOAuthIdentity → write one-shot
// AuthCode → 302 to oauthCallbackFrontendURL?code=…
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
			// sid bound to a different provider — likely tampering, treat
			// as session-not-found (don't leak which provider owned it).
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

		user, _, err := m.ResolveOAuthIdentity(ctx, ident)
		if err != nil {
			handler.WriteResponse(c, 0, nil, apierr.FromError(err))
			return
		}

		authCode := randomID()
		if err := m.authCodeStore.Save(ctx, authCode, &AuthCodeData{
			UserID:       user.RID,
			RedirectBack: sess.RedirectBack,
			CreatedAt:    time.Now(),
		}, 5*time.Second); err != nil {
			handler.WriteResponse(c, 0, nil, apierr.ErrInternal.Wrap(err))
			return
		}

		// Concatenate the front-end URL with ?code=. Url-escape just the
		// code value; the frontendURL itself was set by the operator and
		// is trusted as configured.
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
// The JWT never appears in the URL — see SPEC §7.
func (m *Module) handleExchange(ctx context.Context, req *exchangeRequest) (*exchangeResponse, error) {
	data, err := m.authCodeStore.Take(ctx, req.Code)
	if err != nil {
		if errors.Is(err, ErrAuthCodeNotFound) {
			return nil, apierr.ErrGone.WithMessage("oauth auth code expired or already consumed")
		}
		return nil, err
	}
	user, err := m.userStore.Get(ctx, store.RID(data.UserID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, apierr.ErrUnauthenticated.WithMessage("account not found")
		}
		return nil, err
	}
	if !user.Active {
		return nil, apierr.ErrUnauthenticated.WithMessage("account is disabled")
	}
	tok, err := m.issueToken(user)
	if err != nil {
		return nil, err
	}
	return &exchangeResponse{
		Token:        tok.Token,
		ExpiresAt:    tok.ExpiresAt,
		RedirectBack: data.RedirectBack,
	}, nil
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

// linkIdentityRequest is the JSON body of POST /auth/identities/link.
// The flow is:
//   1. Front-end calls POST /auth/identities/link → 302 to /auth/{name}/start?redirect_back=…
//   2. Browser bounces through IdP and lands at /auth/{name}/callback
//   3. Module.ResolveOAuthIdentity sees the existing local user (because
//      the original session is authenticated) and... actually no — that
//      flow is more involved. Phase 2 v0.3 keeps it simple: this handler
//      is the entry-point HTTP-side; it just returns 302 to /auth/{p}/start
//      with the link intent encoded in the redirect_back. Phase 4+ may
//      formalise the link-flow with a dedicated session marker.
type linkIdentityRequest struct {
	Provider     string `json:"provider"      binding:"required"`
	RedirectBack string `json:"redirect_back"`
}

func (m *Module) handleLinkIdentity(ctx context.Context, req *linkIdentityRequest) (*linkIdentityRedirect, error) {
	if _, ok := auth.PrincipalFrom(ctx); !ok {
		return nil, apierr.ErrUnauthenticated
	}
	if _, ok := m.Provider(req.Provider); !ok {
		return nil, apierr.ErrInvalidArgument.WithMessage("unknown provider: " + req.Provider)
	}
	if req.RedirectBack != "" {
		if err := m.validateRedirectBack(nil, req.RedirectBack); err != nil {
			return nil, apierr.ErrInvalidArgument.WithMessage("redirect_back not allowed: " + err.Error())
		}
	}
	return &linkIdentityRedirect{
		StartURL: "/auth/" + req.Provider + "/start" + buildRedirectQuery(req.RedirectBack),
	}, nil
}

// linkIdentityRedirect tells the front-end where to navigate to start
// the OAuth dance for binding. Returned as JSON rather than a 302 so
// SPAs that talk to the back-end via fetch() can handle the navigation
// (a 302 from a fetch() redirects the XHR, not the browser top frame).
type linkIdentityRedirect struct {
	StartURL string `json:"start_url"`
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

func buildRedirectQuery(redirectBack string) string {
	if redirectBack == "" {
		return ""
	}
	return "?redirect_back=" + url.QueryEscape(redirectBack)
}
