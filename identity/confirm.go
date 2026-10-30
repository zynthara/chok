package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/store"
)

// A signed-in user confirms the address with a code mailed to it: a proof
// made in the account's own session, so the account stays as it is. The
// routes are for the accounts Facebook makes, whose address is only
// assumed until then.

type confirmEmailRequest struct {
	Code string `json:"code" validate:"required,numeric,max=10"`
}

// A wrong code here is a 400, where passwordless answers 401: the session
// is good, and a front end takes a 401 for its end.
var (
	errConfirmInvalid = chok.NewError(http.StatusBadRequest, "InvalidCode", "the code is not right")
	errConfirmExpired = chok.NewError(http.StatusBadRequest, "CodeExpired", "the code expired or is used up; ask for a new one")
)

// confirms reports whether the confirm-email routes are mounted: with a
// Sender, and a provider that only implies its addresses are verified.
func (idn *Identity) confirms() bool {
	return idn.sender != nil && idn.oauth != nil && idn.oauth.assumesEmail
}

// The user's confirmation code is an auth_login_codes row keyed by the
// user's id, which no address equals, so passwordless sign-in never meets
// it. Its MAC covers the address the code went to as well, so the code
// proves that address alone.
func confirmSecret(email, code string) string { return email + "\n" + code }

// confirmEmailStart is POST {prefix}/confirm-email/start for a signed-in
// user: it mails a code to the user's address and answers 202. Within
// resend_after of the last code nothing changes; the starts count in the
// login throttle, by user.
func (idn *Identity) confirmEmailStart(ctx context.Context, _ *chok.None) (chok.None, error) {
	s, ok := sessionFrom(ctx)
	if !ok {
		return chok.None{}, errNoPrincipal
	}
	u := s.user
	key := limiterKey{Name: "confirm_email", Value: u.RID}
	if idn.limiter != nil {
		if ok, _ := idn.limiter.check(key); !ok {
			chok.LoggerFrom(ctx).WarnContext(ctx, "identity: confirm-email start throttled", "user_id", u.RID)
			return chok.None{}, errConfirmThrottled(idn)
		}
		idn.limiter.record(key)
	}
	pw := idn.cfg.Passwordless
	code, now := newCode(pw.CodeLength), time.Now().UTC()
	fresh, err := idn.saveLoginCode(ctx, u.RID, confirmSecret(u.Email, code), randomSecret(), now)
	if err != nil {
		return chok.None{}, err
	}
	if fresh {
		idn.sendMail(ctx, Mail{Kind: "confirm-email", To: u.Email, Code: code, ExpiresAt: now.Add(pw.CodeTTL)})
	}
	return chok.None{}, nil
}

// confirmEmail is POST {prefix}/confirm-email {code} for a signed-in user:
// the code start mailed proves the address in this session, so the
// address counts as the user's from now on — no longer assumed — and the
// account keeps its password, its providers and its tokens. 204. A wrong
// code is InvalidCode with attempts_left; an expired or used one, one
// tried max_attempts times, or none at all is CodeExpired; both 400.
// Failures count in the login throttle, by user.
func (idn *Identity) confirmEmail(ctx context.Context, req *confirmEmailRequest) (chok.None, error) {
	s, ok := sessionFrom(ctx)
	if !ok {
		return chok.None{}, errNoPrincipal
	}
	u := s.user
	key := limiterKey{Name: "confirm_email", Value: u.RID}
	if idn.limiter != nil {
		if ok, _ := idn.limiter.check(key); !ok {
			chok.LoggerFrom(ctx).WarnContext(ctx, "identity: confirm-email throttled", "user_id", u.RID)
			return chok.None{}, errConfirmThrottled(idn)
		}
	}
	failed := func(err error) error {
		if idn.limiter != nil {
			idn.limiter.record(key)
		}
		return err
	}
	var row loginCode
	if err := idn.d.Unsafe(ctx).Where("email = ?", u.RID).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return chok.None{}, failed(errConfirmExpired)
		}
		return chok.None{}, fmt.Errorf("identity: read the confirmation code: %w", err)
	}
	pw := idn.cfg.Passwordless
	now := time.Now()
	if !now.Before(row.ExpiresAt) || row.Attempts >= pw.MaxAttempts {
		return chok.None{}, failed(errConfirmExpired)
	}
	claimed, err := idn.claimAttempt(ctx, &row, now)
	if err != nil {
		return chok.None{}, err
	}
	if !claimed {
		return chok.None{}, failed(errConfirmExpired)
	}
	if !hmacEqualHex(idn.macBytes(confirmSecret(u.Email, req.Code)), row.CodeHash) {
		return chok.None{}, failed(errConfirmInvalid.With("attempts_left", pw.MaxAttempts-row.Attempts-1))
	}
	spent, err := idn.spendCode(ctx, &row, now)
	if err != nil {
		return chok.None{}, err
	}
	if !spent {
		return chok.None{}, failed(errConfirmExpired)
	}
	if u.emailProven() {
		return chok.None{}, nil
	}
	return chok.None{}, idn.users.Update(ctx, store.RID(u.RID), store.Set(map[string]any{"email_verified": true, "email_assumed": false}))
}

// errConfirmThrottled is the 429 of a user over the login throttle.
func errConfirmThrottled(idn *Identity) error {
	return chok.ErrRateLimited.Msgf("too many confirmation codes; try again later").With("retry_after", max(int(idn.cfg.LoginRateWindow/time.Second), 1))
}
