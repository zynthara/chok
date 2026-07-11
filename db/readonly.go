package db

import (
	"regexp"
	"strings"

	"gorm.io/gorm"
)

var lockingSelect = regexp.MustCompile(`(?is)\bFOR\s+(?:NO\s+KEY\s+UPDATE|KEY\s+SHARE|UPDATE|SHARE)\b`)

// registerReadOnlyCallbacks installs the statement-level guard on read-only
// handles. Driver-level read-only settings remain the final defence for code
// that deliberately unwraps gorm to database/sql.
func registerReadOnlyCallbacks(gdb *gorm.DB, readOnly bool) error {
	if !readOnly {
		return nil
	}
	reject := func(db *gorm.DB) { db.AddError(ErrReadOnly) }
	if err := gdb.Callback().Create().Before("*").Register("chok:read_only:create", reject); err != nil {
		return err
	}
	if err := gdb.Callback().Update().Before("*").Register("chok:read_only:update", reject); err != nil {
		return err
	}
	if err := gdb.Callback().Delete().Before("*").Register("chok:read_only:delete", reject); err != nil {
		return err
	}
	if err := gdb.Callback().Raw().Before("*").Register("chok:read_only:raw", reject); err != nil {
		return err
	}
	guardQuery := func(db *gorm.DB) {
		sql := strings.TrimSpace(db.Statement.SQL.String())
		if sql == "" {
			return // ORM query; GORM has not built its SELECT yet.
		}
		if !readOnlyRawSQL(sql) {
			db.AddError(ErrReadOnly)
		}
	}
	if err := gdb.Callback().Query().Before("*").Register("chok:read_only:query", guardQuery); err != nil {
		return err
	}
	if err := gdb.Callback().Row().Before("*").Register("chok:read_only:row", guardQuery); err != nil {
		return err
	}
	return nil
}

func readOnlyRawSQL(sql string) bool {
	sql = strings.TrimSpace(sql)
	if len(sql) < len("select") || !strings.EqualFold(sql[:len("select")], "select") {
		return false
	}
	if len(sql) > len("select") {
		next := sql[len("select")]
		if next != ' ' && next != '\t' && next != '\r' && next != '\n' && next != '(' {
			return false
		}
	}
	return !lockingSelect.MatchString(sql)
}
