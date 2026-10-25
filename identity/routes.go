package identity

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/internal/openapi"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/store/where"
)

// Mount registers the routes under the prefix (/auth by default):
//
//	POST   {prefix}/register          public; not with disable_register
//	POST   {prefix}/login             public
//	POST   {prefix}/forgot-password   public; with a Sender
//	POST   {prefix}/reset-password    public; with a Sender
//	POST   {prefix}/refresh-token     bearer
//	PUT    {prefix}/change-password   bearer
//	GET    {prefix}/me                bearer
//
// With auth.passwordless.enabled, sign-in by a mailed code:
//
//	POST   {prefix}/passwordless/start    public: mail a code, 202
//	POST   {prefix}/passwordless/verify   public: the code or token for a token
//
// and with auth.passwordless.only no password routes: register, login,
// forgot-password, reset-password and change-password are left out.
//
// With auth.cookie.enabled the routes that issue a token set the session
// cookie too, and one more route clears it:
//
//	POST   {prefix}/logout            bearer or cookie: 204
//
// With providers, the OAuth routes too:
//
//	GET    {prefix}/{name}/start      public; GET or POST /{name}/callback after it
//	POST   {prefix}/exchange          public: the one-time code for a token
//	GET    {prefix}/identities        bearer: the user's login methods
//	POST   {prefix}/identities/link   bearer: link another provider
//	DELETE {prefix}/identities/{id}   bearer: unlink one
//
// and with auth.audit.enabled and admin_api, GET /audit/logs. Public
// routes are documented without security in the OpenAPI document.
func (idn *Identity) Mount(r chok.Router) {
	g := r.Group(idn.cfg.Prefix)
	tag := chok.Tags("auth")
	// With passwordless.only the password routes stay out: accounts are
	// their addresses, and a mailed code is the only way in.
	passwords := !idn.cfg.Passwordless.Only
	if passwords && !idn.cfg.DisableRegister {
		tokenRoute(idn, g, "/register", idn.register, chok.Created(), chok.Public(), tag, chok.Summary("Register an account"))
	}
	if passwords {
		tokenRoute(idn, g, "/login", idn.login, chok.Public(), tag, chok.Summary("Log in"))
	}
	if passwords && idn.sender != nil {
		chok.POST(g, "/forgot-password", idn.forgotPassword, chok.Public(), tag, chok.Summary("Mail a password-reset token"))
		chok.POST(g, "/reset-password", idn.resetPassword, chok.Public(), tag, chok.Summary("Reset the password with a token"))
	}
	if idn.cfg.Passwordless.Enabled {
		chok.POST(g, "/passwordless/start", idn.passwordlessStart, chok.Status(http.StatusAccepted), chok.Public(), tag, chok.Summary("Mail a sign-in code"))
		tokenRoute(idn, g, "/passwordless/verify", idn.passwordlessVerify, chok.Public(), tag, chok.Summary("Sign in with a mailed code"))
	}
	authed := g.Group("", idn.Authn())
	tokenRoute(idn, authed, "/refresh-token", idn.refreshToken, tag, chok.Summary("Refresh the access token"))
	if passwords {
		chok.PUT(authed, "/change-password", idn.changePassword, tag, chok.Summary("Change the password"))
	}
	chok.GET(authed, "/me", idn.me, tag, chok.Summary("Get the signed-in user"))
	if idn.cfg.Cookie.Enabled {
		chok.POST(authed, "/logout", idn.logout, chok.Use(keepResponse), tag, chok.Summary("Log out"))
		// The OpenAPI document lists the cookie next to the bearer token.
		if openapi.UseCookieAuth != nil {
			openapi.UseCookieAuth(r, idn.cfg.Cookie.Name)
		}
	}
	if idn.oauth != nil {
		idn.oauth.mount(g, authed)
	}
	if idn.cfg.Audit.Enabled && idn.cfg.Audit.AdminAPI {
		admin := r.Group("/audit", idn.Authn(), idn.requireAuditReader())
		chok.List(admin, "/logs", idn.audit, chok.Tags("audit"), chok.Summary("List audit records"))
	}
}

type registerRequest struct {
	Email    string `json:"email" validate:"required,email,max=200"`
	Password string `json:"password" validate:"required,min=8,max=72"`
	Name     string `json:"name" validate:"max=100"`
}

type loginRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required"`
}

type tokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password" validate:"required"`
	NewPassword string `json:"new_password" validate:"required,min=8,max=72"`
}

