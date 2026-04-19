package account

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/internal/ctxval"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

// dummyHash is a pre-computed bcrypt hash used for constant-time comparison
// when a login request targets a non-existent email. This prevents timing
// side-channel attacks that could enumerate valid accounts.
var dummyHash string

func init() {
	h, err := auth.HashPassword("dummy-constant-time-padding")
	if err != nil {
		panic("account: failed to generate dummy hash: " + err.Error())
	}
	dummyHash = h
}

// ---------------------------------------------------------------------------
// Request / Response types
// ---------------------------------------------------------------------------

type registerRequest struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required,min=8,max=72"`
	Name     string `json:"name"     binding:"max=100"`
}

type loginRequest struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

type tokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type refreshTokenRequest struct{} // no body, uses token from Authorization header

type changePasswordRequest struct {
	OldPassword string `json:"old_password" binding:"required"`
	NewPassword string `json:"new_password" binding:"required,min=8,max=72"`
}

type forgotPasswordRequest struct {
	Email string `json:"email" binding:"required,email"`
}

type resetPasswordRequest struct {
	Token       string `json:"token"        binding:"required"`
	NewPassword string `json:"new_password" binding:"required,min=8,max=72"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (m *Module) register(ctx context.Context, req *registerRequest) (*tokenResponse, error) {
	req.Email = normalizeEmail(req.Email)

	if err := validatePasswordStrength(req.Password); err != nil {
		return nil, err
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		return nil, err
	}

	name := req.Name
	if name == "" {
		name = maskEmail(req.Email)
	}

	user := &User{
		Email:        req.Email,
		PasswordHash: hash,
		Name:         name,
		Active:       true,
	}
	if err := m.store.Create(ctx, user); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return nil, apierr.ErrConflict.WithMessage("email already registered")
		}
		return nil, err
	}

	return m.issueToken(user)
}

func (m *Module) login(ctx context.Context, req *loginRequest) (*tokenResponse, error) {
	req.Email = normalizeEmail(req.Email)

	// Rate limit is keyed on (email, client-ip). An attacker rotating
	// emails (credential stuffing) still exhausts the IP-keyed bucket,
	// and an attacker rotating IPs against one email still exhausts the
	// email bucket. Either key alone triggers 429.
	clientIP := ctxval.ClientIPFrom(ctx)
	limitKeys := []limiterKey{
		{Name: "email", Value: req.Email},
		{Name: "client_ip", Value: clientIP},
	}
	if m.limiter != nil {
		if ok, triggered := m.limiter.check(limitKeys...); !ok {
			if m.logger != nil {
				m.logger.Warn("account: login rate limit hit",
					"triggered_by", triggered,
					"has_ip", clientIP != "")
			}
			return nil, tooManyRequestsWithRetryAfter(m.limiter.window)
		}
	}

	user, err := m.store.Get(ctx, store.Where(where.WithFilter("email", req.Email)))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Perform a dummy hash comparison to prevent timing-based
			// email enumeration. Without this, requests for non-existent
			// emails return faster (no bcrypt work), leaking user existence.
			auth.ComparePassword(dummyHash, req.Password)
			if m.limiter != nil {
				m.limiter.record(limitKeys...)
			}
			return nil, apierr.ErrUnauthenticated.WithMessage("invalid email or password")
		}
		return nil, err
	}

	if !user.Active {
		return nil, apierr.ErrUnauthenticated.WithMessage("account is disabled")
	}

	if err := auth.ComparePassword(user.PasswordHash, req.Password); err != nil {
		if m.limiter != nil {
			m.limiter.record(limitKeys...)
		}
		return nil, apierr.ErrUnauthenticated.WithMessage("invalid email or password")
	}

	return m.issueToken(user)
}

// tooManyRequestsWithRetryAfter returns ErrTooManyRequests carrying a
// Retry-After header hint so clients can back off deterministically. The
// value is the rate-limit window in whole seconds (RFC 9110 §10.2.3).
func tooManyRequestsWithRetryAfter(window time.Duration) *apierr.Error {
	secs := int(window / time.Second)
	if secs < 1 {
		secs = 1
	}
	return apierr.ErrTooManyRequests.WithHeader("Retry-After", strconv.Itoa(secs))
}

func (m *Module) refreshToken(ctx context.Context, _ *refreshTokenRequest) (*tokenResponse, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, apierr.ErrUnauthenticated
	}

	// Verify user still exists and is active.
	user, err := m.store.Get(ctx, store.RID(p.Subject))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, apierr.ErrUnauthenticated.WithMessage("account not found")
		}
		return nil, err
	}
	if !user.Active {
		return nil, apierr.ErrUnauthenticated.WithMessage("account is disabled")
	}

	// Reject tokens issued before the last password change. The "pv"
	// (password version) claim is incremented whenever a password is
	// changed or reset, so old tokens are automatically invalidated.
	if pv, ok := p.Claims["pv"].(float64); !ok || int(pv) != user.PasswordVersion {
		return nil, apierr.ErrUnauthenticated.WithMessage("token invalidated by password change")
	}

	return m.issueToken(user)
}

func (m *Module) changePassword(ctx context.Context, req *changePasswordRequest) error {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return apierr.ErrUnauthenticated
	}

	user, err := m.store.Get(ctx, store.RID(p.Subject))
	if err != nil {
		return err
	}

	// Reject disabled accounts here just like login/refresh do. A valid
	// access token alone must not be enough for a deactivated user to
	// rotate credentials.
	if !user.Active {
		return apierr.ErrUnauthenticated.WithMessage("account is disabled")
	}

	if err := auth.ComparePassword(user.PasswordHash, req.OldPassword); err != nil {
		return apierr.ErrUnauthenticated.WithMessage("old password is incorrect")
	}

	if err := validatePasswordStrength(req.NewPassword); err != nil {
		return err
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		return err
	}
	user.PasswordHash = hash
	user.PasswordVersion++
	return m.store.Update(ctx, store.RID(user.RID), store.Fields(user, "password_hash", "password_version"))
}

func (m *Module) forgotPassword(ctx context.Context, req *forgotPasswordRequest) error {
	req.Email = normalizeEmail(req.Email)

	user, err := m.store.Get(ctx, store.Where(where.WithFilter("email", req.Email)))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	// Constant-time 204 for every outcome — unknown email, disabled
	// account, active account, and even Sender failures must all look
	// identical to the client. The email-delivery work runs on a
	// background goroutine so the blocking SMTP/HTTP call doesn't
	// stretch the response time on the "account exists + active" path
	// (which is what used to make timing-based enumeration trivial).
	//
	// The detached ctx prevents the request's cancellation from aborting
	// an in-flight Send. Errors are logged but never surfaced.
	if user != nil && user.Active {
		bgCtx := context.WithoutCancel(ctx)
		go m.dispatchResetEmail(bgCtx, user)
	}
	return nil
}

// dispatchResetEmail signs the reset token and calls the Sender. It is
// intended to run asynchronously from forgotPassword — see there for
// the timing-attack rationale. All errors are logged, never returned.
func (m *Module) dispatchResetEmail(ctx context.Context, user *User) {
	defer func() {
		if r := recover(); r != nil && m.logger != nil {
			m.logger.Error("forgot-password dispatch panicked", "panic", r)
		}
	}()
	token, _, err := m.resetJWT.Sign(user.RID, map[string]any{
		"purpose": "reset",
		"pv":      user.PasswordVersion,
	})
	if err != nil {
		if m.logger != nil {
			m.logger.Error("forgot-password: sign token failed", "error", err)
		}
		return
	}
	if err := m.sender.Send(ctx, user.Email, token); err != nil {
		if m.logger != nil {
			m.logger.Error("forgot-password: send failed", "error", err)
		}
	}
}

func (m *Module) resetPassword(ctx context.Context, req *resetPasswordRequest) error {
	subject, claims, err := m.resetJWT.Parse(req.Token)
	if err != nil {
		return apierr.ErrInvalidArgument.WithMessage("invalid or expired reset token")
	}
	if purpose, _ := claims["purpose"].(string); purpose != "reset" {
		return apierr.ErrInvalidArgument.WithMessage("invalid or expired reset token")
	}

	user, err := m.store.Get(ctx, store.RID(subject))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apierr.ErrInvalidArgument.WithMessage("invalid or expired reset token")
		}
		return err // DB/driver errors surface as 500
	}

	// Refuse to reset a disabled account's password. Reuse the same
	// "invalid or expired reset token" message so an attacker cannot
	// distinguish disabled accounts from unknown ones.
	if !user.Active {
		return apierr.ErrInvalidArgument.WithMessage("invalid or expired reset token")
	}

	// Verify the token was issued for the current password version.
	// PasswordVersion is incremented on every password change, so any
	// previously issued reset tokens are automatically invalidated —
	// the same token cannot be replayed after the first successful use.
	pv, ok := claims["pv"].(float64)
	if !ok || int(pv) != user.PasswordVersion {
		return apierr.ErrInvalidArgument.WithMessage("invalid or expired reset token")
	}

	if err := validatePasswordStrength(req.NewPassword); err != nil {
		return err
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		return err
	}
	user.PasswordHash = hash
	user.PasswordVersion++
	return m.store.Update(ctx, store.RID(user.RID), store.Fields(user, "password_hash", "password_version"))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// normalizeEmail canonicalises an email address for lookup, storage, and
// rate-limit keying. Trims surrounding whitespace, then lowercases the
// whole string. Without TrimSpace, inputs like "Alice@Example.com " would
// bypass the per-email rate limiter (different key) and collide with the
// canonical row under distinct-but-equivalent forms.
func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// validatePasswordStrength rejects the lowest-effort password shapes:
// all-digits, all-lowercase, all-uppercase. The `binding:"min=8,max=72"`
// tag on the request already enforces length; this is the character-
// class complement that keeps passwords like "11111111" or "password"
// out of the user table. Applications with stricter requirements should
// layer their own validator on top.
func validatePasswordStrength(pwd string) error {
	var hasLower, hasUpper, hasDigit, hasOther bool
	for _, r := range pwd {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= '0' && r <= '9':
			hasDigit = true
		default:
			hasOther = true
		}
	}
	classes := 0
	for _, b := range []bool{hasLower, hasUpper, hasDigit, hasOther} {
		if b {
			classes++
		}
	}
	if classes < 2 {
		return apierr.ErrInvalidArgument.WithMessage("password must contain at least two of: lowercase, uppercase, digit, symbol")
	}
	return nil
}

// maskEmail generates a display name from an email address by masking
// the local part and domain name with asterisks. The TLD suffix is kept.
//
//	alice@test.com        → a***e@t**t.com
//	ab@test.com           → a*b@t**t.com
//	a@test.com            → a*@t**t.com
//	john.doe@example.com  → j******e@e****e.com
//	alice@mail.test.co.jp → a***e@m**l.t**t.co.jp
func maskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return email
	}
	local := email[:at]
	domain := email[at+1:]

	return maskPart(local) + "@" + maskDomain(domain)
}

func maskPart(s string) string {
	switch len(s) {
	case 0:
		return "*"
	case 1:
		return s + "*"
	case 2:
		return s[:1] + "*" + s[1:]
	default:
		return s[:1] + strings.Repeat("*", len(s)-2) + s[len(s)-1:]
	}
}

// maskDomain masks domain labels but preserves TLD suffixes.
// "example.com" → "e****e.com", "mail.test.co.jp" → "m**l.t**t.co.jp"
func maskDomain(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) <= 1 {
		return maskPart(domain)
	}

	// Find where TLD starts: keep known multi-part TLDs (co.jp, com.cn, etc.)
	// and single TLDs (com, net, org). Simple heuristic: parts with <= 3 chars
	// at the end are TLD segments.
	tldStart := len(parts)
	for i := len(parts) - 1; i >= 1; i-- {
		if len(parts[i]) <= 3 {
			tldStart = i
		} else {
			break
		}
	}
	// At least the last part is TLD.
	if tldStart >= len(parts) {
		tldStart = len(parts) - 1
	}

	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('.')
		}
		if i < tldStart {
			b.WriteString(maskPart(p))
		} else {
			b.WriteString(p)
		}
	}
	return b.String()
}

func (m *Module) issueToken(user *User) (*tokenResponse, error) {
	claims := map[string]any{
		"name": user.Name,
		"pv":   user.PasswordVersion, // password version; used to invalidate tokens on password change
	}
	if roles := user.RoleList(); len(roles) > 0 {
		claims["roles"] = roles
	}

	token, exp, err := m.jwt.Sign(user.RID, claims)
	if err != nil {
		return nil, err
	}
	return &tokenResponse{Token: token, ExpiresAt: exp}, nil
}
