package account

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/store"
)

// Role naming constraints for UpdateUserRoles. The User.Roles column is a
// comma-separated string (account/model.go), so a comma in a role name
// silently splits into two roles after a round-trip through RoleList —
// validateRoles rejects that and a few neighbouring shapes that would
// produce nonsense or DB errors.
const (
	roleMaxLen      = 32  // single role name
	rolesTotalCSV   = 500 // matches User.Roles `gorm:"size:500"`
	roleForbidChars = "," // comma is the CSV separator
)

func validateRoles(roles []string) error {
	totalCSV := 0
	for i, r := range roles {
		if r == "" {
			return apierr.ErrInvalidArgument.WithMessage("role cannot be empty")
		}
		if strings.ContainsAny(r, roleForbidChars) {
			return apierr.ErrInvalidArgument.WithMessage("role cannot contain comma: " + r)
		}
		if len(r) > roleMaxLen {
			return apierr.ErrInvalidArgument.WithMessage(fmt.Sprintf("role too long (max %d chars): %q", roleMaxLen, r))
		}
		if i > 0 {
			totalCSV++ // separator
		}
		totalCSV += len(r)
	}
	if totalCSV > rolesTotalCSV {
		return apierr.ErrInvalidArgument.WithMessage(fmt.Sprintf("roles joined length exceeds %d", rolesTotalCSV))
	}
	return nil
}

// UpdateUserRoles replaces the user's role list and bumps PasswordVersion
// in a single atomic UPDATE. The PV bump invalidates every outstanding
// access token for that user, so the next request after a role change
// forces a re-login that picks up the new "roles" claim.
//
// This is the framework-blessed entry point for role mutation. Direct
// writes through Module.Store() (the public store) are rejected at the
// store layer because "roles" is no longer in its update whitelist.
//
// roles is stored as a comma-separated string (User.SetRoles encoding).
// Pass nil or an empty slice to clear all roles. Each role MUST be
// non-empty, comma-free, and at most 32 chars; the joined CSV MUST fit
// in 500 chars (the DB column size).
//
// Concurrency: the UPDATE is atomic at the DB level — `password_version
// = password_version + 1` is computed by the engine, so two concurrent
// callers each get a distinct increment. The roles column itself is
// last-write-wins between concurrent admins (consistent with intent;
// roles are an admin-set value, not a derived counter).
func (m *Service) UpdateUserRoles(ctx context.Context, userID string, roles []string) error {
	if userID == "" {
		return apierr.ErrInvalidArgument.WithMessage("userID is required")
	}
	if err := validateRoles(roles); err != nil {
		return err
	}

	rolesCSV := strings.Join(roles, ",")
	err := m.userStore.Update(ctx, store.RID(userID), store.Set(map[string]any{
		"roles":            rolesCSV,
		"password_version": gorm.Expr("password_version + 1"),
	}))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apierr.ErrNotFound.WithMessage("user not found")
		}
		return err
	}
	return nil
}

// SetUserActive enables or disables an account in a single atomic UPDATE.
// Disabling bumps PasswordVersion so existing tokens are revoked
// immediately (otherwise a disabled user keeps access until token
// expiry); enabling also bumps PV for symmetry — admins who re-enable
// an account typically want a fresh login session, not stale tokens.
//
// This is the framework-blessed entry point for the active flag. Direct
// writes through Module.Store() are rejected.
//
// Concurrency: same atomicity guarantees as UpdateUserRoles. Two
// concurrent SetUserActive calls each produce a distinct PV increment;
// the active value is last-write-wins (admin intent is the source of
// truth, not a prior read).
func (m *Service) SetUserActive(ctx context.Context, userID string, active bool) error {
	if userID == "" {
		return apierr.ErrInvalidArgument.WithMessage("userID is required")
	}
	err := m.userStore.Update(ctx, store.RID(userID), store.Set(map[string]any{
		"active":           active,
		"password_version": gorm.Expr("password_version + 1"),
	}))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apierr.ErrNotFound.WithMessage("user not found")
		}
		return err
	}
	return nil
}

// BumpPasswordVersion increments PasswordVersion without touching any
// other column. It is the general escape hatch for token revocation
// when no single business field changed:
//
//   - revoking a leaked API key (no user-table column to flip)
//   - forced logout-everywhere triggered by a security alert
//   - rotating session secrets after a key compromise
//
// Routine flows (password change, role change, disable) bump PV through
// their dedicated APIs; reach for BumpPasswordVersion only when none of
// those describe what you mean.
//
// Concurrency: implemented as `UPDATE ... SET password_version =
// password_version + 1`, computed atomically by the DB engine. N
// concurrent callers produce N distinct increments — every bump lands,
// no lost updates regardless of isolation level.
func (m *Service) BumpPasswordVersion(ctx context.Context, userID string) error {
	if userID == "" {
		return apierr.ErrInvalidArgument.WithMessage("userID is required")
	}
	err := m.userStore.Update(ctx, store.RID(userID), store.Set(map[string]any{
		"password_version": gorm.Expr("password_version + 1"),
	}))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apierr.ErrNotFound.WithMessage("user not found")
		}
		return err
	}
	return nil
}

// MarkEmailVerified flips User.EmailVerified to true. Intended to be
// called by an application after its own email-verification flow
// (link click, magic code) succeeds — chok does not impose a delivery
// mechanism. Idempotent; calling on an already-verified user is a no-op
// returning nil.
//
// Email verification does NOT bump PasswordVersion — it does not affect
// any claim in the JWT, so existing tokens remain valid.
//
// Implementation: a single UPDATE attempts the write; on ErrNotFound we
// re-read to disambiguate "user truly missing" from "row exists but
// already verified". The fallback exists because MySQL's default driver
// (`db/db.go` does not set `clientFoundRows`) reports `RowsAffected=0`
// for a no-op UPDATE — store.finalizeUpdate then maps that to ErrNotFound,
// which would surface as a spurious 404 to a concurrent caller losing
// the race or to anyone calling on an already-verified user. SQLite
// reports matched-rows so this path is MySQL-specific in practice.
func (m *Service) MarkEmailVerified(ctx context.Context, userID string) error {
	if userID == "" {
		return apierr.ErrInvalidArgument.WithMessage("userID is required")
	}
	err := m.userStore.Update(ctx, store.RID(userID), store.Set(map[string]any{
		"email_verified": true,
	}))
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	// RowsAffected=0 — could be a truly missing user or a MySQL row that
	// is already verified. Re-read to tell them apart.
	u, getErr := m.userStore.Get(ctx, store.RID(userID))
	if getErr != nil {
		if errors.Is(getErr, store.ErrNotFound) {
			return apierr.ErrNotFound.WithMessage("user not found")
		}
		return fmt.Errorf("account: re-read user: %w", getErr)
	}
	if u.EmailVerified {
		return nil
	}
	// Row exists, not verified, yet UPDATE missed — defensively surface
	// rather than silently lying. In practice this is unreachable: the
	// only way to reach here is a soft-delete in the same window, which
	// the re-read would also miss (returning ErrNotFound above).
	return apierr.ErrInternal.WithMessage("MarkEmailVerified: update did not affect any row")
}
