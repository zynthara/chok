package account

import (
	"context"
	"strings"

	"github.com/zynthara/chok/v2/db"
)

// User is the built-in user model for the account module.
//
// EmailVerified gates the LinkByEmail OAuth auto-merge flow (see
// account-multiprovider SPEC §8). Default false; flipped to true via
// Module.MarkEmailVerified after the application's own email-verification
// flow completes (link click, magic code, etc. — chok does not impose a
// delivery mechanism).
type User struct {
	db.SoftDeleteModel
	Email         string `json:"email"          gorm:"size:200;not null"`
	EmailVerified bool   `json:"email_verified" gorm:"column:email_verified;default:false;not null"`
	PasswordHash  string `json:"-"              gorm:"column:password_hash;size:128;not null"`
	// HasPassword is the source of truth for "this account can authenticate
	// via /login with a password the user knows". /register and
	// changePassword/resetPassword set it true; OAuth-only account
	// creation (random unguessable PasswordHash) leaves it false.
	//
	// Earlier Phase 2 code derived this from `PasswordVersion > 0 ||
	// len(idents) == 0`, which broke as soon as a regular password user
	// linked an OAuth identity (PV stayed 0, idents became 1, the user
	// was misclassified as OAuth-only and locked out of password
	// /login). HasPassword tracks intent explicitly.
	//
	// AccountComponent.Migrate runs an idempotent backfill so legacy
	// rows whose PasswordHash was set by /register pre-fix get
	// HasPassword=true on first migrate.
	HasPassword     bool   `json:"-"              gorm:"column:has_password;default:false;not null"`
	PasswordVersion int    `json:"-"              gorm:"column:password_version;default:0;not null"`
	Name            string `json:"name"           gorm:"size:100;default:'';not null"`
	Roles           string `json:"-"              gorm:"column:roles;size:500;default:'';not null"`
	Active          bool   `json:"-"              gorm:"default:true;not null"`
}

// RIDPrefix returns the prefix for user resource IDs (e.g. "usr_abc123").
func (User) RIDPrefix() string { return "usr" }

// RoleList returns the roles as a string slice.
func (u *User) RoleList() []string {
	if u.Roles == "" {
		return nil
	}
	return strings.Split(u.Roles, ",")
}

// SetRoles stores the given roles as a comma-separated string.
func (u *User) SetRoles(roles []string) {
	u.Roles = strings.Join(roles, ",")
}

// Table returns the migration spec for the User model.
// Use with db.Migrate:
//
//	db.Migrate(ctx, gdb, account.Table(), db.Table(&Product{}))
func Table() db.TableSpec {
	return db.Table(&User{}, db.SoftUnique("uk_user_email", "email"))
}

// MigrateSchema runs AutoMigrate for User + Identity tables and then
// the idempotent has_password backfill. This is the single canonical
// migration path — the account module's Migrator calls it (honouring
// the framework migrate mode), and kernel-less embedders can call it
// directly.
func MigrateSchema(ctx context.Context, h *db.DB) error {
	if err := h.Migrate(ctx, Table(), IdentityTable()); err != nil {
		return err
	}
	return BackfillHasPassword(ctx, h)
}

// BackfillHasPassword populates User.HasPassword for legacy rows that
// pre-date the column. AutoMigrate adds has_password with default
// false, but pre-fix /register left it at the zero value too, so a
// blanket "false" would lock every existing password user out at the
// next /login.
//
// Strategy: any User with no OAuth Identity row and a non-empty
// PasswordHash is, by construction, a legacy password user — set
// has_password=true. Users with Identity rows are left at false; if
// they're actually legacy password users who linked OAuth before the
// fix, they recover by running /forgot-password (which sets
// has_password=true).
//
// Idempotent: WHERE has_password = false guards against re-running.
// Safe to call from AccountComponent.Migrate on every startup.
func BackfillHasPassword(ctx context.Context, h *db.DB) error {
	const sql = `
UPDATE users
SET has_password = TRUE
WHERE has_password = FALSE
  AND password_hash != ''
  AND deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM identities
    WHERE identities.user_id = users.rid
      AND identities.deleted_at IS NULL
  )
`
	return h.Unsafe(ctx).Exec(sql).Error
}
