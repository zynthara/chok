package db

import (
	"gorm.io/gorm"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

// migrateColumnUnique is MigrateColumnUnique of gorm.io/gorm v1.31.2's
// migrator, which the Postgres and SQLite drivers use, with a guard: it
// drops the unique tag's constraint (uni_<table>_<column>) only when the
// table has it. gorm drops it whenever the column is unique and its field
// has no unique tag, but a constraint of another name, made by hand or by
// an earlier schema, may be what makes the column unique. Postgres then
// failed Open with SQLSTATE 42704, as there was no such constraint to
// drop. SQLite, which drops a constraint by rebuilding the table, rebuilt
// it on every Open: an index made by hand on it was lost, and the DROP
// TABLE of the rebuild fired the foreign keys of the tables that refer to
// it, deleting their rows where the key cascades and failing Open where
// it takes the default action. Auto migration drops or rebuilds no index
// or constraint that chok did not create; db/mysql.go keeps the rule for
// MySQL. The unique tag migrates as in gorm; compare the two functions
// when upgrading gorm.
func migrateColumnUnique(m migrator.Migrator, value any, field *schema.Field, columnType gorm.ColumnType) error {
	unique, ok := columnType.Unique()
	if !ok || field.PrimaryKey {
		return nil // skip primary key
	}
	// By default, ColumnType's Unique is not affected by UniqueIndex, so we don't care about UniqueIndex.
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		// We're currently only receiving boolean values on `Unique` tag,
		// so the UniqueConstraint name is fixed
		constraint := m.DB.NamingStrategy.UniqueName(stmt.Table, field.DBName)
		if unique && !field.Unique {
			if !m.DB.Migrator().HasConstraint(value, constraint) {
				return nil // unique by a constraint chok did not make: keep it
			}
			return m.DB.Migrator().DropConstraint(value, constraint)
		}
		if !unique && field.Unique {
			return m.DB.Migrator().CreateConstraint(value, constraint)
		}
		return nil
	})
}
