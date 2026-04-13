package account

import (
	"strings"

	"github.com/zynthara/chok/db"
)

// User is the built-in user model for the account module.
type User struct {
	db.SoftDeleteModel
	Email        string `json:"email"    gorm:"size:200;not null"`
	PasswordHash string `json:"-"        gorm:"column:password_hash;size:128;not null"`
	Name         string `json:"name"     gorm:"size:100;default:'';not null"`
	Roles        string `json:"-"        gorm:"column:roles;size:500;default:'';not null"`
	Active       bool   `json:"-"        gorm:"default:true;not null"`
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
//	db.Migrate(gdb, account.Table(), db.Table(&Product{}))
func Table() db.TableSpec {
	return db.Table(&User{}, db.SoftUnique("uk_user_email", "email"))
}
