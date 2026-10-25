package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/store/where"
)

// loginCode is an auth_login_codes row: the code and magic-link token
// last mailed to an address — their HMACs, never themselves — and the
// wrong codes tried against them. An address has one row at most; a
// sign-in deletes it, the hourly sweep the expired ones.
type loginCode struct {
	ID        string    `gorm:"primaryKey;size:24"`
	Email     string    `gorm:"size:200;not null;uniqueIndex:uk_login_code_email"`
	CodeHash  string    `gorm:"size:64;not null"`
	TokenHash string    `gorm:"size:64;not null;uniqueIndex:uk_login_code_token"`
	Attempts  int       `gorm:"not null;default:0"`
	ExpiresAt time.Time `gorm:"not null;index"`
	CreatedAt time.Time `gorm:"not null"`
}

func (loginCode) TableName() string { return "auth_login_codes" }

// loginCodeSweep is how often the expired codes are deleted.
const loginCodeSweep = time.Hour

type passwordlessStartRequest struct {
	Email string `json:"email" validate:"required,email,max=200"`
}

// passwordlessVerifyRequest is either {email, code} or {token}; with both,
// the token decides.
type passwordlessVerifyRequest struct {
	Email string `json:"email" validate:"omitempty,email,max=200"`
	Code  string `json:"code" validate:"omitempty,numeric,max=10"`
	Token string `json:"token" validate:"omitempty,max=128"`
}

var (
	errInvalidCode = chok.NewError(http.StatusUnauthorized, "InvalidCode", "the code is not right")
	errCodeExpired = chok.NewError(http.StatusUnauthorized, "CodeExpired", "the code expired or is used up; ask for a new one")
)

// passwordlessStart mails a sign-in code to the address and answers 202,
// whatever the address: registered or not, it takes the same work and the
// mail goes out in the background. Within resend_after of the last code
// nothing changes; an address that cannot sign in — an unknown one
// without auto_register, a disabled account — gets its row but no mail.
func (idn *Identity) passwordlessStart(ctx context.Context, req *passwordlessStartRequest) (chok.None, error) {
	if err := idn.throttleStart(ctx); err != nil {
		return chok.None{}, err
	}
	email := normalizeEmail(req.Email)
	user, err := idn.users.Get(ctx, store.Where(where.Eq("email", email)))
	if err != nil && !errors.Is(err, chok.ErrNotFound) {
		return chok.None{}, err
	}
	known := err == nil
	pw := idn.cfg.Passwordless
	code, token, now := newCode(pw.CodeLength), randomSecret(), time.Now().UTC()
	fresh, err := idn.saveLoginCode(ctx, email, code, token, now)
	if err != nil {
		return chok.None{}, err
	}
	if fresh && (known && user.Active || !known && pw.autoRegister()) {
		idn.sendMail(ctx, Mail{Kind: "login-code", To: email, Token: token, Code: code, Link: withToken(pw.MagicLinkURL, token), ExpiresAt: now.Add(pw.CodeTTL)})
	}
	return chok.None{}, nil
}

// throttleStart counts a start against the client's address, in the login
// throttle's table under a name of its own, and refuses the ones over
// login_rate_limit within login_rate_window.
func (idn *Identity) throttleStart(ctx context.Context) error {
	if idn.limiter == nil {
		return nil
	}
	key := limiterKey{Name: "passwordless_ip", Value: chok.ClientIP(ctx)}
	if ok, _ := idn.limiter.check(key); !ok {
		chok.LoggerFrom(ctx).WarnContext(ctx, "identity: passwordless start throttled", "triggered_by", key.Name)
		return chok.ErrRateLimited.Msgf("too many sign-in codes asked for; try again later").With("retry_after", max(int(idn.cfg.LoginRateWindow/time.Second), 1))
	}
	idn.limiter.record(key)
	return nil
}

