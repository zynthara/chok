package identity

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/identity/internal/oauth"
	"github.com/zynthara/chok/v3/internal/csrf"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/store/where"
)

// oauthFlow is OAuth sign-in: the providers, the cookie that carries a
// sign-in across the provider's redirect, and the server-side state of
// the sign-ins in flight.
type oauthFlow struct {
	idn         *Identity
	providers   map[string]Provider
	names       []string // sorted
	carrier     *cookieCarrier
	store       *memoryStore
	devMode     bool
	allowed     []allowedRedirect
	frontend    string
	linkByEmail bool
}

// The names of the flow's cookies: the session id across the provider's
// redirect, and the binding that ties an exchange to the browser.
const (
	sessionCookie = "_chok_oauth_sid"
	bindingCookie = "_chok_oauth_xchg"
	// bindingMaxAge outlives the code (5 s) long enough for a front end to
	// load and call exchange, and no longer.
	bindingMaxAge = 60
	// memoryCapacity and memoryCleanup size the in-process store.
	memoryCapacity = 10_000
	memoryCleanup  = time.Minute
)

func newOAuthFlow(idn *Identity, providers []Provider) (*oauthFlow, error) {
	cfg := idn.cfg
	if cfg.OAuthCallbackFrontendURL == "" {
		return nil, errors.New("identity: auth.oauth_callback_frontend_url is required with providers")
	}
	f := &oauthFlow{idn: idn, providers: map[string]Provider{}, frontend: cfg.OAuthCallbackFrontendURL, linkByEmail: cfg.LinkByEmail}
	for i, p := range providers {
		if p == nil {
			return nil, fmt.Errorf("identity: Providers: provider %d is nil", i)
		}
		name := p.Name()
		if !providerShape.MatchString(name) {
			return nil, fmt.Errorf("identity: Providers: %q must be a lower-case name such as google", name)
		}
		if _, dup := f.providers[name]; dup {
			return nil, fmt.Errorf("identity: Providers: %s is given twice", name)
		}
		if err := p.Check(); err != nil {
			return nil, fmt.Errorf("identity: provider %s: %w", name, err)
		}
		f.providers[name] = p
		f.names = append(f.names, name)
		if i == 0 {
			// An http callback on localhost means local development, where
			// browsers drop Secure cookies.
			f.devMode = isLocalDevRedirect(p.Capabilities().RedirectURL)
		}
	}
	slices.Sort(f.names)
	for _, raw := range cfg.AllowedRedirectBacks {
		entry, err := parseAllowedRedirect(raw)
		if err != nil {
			return nil, fmt.Errorf("identity: allowed_redirect_backs %q: %w", raw, err)
		}
		if entry.scheme == "http" {
			idn.logger.Warn("identity: allowed_redirect_backs has an http entry; production should be https", "url", raw)
		}
		f.allowed = append(f.allowed, entry)
	}
	secret, err := hkdf.Key(sha256.New, []byte(cfg.SigningKey), nil, "chok/oauth-session/v1", 32)
	if err != nil {
		return nil, fmt.Errorf("identity: derive the oauth cookie secret: %w", err)
	}
	f.carrier = newCookieCarrier(secret, sessionCookie, f.devMode)
	f.store = newMemoryStore(memoryCapacity, memoryCleanup)
	return f, nil
}

func (f *oauthFlow) close() error {
	f.store.close()
	return nil
}

// isLocalDevRedirect reports an http URL on localhost.
func isLocalDevRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// mount registers the OAuth routes: g is the prefix group, authed the
// same group behind Authn.
func (f *oauthFlow) mount(g, authed chok.Router) {
	tag := chok.Tags("auth")
	for _, name := range f.names {
		p := f.providers[name]
		g.Handle(http.MethodGet, "/"+name+"/start", f.handleBegin(p))
		g.Handle(callbackMethod(p), "/"+name+"/callback", f.handleCallback(p))
		// A provider may post the callback from its own site (Apple's
		// form_post): the state parameter, not the origin, defends it.
		if csrf.Exempt != nil {
			csrf.Exempt(g, http.MethodPost, "/"+name+"/callback")
		}
	}
	g.Handle(http.MethodPost, "/exchange", http.HandlerFunc(f.handleExchange))
	chok.GET(authed, "/identities", f.listMethods, tag, chok.Summary("List the login methods"))
	authed.Handle(http.MethodPost, "/identities/link", http.HandlerFunc(f.handleLink))
	chok.DELETE(authed, "/identities/{id}", f.unlink, tag, chok.Summary("Unlink a login method"))
}

