package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

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

// sqliteMigrator is the driver's migrator with its own MigrateColumnUnique,
// and with the four methods that rebuild a table run through rebuilding.
type sqliteMigrator struct {
	sqlite.Migrator
}

// MigrateColumnUnique is migrateColumnUnique, gorm's with a guard. Auto
// migration drops or rebuilds no index or constraint that chok did not
// create.
func (m sqliteMigrator) MigrateColumnUnique(value any, field *schema.Field, columnType gorm.ColumnType) error {
	return migrateColumnUnique(m.Migrator.Migrator, value, field, columnType)
}

// AlterColumn runs the driver's through rebuilding, on value's table.
func (m sqliteMigrator) AlterColumn(value any, field string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		return m.rebuilding(stmt.Table, func() error { return m.Migrator.AlterColumn(value, field) })
	})
}

// DropColumn runs the driver's through rebuilding, on value's table.
func (m sqliteMigrator) DropColumn(value any, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		return m.rebuilding(stmt.Table, func() error { return m.Migrator.DropColumn(value, name) })
	})
}

// CreateConstraint runs the driver's through rebuilding, on the table the
// driver rebuilds: the constraint's, found as the driver finds it with
// GuessConstraintInterfaceAndTable. For the foreign key of a has-one or
// has-many relation that is the table holding the key, which need not be
// value's. A foreign key added while foreign keys are on must then hold
// for the rows the rebuild copied: checkForeignKey.
func (m sqliteMigrator) CreateConstraint(value any, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		constraint, table := m.GuessConstraintInterfaceAndTable(stmt, name)
		return m.withoutForeignKeys(func(on bool) error {
			if err := m.rebuilding(table, func() error { return m.Migrator.CreateConstraint(value, name) }); err != nil {
				return err
			}
			if fk, ok := constraint.(*schema.Constraint); ok && on {
				return m.checkForeignKey(value, name, table, fk)
			}
			return nil
		})
	})
}

// DropConstraint runs the driver's through rebuilding, on the table the
// driver rebuilds, found as for CreateConstraint.
func (m sqliteMigrator) DropConstraint(value any, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		_, table := m.GuessConstraintInterfaceAndTable(stmt, name)
		return m.rebuilding(table, func() error { return m.Migrator.DropConstraint(value, name) })
	})
}