type forgotPasswordRequest struct {
	Email string `json:"email" validate:"required,email"`
}

type resetPasswordRequest struct {
	Token       string `json:"token" validate:"required"`
	NewPassword string `json:"new_password" validate:"required,min=8,max=72"`
}

// meResponse is the signed-in user: the public fields of User and the
// roles its tokens carry.
type meResponse struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	Name          string    `json:"name"`
	EmailVerified bool      `json:"email_verified"`
	Roles         []string  `json:"roles"`
	CreatedAt     time.Time `json:"created_at"`
}

var (
	errEmailTaken  = chok.ErrConflict.Msgf("email already registered").With("field", "email")
	errBadLogin    = chok.ErrUnauthorized.Msgf("invalid email or password")
	errOAuthOnly   = chok.NewError(401, "OAUTH_ONLY_ACCOUNT", "this account has no password; sign in with its OAuth provider, or reset the password")
	errWrongOld    = chok.ErrUnauthorized.Msgf("old password is incorrect")
	errResetToken  = chok.ErrBadRequest.Msgf("invalid or expired reset token")
	errNoPrincipal = chok.ErrUnauthorized
)

func (idn *Identity) register(ctx context.Context, req *registerRequest) (*tokenResponse, error) {
	user, err := idn.createUser(ctx, req.Email, req.Password, req.Name)
	if err != nil {
		return nil, err
	}
	return idn.issueToken(user, time.Now().Unix())
}

// createUser makes an active account with a password: register and
// UserService.Create.
func (idn *Identity) createUser(ctx context.Context, email, password, name string) (*User, error) {
	email = normalizeEmail(email)
	if err := checkPasswordStrength("password", password); err != nil {
		return nil, err
	}
	hash, err := hashPassword(password, idn.cost)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = maskEmail(email)
	}
	user := &User{Email: email, PasswordHash: hash, HasPassword: true, Name: name, Active: true}
	if err := idn.users.Create(ctx, user); err != nil {
		if errors.Is(err, chok.ErrConflict) {
			return nil, errEmailTaken
		}
		return nil, err
	}
	return user, nil
}

func (idn *Identity) login(ctx context.Context, req *loginRequest) (*tokenResponse, error) {
	email := normalizeEmail(req.Email)
	// The throttle counts failures per email and per client address:
	// rotating emails still exhausts the address's bucket, rotating
	// addresses the email's.
	keys := []limiterKey{{Name: "email", Value: email}, {Name: "client_ip", Value: chok.ClientIP(ctx)}}
	if idn.limiter != nil {
		if ok, by := idn.limiter.check(keys...); !ok {
			chok.LoggerFrom(ctx).WarnContext(ctx, "identity: login throttled", "triggered_by", by)
			return nil, chok.ErrRateLimited.Msgf("too many failed logins; try again later").With("retry_after", max(int(idn.cfg.LoginRateWindow/time.Second), 1))
		}
	}
	failed := func() {
		if idn.limiter != nil {
			idn.limiter.record(keys...)
		}
	}
	user, err := idn.users.Get(ctx, store.Where(where.Eq("email", email)))
	if err != nil {
		if !errors.Is(err, chok.ErrNotFound) {
			return nil, err
		}
		// Spend a bcrypt comparison anyway: an unknown email must not
		// answer faster than a wrong password.
		_ = comparePassword(idn.padding, req.Password)
		failed()
		return nil, errBadLogin
	}
	if !user.Active {
		return nil, errDisabled
	}
	// An account created through OAuth holds the hash of a random secret:
	// direct its owner to the provider instead of comparing. Not a
	// credential failure, so the throttle does not count it.
	if !user.HasPassword {
		return nil, errOAuthOnly
	}
	if comparePassword(user.PasswordHash, req.Password) != nil {
		failed()
		return nil, errBadLogin
	}
	return idn.issueToken(user, time.Now().Unix())
}

// issueToken signs an access token for user in the session that began
// at start (Unix seconds).
func (idn *Identity) issueToken(user *User, start int64) (*tokenResponse, error) {
	claims := map[string]any{"name": user.Name, "pv": user.PasswordVersion, "sat": start}
	if roles := user.RoleList(); len(roles) > 0 {
		claims["roles"] = roles
	}
	token, exp, err := idn.access.sign(user.RID, claims)
	if err != nil {
		return nil, err
	}
	return &tokenResponse{Token: token, ExpiresAt: exp.UTC()}, nil
}