// callbackMethod is POST for a provider that posts its callback as a
// form, GET otherwise.
func callbackMethod(p Provider) string {
	c := p.Capabilities()
	if strings.EqualFold(c.CallbackMethod, http.MethodPost) || c.RequiresFormPost {
		return http.MethodPost
	}
	return http.MethodGet
}

// handleBegin is GET {prefix}/{name}/start?redirect_back=…: it checks
// redirect_back, starts a sign-in and redirects to the provider.
func (f *oauthFlow) handleBegin(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		back := r.URL.Query().Get("redirect_back")
		if err := f.validateRedirectBack(back); err != nil {
			chok.WriteError(w, r, chok.ErrBadRequest.Msgf("redirect_back not allowed: %v", err).With("field", "redirect_back"))
			return
		}
		to, err := f.start(w, r, p, back, "")
		if err != nil {
			chok.WriteError(w, r, err)
			return
		}
		http.Redirect(w, r, to, http.StatusFound)
	}
}

// start mints the sign-in's session — state, nonce and PKCE verifier as
// the provider needs — saves it, sets the session cookie and returns the
// provider's authorization URL. A failure removes the saved session.
// linkUserID makes it a link of another provider to that user.
func (f *oauthFlow) start(w http.ResponseWriter, r *http.Request, p Provider, back, linkUserID string) (string, error) {
	caps := p.Capabilities()
	sess := &oauthSession{State: randomID(), Provider: p.Name(), RedirectBack: back, LinkUserID: linkUserID, CreatedAt: time.Now()}
	if caps.RequiresNonce {
		sess.Nonce = randomID()
	}
	if caps.SupportsPKCE {
		sess.CodeVerifier = randomID()
	}
	sid := randomID()
	f.store.saveSession(sid, sess, oauthSessionTTL)
	to, err := p.BeginAuth(r.Context(), &oauth.BeginRequest{
		State: sess.State, Nonce: sess.Nonce, CodeChallenge: pkceChallenge(sess.CodeVerifier), RedirectBack: back,
	})
	if err != nil {
		_, _ = f.store.takeSession(sid)
		return "", chok.ErrInternal.Wrap(fmt.Errorf("identity: %s: begin: %w", p.Name(), err))
	}
	f.carrier.issue(w, sid)
	return to, nil
}

var (
	errSessionGone = chok.NewError(http.StatusGone, "Gone", "oauth session expired")
	errCodeGone    = chok.NewError(http.StatusGone, "Gone", "oauth code expired or already used")
	errBindMissing = chok.ErrUnauthorized.Msgf("oauth exchange binding missing")
	errBindInvalid = chok.ErrUnauthorized.Msgf("oauth exchange binding invalid")
)

