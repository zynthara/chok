package account

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

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
	req.Email = strings.ToLower(req.Email)

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
	req.Email = strings.ToLower(req.Email)

	user, err := m.store.Get(ctx, where.WithFilter("email", req.Email))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, apierr.ErrUnauthenticated.WithMessage("invalid email or password")
		}
		return nil, err
	}

	if !user.Active {
		return nil, apierr.ErrUnauthenticated.WithMessage("account is disabled")
	}

	if err := auth.ComparePassword(user.PasswordHash, req.Password); err != nil {
		return nil, apierr.ErrUnauthenticated.WithMessage("invalid email or password")
	}

	return m.issueToken(user)
}

func (m *Module) refreshToken(ctx context.Context, _ *refreshTokenRequest) (*tokenResponse, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, apierr.ErrUnauthenticated
	}

	// Verify user still exists and is active.
	user, err := m.store.GetOne(ctx, p.Subject)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, apierr.ErrUnauthenticated.WithMessage("account not found")
		}
		return nil, err
	}
	if !user.Active {
		return nil, apierr.ErrUnauthenticated.WithMessage("account is disabled")
	}

	return m.issueToken(user)
}

func (m *Module) changePassword(ctx context.Context, req *changePasswordRequest) error {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return apierr.ErrUnauthenticated
	}

	user, err := m.store.GetOne(ctx, p.Subject)
	if err != nil {
		return err
	}

	if err := auth.ComparePassword(user.PasswordHash, req.OldPassword); err != nil {
		return apierr.ErrUnauthenticated.WithMessage("old password is incorrect")
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		return err
	}
	user.PasswordHash = hash
	return m.store.UpdateOne(ctx, user, "password_hash")
}

func (m *Module) forgotPassword(ctx context.Context, req *forgotPasswordRequest) error {
	req.Email = strings.ToLower(req.Email)

	user, err := m.store.Get(ctx, where.WithFilter("email", req.Email))
	if err != nil {
		// Always return 204 to prevent email enumeration.
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}

	token, _, err := m.resetJWT.Sign(user.RID, map[string]any{"purpose": "reset"})
	if err != nil {
		return err
	}

	return m.sender.Send(ctx, user.Email, token)
}

func (m *Module) resetPassword(ctx context.Context, req *resetPasswordRequest) error {
	subject, claims, err := m.resetJWT.Parse(req.Token)
	if err != nil {
		return apierr.ErrInvalidArgument.WithMessage("invalid or expired reset token")
	}
	if purpose, _ := claims["purpose"].(string); purpose != "reset" {
		return apierr.ErrInvalidArgument.WithMessage("invalid or expired reset token")
	}

	user, err := m.store.GetOne(ctx, subject)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apierr.ErrInvalidArgument.WithMessage("invalid or expired reset token")
		}
		return err // DB/driver errors surface as 500
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		return err
	}
	user.PasswordHash = hash
	return m.store.UpdateOne(ctx, user, "password_hash")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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