// rebuilding runs rebuild, the driver's AlterColumn, DropColumn,
// CreateConstraint or DropConstraint (glebarez/sqlite v1.11.0). SQLite
// cannot alter a column or a constraint in place, so each rebuilds table:
// in one transaction it creates <table>__temp with the new DDL, copies the
// rows into it, drops table and renames the copy. DROP TABLE drops the
// indexes and triggers of table with it, and AutoMigrate and createIndex
// made only their own indexes again: an index or a trigger made by hand
// was lost. With foreign keys on, as chok opens SQLite, DROP TABLE also
// deletes the rows first, which fires the foreign keys of the tables that
// refer to table: their rows were deleted where the key cascades, and the
// rebuild failed (787) where it takes the default action. The driver
// turns the keys off for AlterColumn alone.
//
// rebuilding reads the indexes and triggers of table that have SQL, runs
// rebuild with foreign keys off, then makes again from that SQL, in the
// order the catalog held them, each one the rebuild dropped. One it cannot
// make again, such as an index over a column that DropColumn dropped,
// fails the call rather than vanish without a word; the ones after it are
// still made again, and the call returns every such error joined. The
// indexes without SQL are those of UNIQUE and PRIMARY KEY clauses, which
// come back with the DDL of the table. So, as on the dialects that alter a
// table in place, auto migration drops or rebuilds no index or constraint
// that chok did not create, and fires no foreign key action. With the keys
// off, the rows copied are not checked against them either;
// CreateConstraint checks them against a foreign key it adds, as those
// dialects do.
//
// CREATE TRIGGER does not check the body of a trigger, so each trigger
// made again is compiled with the statements that fire it, by
// compileTriggers. One that does not compile, such as a trigger whose
// body uses a column that DropColumn dropped, is dropped again and fails
// the call, as SQLite's own ALTER TABLE DROP COLUMN refuses to drop that
// column. Left in place, it failed each statement that fires it and each
// later rebuild of any table, whose rename checks every trigger of the
// schema. A trigger of UPDATE OF a dropped column whose body does not use
// the column is fired by no statement, so nothing compiles it: it is made
// again as it was and never fires. Should a trigger that does not compile
// fail to be dropped again, each later check would fail on it, so the
// triggers made after it are not checked, and its error says so.
//
// A view that refers to table fails the rebuild, and so does a trigger of
// another table that refers to it: renaming the copy checks every view and
// trigger of the schema while table is not there, so the driver's
// transaction rolls back and the call fails. SQLite's own steps for
// changing a table drop such a view first and make it again afterwards;
// rebuilding leaves that to the application on purpose, and does not set
// PRAGMA legacy_alter_table either, which lets the rename through and
// leaves the view broken without a word.
//
// The rebuild commits before the indexes and triggers are made again: a
// process that stops in between leaves table without them, and no later
// Open makes again the ones made by hand.
func (m sqliteMigrator) rebuilding(table string, rebuild func() error) error {
	before, err := m.objectSQL(table)
	if err != nil {
		return err
	}
	return m.withoutForeignKeys(func(bool) error {
		if err := rebuild(); err != nil || len(before) == 0 {
			return err
		}
		after, err := m.objectSQL(table)
		if err != nil {
			return err
		}
		var errs []error
		check := true // whether a trigger made again is compiled
		for _, o := range before {
			if slices.ContainsFunc(after, func(a sqliteObject) bool { return a.kind == o.kind && strings.EqualFold(a.name, o.name) }) {
				continue
			}
			// Each error about o starts with what the rebuild dropped and
			// the SQL that makes it again.
			dropped := fmt.Sprintf("rebuilding table %s dropped %s %s, and %s", table, o.kind, o.name, o.sql)
			if err := m.DB.Exec(o.sql).Error; err != nil {
				errs = append(errs, fmt.Errorf("%s failed: %w", dropped, err))
				continue
			}
			if o.kind != "trigger" || !check {
				continue
			}
			compileErr := m.compileTriggers(table)
			if compileErr == nil {
				continue
			}
			if err := m.DB.Exec("DROP TRIGGER " + quote("sqlite", o.name)).Error; err != nil {
				check = false
				errs = append(errs, fmt.Errorf("%s makes it again, but it does not compile (%w), and dropping it again failed, so it is on the table and the triggers made after it are not checked: %w", dropped, compileErr, err))
				continue
			}
			errs = append(errs, fmt.Errorf("%s makes it again, but it does not compile, so it is dropped again: %w", dropped, compileErr))
		}
		return errors.Join(errs...)
	})
}

// compileTriggers compiles, without running them, an INSERT, an UPDATE of
// every column and a DELETE on table, and so each trigger of table that
// such a statement fires: SQLite checks the body of a trigger when it
// compiles a statement that fires it, not when CREATE TRIGGER makes it.
// Generated columns cannot be set and stay out of the UPDATE.
func (m sqliteMigrator) compileTriggers(table string) error {
	var columns []string
	if err := m.DB.Raw("SELECT name FROM pragma_table_xinfo(?) WHERE hidden = 0", table).Scan(&columns).Error; err != nil {
		return fmt.Errorf("read the columns of table %s: %w", table, err)
	}
	set := make([]string, len(columns))
	for i, c := range columns {
		set[i] = quote("sqlite", c) + " = " + quote("sqlite", c)
	}
	t := quote("sqlite", table)
	for _, q := range []string{
		"EXPLAIN INSERT INTO " + t + " DEFAULT VALUES",
		"EXPLAIN UPDATE " + t + " SET " + strings.Join(set, ", "),
		"EXPLAIN DELETE FROM " + t,
	} {
		if err := m.DB.Exec(q).Error; err != nil {
			return err
		}
	}
	return nil
}