// saveLoginCode stores a new code and token for email, unless the row
// there is younger than resend_after, and reports whether it did. An
// UPDATE replaces a row old enough, an INSERT that yields to a conflict
// writes a first one: of two concurrent starts one stores its code.
func (idn *Identity) saveLoginCode(ctx context.Context, email, code, token string, now time.Time) (bool, error) {
	pw := idn.cfg.Passwordless
	row := loginCode{
		ID: db.NewRID("lgc"), Email: email, CodeHash: idn.mac(code), TokenHash: idn.mac(token),
		ExpiresAt: now.Add(pw.CodeTTL), CreatedAt: now,
	}
	res := idn.d.Unsafe(ctx).Model(&loginCode{}).Where("email = ? AND created_at <= ?", email, now.Add(-pw.ResendAfter)).
		Updates(map[string]any{"code_hash": row.CodeHash, "token_hash": row.TokenHash, "attempts": 0, "expires_at": row.ExpiresAt, "created_at": now})
	if res.Error != nil {
		return false, fmt.Errorf("identity: store the login code: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		return true, nil
	}
	res = idn.d.Unsafe(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "email"}}, DoNothing: true}).Create(&row)
	if res.Error != nil {
		return false, fmt.Errorf("identity: store the login code: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// passwordlessVerify trades a mailed code, with its address, or the
// magic-link token for the same session a login gives. A wrong code is
// InvalidCode with attempts_left; an expired or used one, one tried
// max_attempts times, or none at all is CodeExpired. Failures count in the
// login throttle, by address and by client.
func (idn *Identity) passwordlessVerify(ctx context.Context, req *passwordlessVerifyRequest) (*tokenResponse, error) {
	byToken := req.Token != ""
	if !byToken && (req.Email == "" || req.Code == "") {
		missing := map[string]string{}
		for name, v := range map[string]string{"email": req.Email, "code": req.Code} {
			if v == "" {
				missing[name] = "required"
			}
		}
		return nil, chok.ErrValidation.Msgf("give email and code, or token").With("fields", missing)
	}
	keys := []limiterKey{{Name: "client_ip", Value: chok.ClientIP(ctx)}}
	if !byToken {
		keys = append(keys, limiterKey{Name: "email", Value: normalizeEmail(req.Email)})
	}
	if idn.limiter != nil {
		if ok, by := idn.limiter.check(keys...); !ok {
			chok.LoggerFrom(ctx).WarnContext(ctx, "identity: passwordless verify throttled", "triggered_by", by)
			return nil, chok.ErrRateLimited.Msgf("too many failed sign-ins; try again later").With("retry_after", max(int(idn.cfg.LoginRateWindow/time.Second), 1))
		}
	}
	failed := func(err error) error {
		if idn.limiter != nil {
			idn.limiter.record(keys...)
		}
		return err
	}
	pw := idn.cfg.Passwordless
	var row loginCode
	q := idn.d.Unsafe(ctx)
	if byToken {
		q = q.Where("token_hash = ?", idn.mac(req.Token))
	} else {
		q = q.Where("email = ?", normalizeEmail(req.Email))
	}
	if err := q.Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, failed(errCodeExpired)
		}
		return nil, fmt.Errorf("identity: read the login code: %w", err)
	}
	now := time.Now()
	if !now.Before(row.ExpiresAt) || row.Attempts >= pw.MaxAttempts {
		return nil, failed(errCodeExpired)
	}
	attemptsLeft := pw.MaxAttempts - row.Attempts
	if !byToken {
		// An attempt is claimed before the comparison, so concurrent
		// guesses get no more than max_attempts comparisons out of a code.
		res := idn.d.Unsafe(ctx).Model(&loginCode{}).Where("id = ? AND attempts < ? AND expires_at > ?", row.ID, pw.MaxAttempts, now.UTC()).
			Update("attempts", gorm.Expr("attempts + 1"))
		if res.Error != nil {
			return nil, fmt.Errorf("identity: count the attempt: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return nil, failed(errCodeExpired)
		}
		attemptsLeft--
		if !hmacEqualHex(idn.macBytes(req.Code), row.CodeHash) {
			return nil, failed(errInvalidCode.With("attempts_left", attemptsLeft))
		}
	}
	user, err := idn.users.Get(ctx, store.Where(where.Eq("email", row.Email)))
	known := err == nil
	if err != nil && !errors.Is(err, chok.ErrNotFound) {
		return nil, err
	}
	// No account, and none to make: as wrong as a wrong code.
	if !known && !pw.autoRegister() {
		return nil, failed(errInvalidCode.With("attempts_left", attemptsLeft))
	}
	if known && !user.Active {
		return nil, errDisabled
	}
	// The code is good once: of two requests with it, one deletes it.
	res := idn.d.Unsafe(ctx).Where("id = ? AND expires_at > ?", row.ID, now.UTC()).Delete(&loginCode{})
	if res.Error != nil {
		return nil, fmt.Errorf("identity: use the login code: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, failed(errCodeExpired)
	}
	if !known {
		if user, err = idn.registerByCode(ctx, row.Email); err != nil {
			return nil, err
		}
	}
	// The code proved the address.
	if !user.EmailVerified {
		if err := idn.users.Update(ctx, store.RID(user.RID), store.Set(map[string]any{"email_verified": true})); err != nil {
			return nil, err
		}
		user.EmailVerified = true
	}
	return idn.issueToken(user, now.Unix())
}

// registerByCode creates the account of an address that proved itself
// with a code: email verified, no password (the hash of a secret nobody
// knows, as for OAuth), no name, no roles. An account registered by
// another route meanwhile is used instead.
func (idn *Identity) registerByCode(ctx context.Context, email string) (*User, error) {
	hash, err := hashPassword(randomSecret(), idn.cost)
	if err != nil {
		return nil, err
	}
	u := &User{Email: email, EmailVerified: true, PasswordHash: hash, HasPassword: false, Active: true}
	err = idn.users.Create(ctx, u)
	if errors.Is(err, chok.ErrConflict) {
		return idn.users.Get(ctx, store.Where(where.Eq("email", email)))
	}
	return u, err
}

// newCode is n random decimal digits.
func newCode(n int) string {
	v, err := rand.Int(rand.Reader, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil))
	if err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return fmt.Sprintf("%0*d", n, v)
}

// macBytes is the HMAC-SHA256 of value under the signing key.
func (idn *Identity) macBytes(value string) []byte {
	h := hmac.New(sha256.New, []byte(idn.cfg.SigningKey))
	h.Write([]byte(value))
	return h.Sum(nil)
}

// mac is macBytes in hex: what auth_login_codes keeps of a code or a
// token.
func (idn *Identity) mac(value string) string { return hex.EncodeToString(idn.macBytes(value)) }

// sweepLoginCodes deletes the expired codes and returns how many.
func (idn *Identity) sweepLoginCodes(ctx context.Context) (int64, error) {
	res := idn.d.Unsafe(ctx).Where("expires_at < ?", time.Now().UTC()).Delete(&loginCode{})
	return res.RowsAffected, res.Error
}

// startSweep deletes the expired codes every loginCodeSweep until Close.
func (idn *Identity) startSweep() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	idn.stopSweep = func() {
		cancel()
		<-done
	}
	go func() {
		defer close(done)
		t := time.NewTicker(loginCodeSweep)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := idn.sweepLoginCodes(ctx); err != nil && ctx.Err() == nil {
					idn.logger.Warn("identity: the login code sweep failed", "error", err)
				} else if n > 0 {
					idn.logger.Info("identity: swept expired login codes", "deleted", n)
				}
			}
		}
	}()
}