// handleCallback is the provider's callback, GET or (for a form post)
// POST {prefix}/{name}/callback. It takes the session the cookie names,
// checks the state, has the provider complete the sign-in, then either
// links the provider's account to the user who started a link, or signs
// the user in: it leaves a one-time code, sets the binding cookie and
// redirects to the front end with ?code=.
func (f *oauthFlow) handleCallback(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		caps := p.Capabilities()
		sid, err := f.carrier.read(w, r)
		if err != nil {
			chok.WriteError(w, r, chok.ErrBadRequest.Msgf("oauth session id missing or invalid"))
			return
		}
		sess, err := f.store.takeSession(sid)
		if err != nil {
			chok.WriteError(w, r, errSessionGone)
			return
		}
		if sess.Provider != p.Name() {
			// A session of another provider: tampering or a stale URL. The
			// answer is the same as an expiry, so it says nothing more.
			chok.LoggerFrom(ctx).WarnContext(ctx, "identity: oauth callback for another provider's session", "session", sess.Provider, "callback", p.Name())
			chok.WriteError(w, r, errSessionGone)
			return
		}
		var form url.Values
		if caps.RequiresFormPost {
			if err := r.ParseForm(); err != nil {
				chok.WriteError(w, r, chok.ErrBadRequest.Msgf("invalid form body"))
				return
			}
			form = r.PostForm
		}
		param := func(key string) string {
			if caps.RequiresFormPost {
				return form.Get(key)
			}
			return r.URL.Query().Get(key)
		}
		state := param("state")
		if state != sess.State {
			chok.WriteError(w, r, chok.ErrBadRequest.Msgf("oauth state mismatch"))
			return
		}
		ext, err := p.CompleteAuth(ctx, &oauth.CompleteRequest{
			Code: param("code"), State: state, Nonce: sess.Nonce, CodeVerifier: sess.CodeVerifier, FormBody: form,
		})
		if err != nil {
			chok.WriteError(w, r, err)
			return
		}
		if ext.Provider == "" {
			ext.Provider = p.Name()
		}
		if sess.LinkUserID != "" {
			if _, err := f.link(ctx, sess.LinkUserID, ext); err != nil {
				chok.WriteError(w, r, err)
				return
			}
			http.Redirect(w, r, linkedRedirect(f.frontend, sess.RedirectBack, p.Name()), http.StatusFound)
			return
		}
		user, _, err := f.resolve(ctx, ext)
		if err != nil {
			chok.WriteError(w, r, err)
			return
		}
		binding := randomID()
		sum := sha256.Sum256([]byte(binding))
		code := randomID()
		f.store.saveCode(code, &authCode{UserID: user.RID, RedirectBack: sess.RedirectBack, BindingHash: fmt.Sprintf("%x", sum)}, authCodeTTL)
		http.SetCookie(w, flowCookie(bindingCookie, binding, bindingMaxAge, f.devMode))
		http.Redirect(w, r, withQuery(f.frontend, "code", code), http.StatusFound)
	}
}

// withQuery appends key=value to u's query.
func withQuery(u, key, value string) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + key + "=" + url.QueryEscape(value)
}

// linkedRedirect is where a finished link goes: the link's redirect_back,
// checked at its start, or the front end, with ?link_status=ok.
func linkedRedirect(frontend, back, provider string) string {
	target := back
	if target == "" {
		target = frontend
	}
	return withQuery(withQuery(target, "link_status", "ok"), "provider", provider)
}

type exchangeRequest struct {
	Code string `json:"code"`
}

