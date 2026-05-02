package account

import (
	"context"
	"errors"
	"fmt"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/store"
)

// UpdateUserRoles replaces the user's role list and bumps PasswordVersion
// in a single transaction. The PV bump invalidates every outstanding
// access token for that user, so the next request after a role change
// forces a re-login that picks up the new "roles" claim.
//
// This is the framework-blessed entry point for role mutation. Direct
// writes through Module.Store() (the public store) are rejected at the
// store layer because "roles" is no longer in its update whitelist.
//
// roles is stored as a comma-separated string via User.SetRoles. Pass
// nil or an empty slice to clear all roles.
func (m *Module) UpdateUserRoles(ctx context.Context, userID string, roles []string) error {
	if userID == "" {
		return apierr.ErrInvalidArgument.WithMessage("userID is required")
	}
	return db.RunInTx(ctx, m.userStore.DB(), func(txCtx context.Context) error {
		user, err := m.userStore.Get(txCtx, store.RID(userID))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apierr.ErrNotFound.WithMessage("user not found")
			}
			return err
		}
		user.SetRoles(roles)
		user.PasswordVersion++
		return m.userStore.Update(txCtx, store.RID(user.RID),
			store.Fields(user, "roles", "password_version").NoLock())
	})
}

// SetUserActive enables or disables an account in a single transaction.
// Disabling bumps PasswordVersion so existing tokens are revoked
// immediately (otherwise a disabled user keeps access until token
// expiry); enabling also bumps PV for symmetry — admins who re-enable
// an account typically want a fresh login session, not stale tokens.
//
// This is the framework-blessed entry point for the active flag. Direct
// writes through Module.Store() are rejected.
func (m *Module) SetUserActive(ctx context.Context, userID string, active bool) error {
	if userID == "" {
		return apierr.ErrInvalidArgument.WithMessage("userID is required")
	}
	return db.RunInTx(ctx, m.userStore.DB(), func(txCtx context.Context) error {
		user, err := m.userStore.Get(txCtx, store.RID(userID))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apierr.ErrNotFound.WithMessage("user not found")
			}
			return err
		}
		user.Active = active
		user.PasswordVersion++
		return m.userStore.Update(txCtx, store.RID(user.RID),
			store.Fields(user, "active", "password_version").NoLock())
	})
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
func (m *Module) BumpPasswordVersion(ctx context.Context, userID string) error {
	if userID == "" {
		return apierr.ErrInvalidArgument.WithMessage("userID is required")
	}
	return db.RunInTx(ctx, m.userStore.DB(), func(txCtx context.Context) error {
		user, err := m.userStore.Get(txCtx, store.RID(userID))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apierr.ErrNotFound.WithMessage("user not found")
			}
			return err
		}
		user.PasswordVersion++
		return m.userStore.Update(txCtx, store.RID(user.RID),
			store.Fields(user, "password_version").NoLock())
	})
}

// MarkEmailVerified flips User.EmailVerified to true. Intended to be
// called by an application after its own email-verification flow
// (link click, magic code) succeeds — chok does not impose a delivery
// mechanism. Idempotent; calling on an already-verified user is a no-op
// returning nil.
//
// Email verification does NOT bump PasswordVersion — it does not affect
// any claim in the JWT, so existing tokens remain valid.
func (m *Module) MarkEmailVerified(ctx context.Context, userID string) error {
	if userID == "" {
		return apierr.ErrInvalidArgument.WithMessage("userID is required")
	}
	user, err := m.userStore.Get(ctx, store.RID(userID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apierr.ErrNotFound.WithMessage("user not found")
		}
		return fmt.Errorf("account: load user: %w", err)
	}
	if user.EmailVerified {
		return nil
	}
	user.EmailVerified = true
	return m.userStore.Update(ctx, store.RID(user.RID),
		store.Fields(user, "email_verified").NoLock())
}
