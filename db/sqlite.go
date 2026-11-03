package db

import (
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// sqliteDialector is the dialector of github.com/glebarez/sqlite with its
// migrator wrapped in sqliteMigrator, the way mysqlDialector wraps the
// MySQL driver's; every other method is the driver's.
type sqliteDialector struct {
	*sqlite.Dialector
}

// Migrator returns the driver's migrator wrapped in sqliteMigrator.
func (d sqliteDialector) Migrator(gdb *gorm.DB) gorm.Migrator {
	return sqliteMigrator{d.Dialector.Migrator(gdb).(sqlite.Migrator)}
}

// sqliteMigrator is the driver's migrator with its own MigrateColumnUnique.
type sqliteMigrator struct {
	sqlite.Migrator
}

// MigrateColumnUnique is migrateColumnUnique, gorm's with a guard. Auto
// migration drops or rebuilds no index or constraint that chok did not
// create.
func (m sqliteMigrator) MigrateColumnUnique(value any, field *schema.Field, columnType gorm.ColumnType) error {
	return migrateColumnUnique(m.Migrator.Migrator, value, field, columnType)
}