// checkForeignKey keeps foreign key name, which CreateConstraint has just
// added to table with foreign keys off, only when every row of table has
// the parent row it refers to, as Postgres (23503) and MySQL (1452) refuse
// to add a key that a row breaks. SQLite's own steps for changing a table
// run PRAGMA foreign_key_check before the commit, but the driver has
// committed: when rows break the key, it is dropped again, by another
// rebuild, and the call fails naming the table, the rows, the parent
// table and the key, so every Open fails until the data is fixed, as on
// those dialects. Only the rows that refer to the key's parent table
// count: rows that break another key of the table, to another table, do
// not stop this one.
func (m sqliteMigrator) checkForeignKey(value any, name, table string, fk *schema.Constraint) error {
	parent := fk.ReferenceSchema.Table
	var n int64
	err := m.DB.Raw("SELECT count(*) FROM pragma_foreign_key_check(?) WHERE parent = ? COLLATE NOCASE", table, parent).Scan(&n).Error
	switch {
	case err != nil:
		err = fmt.Errorf("check the rows of table %s against foreign key %s: %w", table, name, err)
	case n == 0:
		return nil
	case n == 1:
		err = fmt.Errorf("table %s has 1 row that refers to no row of %s, so foreign key %s was not added: fix the data and start again", table, parent, name)
	default:
		err = fmt.Errorf("table %s has %d rows that refer to no row of %s, so foreign key %s was not added: fix the data and start again", table, n, parent, name)
	}
	if dropErr := m.rebuilding(table, func() error { return m.Migrator.DropConstraint(value, name) }); dropErr != nil {
		return errors.Join(err, fmt.Errorf("dropping foreign key %s again failed, so it is in table %s: %w", name, table, dropErr))
	}
	return err
}

// withoutForeignKeys runs fn with foreign keys off and tells it whether
// they were on: only then does it turn them off, and on again after fn,
// failing the call when they do not go on. It is the driver's
// RunWithoutForeignKey with that answer and with its errors. PRAGMA
// foreign_keys applies to its connection and does nothing inside a
// transaction. Auto migration runs outside one, on the one connection of
// the SQLite writer, so the keys stay off through the rebuild's own
// transaction; called inside a transaction, they stay on.
//
// The keys go on again under the migration's context without its
// cancellation, as cleanup does: under a context that had ended, by
// cancellation or by the migration timeout, PRAGMA foreign_keys = ON did
// not run, and the writer went on without foreign keys for as long as the
// handle lived.
func (m sqliteMigrator) withoutForeignKeys(fn func(on bool) error) (err error) {
	var on int
	if err := m.DB.Raw("PRAGMA foreign_keys").Scan(&on).Error; err != nil {
		return fmt.Errorf("read PRAGMA foreign_keys: %w", err)
	}
	if on != 1 {
		return fn(false)
	}
	if err := m.DB.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
		return fmt.Errorf("turn foreign keys off: %w", err)
	}
	defer func() {
		cleanup := m.DB.Session(&gorm.Session{Context: context.WithoutCancel(m.DB.Statement.Context)})
		if onErr := cleanup.Exec("PRAGMA foreign_keys = ON").Error; onErr != nil {
			err = errors.Join(err, fmt.Errorf("turn foreign keys on again: %w", onErr))
		}
	}()
	return fn(true)
}

// sqliteObject is an index or a trigger of the SQLite catalog, its kind
// (index or trigger, the catalog's type) and the SQL that made it.
type sqliteObject struct{ kind, name, sql string }

// objectSQL reads the indexes and triggers of table that have SQL, in the
// order the catalog holds them. The catalog names the table of a trigger
// as its CREATE TRIGGER spelled it, so the name matches in any case.
func (m sqliteMigrator) objectSQL(table string) ([]sqliteObject, error) {
	rows, err := m.DB.Raw("SELECT type, name, sql FROM sqlite_master WHERE type IN ('index', 'trigger') AND tbl_name = ? COLLATE NOCASE AND sql IS NOT NULL ORDER BY rowid", table).Rows()
	if err != nil {
		return nil, fmt.Errorf("read the indexes and triggers of table %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []sqliteObject
	for rows.Next() {
		var o sqliteObject
		if err := rows.Scan(&o.kind, &o.name, &o.sql); err != nil {
			return nil, fmt.Errorf("read the indexes and triggers of table %s: %w", table, err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the indexes and triggers of table %s: %w", table, err)
	}
	return out, nil
}
