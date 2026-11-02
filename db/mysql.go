package db

import (
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// mysqlDialector is the dialector of gorm.io/driver/mysql with its
// migrator wrapped in mysqlMigrator. gorm builds every migrator through
// Migrator, including the ones AutoMigrate builds to migrate a column, so
// the wrapper sees each call; every other method is the driver's.
type mysqlDialector struct {
	*mysql.Dialector
}

// Migrator returns the driver's migrator wrapped in mysqlMigrator.
func (d mysqlDialector) Migrator(gdb *gorm.DB) gorm.Migrator {
	return mysqlMigrator{d.Dialector.Migrator(gdb).(mysql.Migrator)}
}

// mysqlMigrator is the driver's migrator with its own MigrateColumnUnique.
type mysqlMigrator struct {
	mysql.Migrator
}

// MigrateColumnUnique is MigrateColumnUnique of gorm.io/driver/mysql v1.6.0
// without its first step. MySQL reports a column unique when a unique
// index covers it alone, and that step then dropped every such index but
// the unique tag's constraint (uni_<table>_<column>) and the uniqueIndex
// tag's index. Each migration of an existing table dropped a one-column
// db.Unique, leaving the table without it until createIndex built it
// again, and lost a one-column unique index made by hand or, under a
// declared name, dropped it before createIndex could check it. chok's
// auto migration drops nothing it did not create, alike on the three
// dialects: gorm's migrator for SQLite and Postgres drops only the unique
// tag's constraint. The unique and uniqueIndex tags migrate as in the
// driver; compare the two functions when upgrading the driver.
func (m mysqlMigrator) MigrateColumnUnique(value any, field *schema.Field, columnType gorm.ColumnType) error {
	unique, ok := columnType.Unique()
	if !ok || field.PrimaryKey {
		return nil // skip primary key
	}

	queryTx, execTx := m.GetQueryAndExecTx()
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		// We're currently only receiving boolean values on `Unique` tag,
		// so the UniqueConstraint name is fixed
		constraint := m.DB.NamingStrategy.UniqueName(stmt.Table, field.DBName)
		if unique {
			hasConstraint := queryTx.Migrator().HasConstraint(value, constraint)
			switch {
			case field.Unique && !hasConstraint:
				if field.Unique {
					if err := execTx.Migrator().CreateConstraint(value, constraint); err != nil {
						return err
					}
				}
			// field isn't Unique but ColumnType's Unique is reported by UniqueConstraint.
			case !field.Unique && hasConstraint:
				if err := execTx.Migrator().DropConstraint(value, constraint); err != nil {
					return err
				}
				if field.UniqueIndex != "" {
					if err := execTx.Migrator().CreateIndex(value, field.UniqueIndex); err != nil {
						return err
					}
				}
			}

			if field.UniqueIndex != "" && !queryTx.Migrator().HasIndex(value, field.UniqueIndex) {
				if err := execTx.Migrator().CreateIndex(value, field.UniqueIndex); err != nil {
					return err
				}
			}
		} else {
			if field.Unique {
				if err := execTx.Migrator().CreateConstraint(value, constraint); err != nil {
					return err
				}
			}
			if field.UniqueIndex != "" && !queryTx.Migrator().HasIndex(value, field.UniqueIndex) {
				if err := execTx.Migrator().CreateIndex(value, field.UniqueIndex); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