// exchangeResponse is the token of an OAuth sign-in; with cookie.only it
// travels in the session cookie alone, and Token is empty.
type exchangeResponse struct {
	Token        string    `json:"token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	RedirectBack string    `json:"redirect_back,omitempty"`
}

// handleExchange is POST {prefix}/exchange {"code"}: it trades the code a
// callback left for a token, once, and only from the browser that holds
// the binding cookie the callback set — a code leaked from a URL is
// useless on its own. The token travels in the response body, never in a
// URL.
func (f *oauthFlow) handleExchange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req exchangeRequest
	if err := readJSON(w, r, &req); err != nil {
		chok.WriteError(w, r, err)
		return
	}
	if req.Code == "" {
		chok.WriteError(w, r, chok.ErrValidation.Msgf("code is required").With("fields", map[string]string{"code": "required"}))
		return
	}
	ck, err := r.Cookie(bindingCookie)
	if err != nil || ck.Value == "" {
		chok.WriteError(w, r, errBindMissing)
		return
	}
	data, err := f.store.takeCode(req.Code)
	if err != nil {
		chok.WriteError(w, r, errCodeGone)
		return
	}
	// The code is spent already: even a match is good once.
	got := sha256.Sum256([]byte(ck.Value))
	if !hmacEqualHex(got[:], data.BindingHash) {
		chok.LoggerFrom(ctx).WarnContext(ctx, "identity: oauth exchange binding mismatch", "user_id", data.UserID)
		chok.WriteError(w, r, errBindInvalid)
		return
	}
	user, err := f.idn.users.Get(ctx, store.RID(data.UserID))
	if err != nil {
		if errors.Is(err, chok.ErrNotFound) {
			err = errNoAccount
		}
		chok.WriteError(w, r, err)
		return
	}
	if !user.Active {
		chok.WriteError(w, r, errDisabled)
		return
	}
	tok, err := f.idn.issueToken(ctx, user, time.Now().Unix())
	if err != nil {
		chok.WriteError(w, r, err)
		return
	}
	http.SetCookie(w, flowCookie(bindingCookie, "", -1, f.devMode))
	resp := &exchangeResponse{Token: tok.Token, ExpiresAt: tok.ExpiresAt, RedirectBack: data.RedirectBack}
	if c := f.idn.cfg.Cookie; c.Enabled {
		http.SetCookie(w, f.idn.tokenCookie(tok.Token))
		if c.Only {
			resp.Token = ""
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type linkRequest struct {
	Provider     string `json:"provider"`
	RedirectBack string `json:"redirect_back"`
}

type linkResponse struct {
	RedirectTo string `json:"redirect_to"`
}

// handleLink is POST {prefix}/identities/link {"provider","redirect_back"}
// for a signed-in user: it starts a sign-in at the provider that links
// the account to this user, and answers the URL to send the browser to —
// a 302 would only move the front end's fetch, not the page.
func (f *oauthFlow) handleLink(w http.ResponseWriter, r *http.Request) {
	p, ok := chok.PrincipalFrom(r.Context())
	if !ok {
		chok.WriteError(w, r, chok.ErrUnauthorized)
		return
	}
	var req linkRequest
	if err := readJSON(w, r, &req); err != nil {
		chok.WriteError(w, r, err)
		return
	}
	provider, ok := f.providers[req.Provider]
	if !ok {
		chok.WriteError(w, r, chok.ErrBadRequest.Msgf("unknown provider %q", req.Provider).With("field", "provider"))
		return
	}
	if err := f.validateRedirectBack(req.RedirectBack); err != nil {
		chok.WriteError(w, r, chok.ErrBadRequest.Msgf("redirect_back not allowed: %v", err).With("field", "redirect_back"))
		return
	}
	to, err := f.start(w, r, provider, req.RedirectBack, p.UserID)
	if err != nil {
		chok.WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, &linkResponse{RedirectTo: to})
}

// loginMethod is one way a user signs in: "password", or a provider with
// the identity's id, which unlink takes.
type loginMethod struct {
	Type       string `json:"type"`
	IdentityID string `json:"identity_id,omitempty"`
	Email      string `json:"email,omitempty"`
}

type methodsResponse struct {
	Methods []loginMethod `json:"methods"`
}

type unlinkRequest struct {
	ID string `path:"id" validate:"required"`
}

func (f *oauthFlow) listMethods(ctx context.Context, _ *chok.None) (*methodsResponse, error) {
	s, ok := sessionFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	methods, err := f.loginMethods(ctx, s.user)
	if err != nil {
		return nil, err
	}
	return &methodsResponse{Methods: methods}, nil
}

func (f *oauthFlow) unlink(ctx context.Context, req *unlinkRequest) (chok.None, error) {
	p, ok := chok.PrincipalFrom(ctx)
	if !ok {
		return chok.None{}, errNoPrincipal
	}
	return chok.None{}, f.unlinkIdentity(ctx, p.UserID, req.ID)
}

// resolve maps a provider's identity to a user: the user it is already
// linked to; else, with link_by_email, the existing user with the same
// verified email; else a new user without a password. A new link needs
// an email the provider vouches for and does not alias.
func (f *oauthFlow) resolve(ctx context.Context, ext *oauth.Identity) (*User, *linkedIdentity, error) {
	if ext.Provider == "" || ext.ProviderAccountID == "" {
		return nil, nil, errors.New("identity: the provider's identity has no provider or account id")
	}
	// The same spelling of an address as register and login use, or an
	// IdP's "Alice@Example.com" would never meet the local account.
	ext.Email = normalizeEmail(ext.Email)
	existing, err := f.idn.links.Get(ctx, store.Where(where.Eq("provider", ext.Provider), where.Eq("provider_account_id", ext.ProviderAccountID)))
	switch {
	case err == nil:
		user, err := f.idn.users.Get(ctx, store.RID(existing.UserID))
		if err != nil {
			return nil, nil, err
		}
		if !user.Active {
			return nil, nil, errDisabled
		}
		f.touch(ctx, existing.RID)
		return user, existing, nil
	case !errors.Is(err, chok.ErrNotFound):
		return nil, nil, err
	}
	if ext.Email == "" || !ext.EmailVerified || ext.IsAliasedEmail {
		return nil, nil, chok.NewError(http.StatusBadRequest, "OAUTH_EMAIL_REQUIRED",
			"an account cannot be created through "+ext.Provider+" without a verified, non-relay email").
			With("provider", ext.Provider).With("missing", missingFor(ext)).With("next_actions", nextActions(ext.Provider))
	}
	var user *User
	var link *linkedIdentity
	// A user and its link are written together: a failed link leaves no
	// orphan user.
	err = f.idn.d.RunInTx(ctx, func(ctx context.Context) error {
		if f.linkByEmail {
			u, err := f.idn.users.Get(ctx, store.Where(where.Eq("email", ext.Email)))
			switch {
			case err == nil && u.EmailVerified:
				l, err := f.linkTx(ctx, u.RID, ext)
				if err != nil {
					return err
				}
				user, link = u, l
				return nil
			case err != nil && !errors.Is(err, chok.ErrNotFound):
				return err
			}
		}
		// An account without a password: its hash is of a random secret
		// nobody knows, and HasPassword false sends a password login to the
		// provider instead.
		hash, err := hashPassword(randomSecret(), f.idn.cost)
		if err != nil {
			return err
		}
		u := &User{
			Email: ext.Email, EmailVerified: true, PasswordHash: hash, HasPassword: false,
			Name: firstNonEmpty(ext.Name, maskEmail(ext.Email)), Active: true,
		}
		if err := f.idn.users.Create(ctx, u); err != nil {
			if errors.Is(err, chok.ErrConflict) {
				return chok.ErrConflict.Msgf("the email is already registered: sign in with the password and link %s in the account settings", ext.Provider).With("field", "email")
			}
			return err
		}
		l, err := f.linkTx(ctx, u.RID, ext)
		if err != nil {
			return err
		}
		user, link = u, l
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return user, link, nil
}

// link attaches a provider's account to an existing user, for a signed-in
// user linking one more provider.
func (f *oauthFlow) link(ctx context.Context, userID string, ext *oauth.Identity) (*linkedIdentity, error) {
	if userID == "" || ext.Provider == "" || ext.ProviderAccountID == "" {
		return nil, errors.New("identity: a link needs the user and the provider's account")
	}
	var l *linkedIdentity
	err := f.idn.d.RunInTx(ctx, func(ctx context.Context) error {
		var err error
		l, err = f.linkTx(ctx, userID, ext)
		return err
	})
	return l, err
}

// linkTx writes the link in the caller's transaction; a provider account
// linked to someone else already is a 409.
func (f *oauthFlow) linkTx(ctx context.Context, userID string, ext *oauth.Identity) (*linkedIdentity, error) {
	profile, err := json.Marshal(ext.Raw)
	if err != nil {
		// A payload json cannot encode (a channel, a cycle) costs only the
		// profile: the link's columns hold what sign-in needs.
		chok.LoggerFrom(ctx).WarnContext(ctx, "identity: oauth profile not kept", "provider", ext.Provider, "error", err)
		profile = nil
	}
	now := time.Now().UTC()
	l := &linkedIdentity{
		UserID: userID, Provider: ext.Provider, ProviderAccountID: ext.ProviderAccountID, Email: ext.Email,
		Profile: jsonData(profile), LastUsedAt: &now,
	}
	if err := f.idn.links.Create(ctx, l); err != nil {
		if errors.Is(err, chok.ErrConflict) {
			return nil, chok.ErrConflict.Msgf("this %s account is linked to another user", ext.Provider)
		}
		return nil, err
	}
	return l, nil
}

// touch records a sign-in through the link; a failure is only logged —
// the user is signed in either way.
func (f *oauthFlow) touch(ctx context.Context, linkID string) {
	if err := f.idn.links.Update(ctx, store.RID(linkID), store.Set(map[string]any{"last_used_at": time.Now().UTC()})); err != nil {
		chok.LoggerFrom(ctx).WarnContext(ctx, "identity: oauth last_used_at not recorded", "identity_id", linkID, "error", err)
	}
}

var errLastMethod = chok.NewError(http.StatusPreconditionFailed, "FailedPrecondition", "keep at least one way to sign in: a password or a provider")

// unlinkIdentity removes one of the user's links, unless it is the last
// way to sign in. The user row is locked for the count and the delete, so
// two unlinks at once cannot both pass the guard and leave none.
func (f *oauthFlow) unlinkIdentity(ctx context.Context, userID, linkID string) error {
	return f.idn.d.RunInTx(ctx, func(ctx context.Context) error {
		u, err := f.idn.users.GetForUpdate(ctx, store.RID(userID))
		if err != nil {
			return notFoundUser(err)
		}
		// Another user's link reads as missing, so its existence stays
		// hidden.
		if _, err := f.idn.links.Get(ctx, store.Where(where.Eq("id", linkID), where.Eq("user_id", userID))); err != nil {
			if errors.Is(err, chok.ErrNotFound) {
				return chok.ErrNotFound.Msgf("identity not found")
			}
			return err
		}
		n, err := f.idn.links.Count(ctx, where.Eq("user_id", userID))
		if err != nil {
			return err
		}
		if u.HasPassword {
			n++
		}
		if n <= 1 {
			return errLastMethod
		}
		return f.idn.links.Delete(ctx, store.RID(linkID))
	})
}

// links lists the user's links.
func (f *oauthFlow) links(ctx context.Context, userID string) ([]linkedIdentity, error) {
	page, err := f.idn.links.List(ctx, where.Eq("user_id", userID), where.Order("created_at"), where.Limit(100))
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

// loginMethods is every way the user signs in: the password, when the
// user set one, then each linked provider.
func (f *oauthFlow) loginMethods(ctx context.Context, u *User) ([]loginMethod, error) {
	links, err := f.links(ctx, u.RID)
	if err != nil {
		return nil, err
	}
	out := make([]loginMethod, 0, len(links)+1)
	if u.HasPassword {
		out = append(out, loginMethod{Type: "password", Email: u.Email})
	}
	for _, l := range links {
		out = append(out, loginMethod{Type: l.Provider, IdentityID: l.RID, Email: l.Email})
	}
	return out, nil
}

// missingFor names what the provider's identity lacks for a new link.
func missingFor(ext *oauth.Identity) string {
	switch {
	case ext.Email == "":
		return "email"
	case !ext.EmailVerified:
		return "verified"
	case ext.IsAliasedEmail:
		return "non_aliased"
	}
	return ""
}

func nextActions(provider string) []string {
	return []string{
		"existing users: sign in with the password, then link " + provider + " in the account settings",
		"new users: register with an email and a password first",
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// randomID is 128 random bits, base64url: a session id, a state, a code.
func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand never fails on supported platforms
	return base64.RawURLEncoding.EncodeToString(b)
}

// randomSecret is the unguessable password of an account created through
// a provider.
func randomSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// pkceChallenge is the S256 challenge of a PKCE verifier (RFC 7636 §4.2).
func pkceChallenge(verifier string) string {
	if verifier == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
