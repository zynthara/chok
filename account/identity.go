package account

import (
	"time"

	"gorm.io/datatypes"

	"github.com/zynthara/chok/v2/db"
)

// Identity is the OAuth-only join row between a chok User and an external
// provider account. Created lazily on first OAuth login and looked up on
// every subsequent login by (Provider, ProviderAccountID).
//
// Password authentication does NOT write Identity rows — User.PasswordHash
// is the source of truth for password login. Identity is purely the OAuth
// side of the multi-provider story (SPEC §4.2).
type Identity struct {
	db.SoftDeleteModel
	UserID            string         `json:"user_id"             gorm:"index;size:32;not null"`
	Provider          string         `json:"provider"            gorm:"index:ix_identity_user_provider,priority:2;size:32;not null"`
	ProviderAccountID string         `json:"provider_account_id" gorm:"size:200;not null"`
	Email             string         `json:"email,omitempty"     gorm:"size:200;default:'';not null"`
	Profile           datatypes.JSON `json:"-"                   gorm:"type:json"`
	LastUsedAt        time.Time      `json:"last_used_at,omitempty"`
}

// RIDPrefix returns the prefix for Identity resource IDs (e.g. "idn_abc123").
func (Identity) RIDPrefix() string { return "idn" }

// IdentityTable returns the migration spec for the Identity model.
// The unique constraint (provider, provider_account_id) is the canonical
// "this IdP account belongs to exactly one chok user" guard. Soft-delete
// aware so an unlinked-then-relinked identity does not collide with its
// own tombstone.
func IdentityTable() db.TableSpec {
	return db.Table(&Identity{},
		db.SoftUnique("uk_identity_provider", "provider", "provider_account_id"),
	)
}
