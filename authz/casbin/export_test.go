package casbin

import (
	"github.com/casbin/casbin/v3/persist"
	"gorm.io/gorm"
)

// NewAdapterForTest exposes the package-private persistence boundary only to
// external tests, which can then import the parent authz migration descriptor
// without creating an authz/casbin -> authz import cycle.
func NewAdapterForTest(gdb *gorm.DB) (persist.Adapter, error) {
	return newGormAdapter(gdb)
}