// refreshToken trades a valid token for a fresh one in the same session,
// until the session is refresh_ttl old.
func (idn *Identity) refreshToken(ctx context.Context, _ *chok.None) (*tokenResponse, error) {
	s, ok := sessionFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	if ttl := idn.cfg.RefreshTTL; ttl > 0 && time.Since(time.Unix(s.start, 0)) > ttl {
		return nil, errSessionEnded
	}
	return idn.issueToken(s.user, s.start)
}

func (idn *Identity) changePassword(ctx context.Context, req *changePasswordRequest) (chok.None, error) {
	s, ok := sessionFrom(ctx)
	if !ok {
		return chok.None{}, errNoPrincipal
	}
	if comparePassword(s.user.PasswordHash, req.OldPassword) != nil {
		return chok.None{}, errWrongOld
	}
	return chok.None{}, idn.setPassword(ctx, s.user.RID, "new_password", req.NewPassword)
}

// setPassword stores a new password and revokes the user's tokens.
func (idn *Identity) setPassword(ctx context.Context, userID, field, password string) error {
	if err := checkPasswordStrength(field, password); err != nil {
		return err
	}
	hash, err := hashPassword(password, idn.cost)
	if err != nil {
		return err
	}
	return idn.users.Update(ctx, store.RID(userID), store.Set(map[string]any{
		"password_hash":    hash,
		"has_password":     true,
		"password_version": bumpVersion,
	}))
}

func (idn *Identity) forgotPassword(ctx context.Context, req *forgotPasswordRequest) (chok.None, error) {
	user, err := idn.users.Get(ctx, store.Where(where.Eq("email", normalizeEmail(req.Email))))
	if err != nil && !errors.Is(err, chok.ErrNotFound) {
		return chok.None{}, err
	}
	// Every outcome is the same 204, at the same speed: the mail goes out
	// in the background, so whether the account exists never shows.
	if err == nil && user.Active {
		// The token is bound to the password version, so it works once.
		token, exp, err := idn.reset.sign(user.RID, map[string]any{"purpose": "reset", "pv": user.PasswordVersion})
		if err != nil {
			return chok.None{}, err
		}
		idn.sendMail(ctx, Mail{Kind: "password-reset", To: user.Email, Token: token, Link: withToken(idn.cfg.ResetURL, token), ExpiresAt: exp.UTC()})
	}
	return chok.None{}, nil
}

func (idn *Identity) resetPassword(ctx context.Context, req *resetPasswordRequest) (chok.None, error) {
	sub, claims, err := idn.reset.parse(req.Token)
	if err != nil {
		return chok.None{}, errResetToken
	}
	if purpose, _ := claims["purpose"].(string); purpose != "reset" {
		return chok.None{}, errResetToken
	}
	user, err := idn.users.Get(ctx, store.RID(sub))
	if err != nil {
		if errors.Is(err, chok.ErrNotFound) {
			return chok.None{}, errResetToken
		}
		return chok.None{}, err
	}
	// A disabled account reads like an unknown one; a token from before
	// the last password change — this one used, or an older one — fails.
	if pv, ok := intClaim(claims, "pv"); !user.Active || !ok || pv != int64(user.PasswordVersion) {
		return chok.None{}, errResetToken
	}
	return chok.None{}, idn.setPassword(ctx, user.RID, "new_password", req.NewPassword)
}

func (idn *Identity) me(ctx context.Context, _ *chok.None) (*meResponse, error) {
	s, ok := sessionFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	u := s.user
	roles := u.RoleList()
	if roles == nil {
		roles = []string{}
	}
	return &meResponse{ID: u.RID, Email: u.Email, Name: u.Name, EmailVerified: u.EmailVerified, Roles: roles, CreatedAt: u.CreatedAt.UTC()}, nil
}

// requireAuditReader admits the audit log's readers: those identity/rbac
// grants audit/read, or without rbac the holders of the admin role.
func (idn *Identity) requireAuditReader() chok.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			p, ok := chok.PrincipalFrom(ctx)
			if !ok {
				chok.WriteError(w, r, chok.ErrUnauthorized)
				return
			}
			allowed := p.HasRole("admin")
			if fn := idn.authorize.Load(); fn != nil {
				var err error
				if allowed, err = (*fn)(ctx, p.UserID, "audit", "read"); err != nil {
					chok.WriteError(w, r, err)
					return
				}
			}
			if !allowed {
				chok.WriteError(w, r, chok.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
