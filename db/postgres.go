package db

import (
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// postgresDialector is the dialector of gorm.io/driver/postgres with its
// migrator wrapped in postgresMigrator, the way mysqlDialector wraps the
// MySQL driver's; every other method is the driver's.
type postgresDialector struct {
	*postgres.Dialector
}

// Migrator returns the driver's migrator wrapped in postgresMigrator.
func (d postgresDialector) Migrator(gdb *gorm.DB) gorm.Migrator {
	return postgresMigrator{d.Dialector.Migrator(gdb).(postgres.Migrator)}
}

// postgresMigrator is the driver's migrator with its own MigrateColumnUnique.
type postgresMigrator struct {
	postgres.Migrator
}

// MigrateColumnUnique is migrateColumnUnique, gorm's with a guard. Auto
// migration drops or rebuilds no index or constraint that chok did not
// create.
func (m postgresMigrator) MigrateColumnUnique(value any, field *schema.Field, columnType gorm.ColumnType) error {
	return migrateColumnUnique(m.Migrator.Migrator, value, field, columnType)
}
