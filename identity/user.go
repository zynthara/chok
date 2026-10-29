package identity

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbx"
)

// User is an account. Its ID (the RID, usr_…) is the stable user id: the
// UserID of the chok.Principal Authn stores, and so the owner_id of the
// rows a store fences to the user. The password hash, the token version,
// the roles and the active flag never leave the server.
type User struct {
	db.SoftDeleteModel
	Email string `json:"email" store:"query,update" gorm:"size:200;not null"`
	// EmailVerified records that the address is the user's: a mailed reset
	// token or code proved it, an OAuth provider vouched for it, or the
	// application called UserService.MarkEmailVerified.
	EmailVerified bool `json:"email_verified" store:"query,update" gorm:"column:email_verified;default:false;not null"`
	// EmailAssumed marks an EmailVerified that rests on a provider's word
	// alone: Facebook returns an address without saying it is confirmed.
	// The address serves as verified, but auth.admins grants it nothing,
	// and the first proof of it from outside the account — a mailed reset
	// token or code, a link_by_email sign-in through a provider that states
	// it — takes the account over as for an unverified one. The user keeps
	// the account as it is by confirming the address in a session: a code
	// mailed by POST {prefix}/confirm-email/start, or a linked provider
	// that states the same address.
	EmailAssumed bool `json:"email_assumed" store:"query,update" gorm:"column:email_assumed;default:false;not null"`
	// PasswordHash is the bcrypt hash; an account created through OAuth
	// holds the hash of a random secret nobody knows.
	PasswordHash string `json:"-" store:"update" gorm:"column:password_hash;size:128;not null"`
	// HasPassword records that the user set a password, so the password
	// login is theirs; false for an account created through OAuth until a
	// password reset.
	HasPassword bool `json:"-" store:"update" gorm:"column:has_password;default:false;not null"`
	// PasswordVersion is in every token; bumping it revokes them all.
	PasswordVersion int    `json:"-" store:"update" gorm:"column:password_version;default:0;not null"`
	Name            string `json:"name" store:"query,update" gorm:"size:100;default:'';not null"`
	// Roles is the comma-separated role list; RoleList splits it.
	Roles  string `json:"-" store:"update" gorm:"size:500;default:'';not null"`
	Active bool   `json:"-" store:"query,update" gorm:"default:true;not null"`
}

// RIDPrefix makes user ids usr_….
func (User) RIDPrefix() string { return "usr" }

// emailProven reports that the address is the user's on more than
// Facebook's word: verified, and not merely assumed.
func (u *User) emailProven() bool { return u.EmailVerified && !u.EmailAssumed }

// RoleList returns the roles.
func (u *User) RoleList() []string {
	if u.Roles == "" {
		return nil
	}
	return strings.Split(u.Roles, ",")
}

// linkedIdentity joins a user to an account at an OAuth provider, found
// again by (provider, provider_account_id) at every sign-in. Password
// logins write none.
type linkedIdentity struct {
	db.SoftDeleteModel
	UserID            string     `json:"user_id" store:"query" gorm:"size:32;not null;index:ix_identity_user_provider,priority:1"`
	Provider          string     `json:"provider" store:"query" gorm:"size:32;not null;index:ix_identity_user_provider,priority:2"`
	ProviderAccountID string     `json:"provider_account_id" store:"query" gorm:"size:200;not null"`
	Email             string     `json:"email" store:"update" gorm:"size:200;default:'';not null"`
	Profile           jsonData   `json:"-" store:"update"`
	LastUsedAt        *time.Time `json:"last_used_at" store:"update"`
}

func (linkedIdentity) RIDPrefix() string { return "idn" }

func (linkedIdentity) TableName() string { return "identities" }

// auditRow is one audit_logs record: append-only, so no model base —
// no version, no updated_at, no soft delete. The composite indexes serve
// the three lookups, newest first: an actor's, an action's and a
// resource's history.
type auditRow struct {
	ID         string    `gorm:"primaryKey;size:24"`
	OccurredAt time.Time `gorm:"not null;index;index:idx_audit_actor_time,priority:2;index:idx_audit_action_time,priority:2;index:idx_audit_resource_time,priority:3"`
	ActorID    string    `gorm:"size:128;index:idx_audit_actor_time,priority:1"`
	ActorType  string    `gorm:"size:16"`
	ActorIP    string    `gorm:"size:45"`
	Action     string    `gorm:"size:64;not null;index:idx_audit_action_time,priority:1"`
	Result     string    `gorm:"size:16;not null"`
	Resource   string    `gorm:"size:64;index:idx_audit_resource_time,priority:1"`
	ResourceID string    `gorm:"size:128;index:idx_audit_resource_time,priority:2"`
	BeforeData jsonData  `gorm:"column:before_data"`
	AfterData  jsonData  `gorm:"column:after_data"`
	TraceID    string    `gorm:"size:32;index"`
	RequestID  string    `gorm:"size:128;index"`
	Metadata   jsonData
	Reason     string `gorm:"size:512"`
}

func (auditRow) TableName() string { return "audit_logs" }

// tables are identity's schema: users, identities, audit_logs and
// auth_login_codes.
func tables() []db.TableSpec {
	return []db.TableSpec{
		db.Table(&User{}, db.SoftUnique("uk_user_email", "email")),
		db.Table(&linkedIdentity{}, db.SoftUnique("uk_identity_provider", "provider", "provider_account_id")),
		dbx.PlainTable(&auditRow{}).(db.TableSpec),
		dbx.PlainTable(&loginCode{}).(db.TableSpec),
	}
}

// jsonData is a JSON document column: JSON on MySQL and SQLite, JSONB on
// Postgres; NULL when empty.
type jsonData []byte

// Value writes the document as text, which every dialect casts to its
// JSON type.
func (j jsonData) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	return string(j), nil
}

// Scan reads the document back.
func (j *jsonData) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = append(jsonData(nil), x...)
	case string:
		*j = jsonData(x)
	default:
		return fmt.Errorf("identity: a JSON column holds %T", v)
	}
	return nil
}

// GormDBDataType picks the column type per dialect.
func (jsonData) GormDBDataType(d *gorm.DB, _ *schema.Field) string {
	if d.Name() == "postgres" {
		return "JSONB"
	}
	return "JSON"
}
