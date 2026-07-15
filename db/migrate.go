package db

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"gorm.io/gorm"
)

// TableSpec holds migration information for a single model.
type TableSpec struct {
	model   any
	indexes []SoftIndex
	soft    bool // true if model embeds SoftDeleteModel
}

// SoftIndex represents a composite unique index that includes delete_token
// for soft-delete compatibility.
type SoftIndex struct {
	Name    string
	Columns []string
}

// Table constructs a TableSpec. Panics if model metadata is invalid
// (does not embed db.Model, illegal RIDPrefix, etc.) — fail-fast at Setup.
func Table(model any, indexes ...SoftIndex) TableSpec {
	if err := ValidateModel(model); err != nil {
		panic(fmt.Sprintf("db.Table: %v", err))
	}
	return TableSpec{
		model:   model,
		indexes: indexes,
		soft:    IsSoftDeleteModel(model),
	}
}

// SoftUnique declares a composite unique index including delete_token.
// The generated index is: UNIQUE(col1, col2, ..., delete_token).
func SoftUnique(name string, columns ...string) SoftIndex {
	return SoftIndex{Name: name, Columns: columns}
}

// Migrate performs AutoMigrate and creates SoftUnique indexes.
//
// SoftDeleteModel supports both uniqueIndex (permanent, survives soft delete)
// and SoftUnique (released on soft delete). Choose per field.
//
// Validation order (fail-fast across the complete specs slice):
//  1. SoftUnique used on non-SoftDeleteModel → error
//  2. SoftUnique columns must be NOT NULL → error for pointer/sql.Null* types or missing "not null" tag
//  3. Only after every spec passes: AutoMigrate, then create its SoftUnique indexes
//
// ctx flows into every DDL statement (AutoMigrate, Raw, Exec) via
// gdb.WithContext so that registry Init timeouts and shutdown cancellation
// can abort long-running migrations instead of blocking startup.
func Migrate(ctx context.Context, gdb *gorm.DB, specs ...TableSpec) error {
	gdb = gdb.WithContext(ctx)
	// Validate the complete declaration set before the first DDL statement.
	// This keeps a typo in a later spec from leaving an avoidable prefix of
	// AutoMigrate changes behind.
	for _, spec := range specs {
		if len(spec.indexes) > 0 && !spec.soft {
			return fmt.Errorf("db.Migrate: SoftUnique is only valid for SoftDeleteModel, "+
				"model %T does not embed SoftDeleteModel", spec.model)
		}
		for _, idx := range spec.indexes {
			if err := validateSoftUniqueColumns(gdb, spec.model, idx); err != nil {
				return err
			}
		}
	}

	for _, spec := range specs {
		if err := gdb.AutoMigrate(spec.model); err != nil {
			return fmt.Errorf("db.Migrate: AutoMigrate %T: %w", spec.model, err)
		}

		for _, idx := range spec.indexes {
			if err := createSoftUniqueIndex(gdb, spec.model, idx); err != nil {
				return fmt.Errorf("db.Migrate: create index %s: %w", idx.Name, err)
			}
		}
	}
	return nil
}

// validateSoftUniqueColumns ensures all columns in a SoftUnique index are NOT NULL.
func validateSoftUniqueColumns(gdb *gorm.DB, model any, idx SoftIndex) error {
	stmt := &gorm.Statement{DB: gdb}
	if err := stmt.Parse(model); err != nil {
		return fmt.Errorf("db.Migrate: parse model %T: %w", model, err)
	}
	t := stmt.Schema.ModelType

	for _, col := range idx.Columns {
		field := stmt.Schema.FieldsByDBName[col]
		if field == nil {
			return fmt.Errorf("db.Migrate: SoftUnique %q references column %q not found in %s",
				idx.Name, col, t.Name())
		}

		// Pointer types are nullable.
		if field.FieldType.Kind() == reflect.Ptr {
			return fmt.Errorf("db.Migrate: SoftUnique column %q in %s is a pointer type (nullable); "+
				"SoftUnique columns must be NOT NULL", col, t.Name())
		}

		// sql.Null* types are nullable.
		typeName := field.FieldType.Name()
		if strings.HasPrefix(typeName, "Null") && field.FieldType.PkgPath() == "database/sql" {
			return fmt.Errorf("db.Migrate: SoftUnique column %q in %s uses sql.%s (nullable); "+
				"SoftUnique columns must be NOT NULL", col, t.Name(), typeName)
		}

		if !field.NotNull {
			return fmt.Errorf("db.Migrate: SoftUnique column %q in %s is missing 'not null' gorm tag; "+
				"SoftUnique columns must be NOT NULL", col, t.Name())
		}
	}
	return nil
}

// createSoftUniqueIndex creates the dialect-appropriate unique index
// backing SoftUnique semantics ("unique among live rows"):
//
//   - Postgres: a partial unique index over the declared columns with
//     WHERE deleted_at IS NULL — soft-deleted rows leave the index
//     entirely, so the delete_token column is not part of the key
//     (SPEC §5.3, M3).
//   - MySQL / SQLite / others: composite UNIQUE(cols..., delete_token)
//     — live rows share the empty delete_token and conflict; a soft
//     delete rewrites the token to a fresh RID, releasing the slot (v1
//     mechanism, unchanged).
//
// Both shapes yield the same observable behaviour: two live rows with
// equal values conflict; a soft-deleted row frees the value; deleted
// rows never conflict with each other.
func createSoftUniqueIndex(gdb *gorm.DB, model any, idx SoftIndex) error {
	stmt := &gorm.Statement{DB: gdb}
	if err := stmt.Parse(model); err != nil {
		return err
	}
	tableName := stmt.Schema.Table

	dialect := gdb.Dialector.Name()

	qTable := quoteIdent(tableName, dialect)
	qIndex := quoteIdent(idx.Name, dialect)

	if dialect == "postgres" {
		cols := make([]string, 0, len(idx.Columns))
		for _, c := range idx.Columns {
			cols = append(cols, quoteIdent(c, dialect))
		}
		return gdb.Exec(fmt.Sprintf(
			"CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s (%s) WHERE %s IS NULL",
			qIndex, qTable, strings.Join(cols, ", "), quoteIdent("deleted_at", dialect),
		)).Error
	}

	cols := make([]string, 0, len(idx.Columns)+1)
	for _, c := range idx.Columns {
		cols = append(cols, quoteIdent(c, dialect))
	}
	cols = append(cols, quoteIdent("delete_token", dialect))
	colList := strings.Join(cols, ", ")

	switch dialect {
	case "mysql":
		// MySQL: no IF NOT EXISTS for CREATE INDEX.
		// Check information_schema first, then create if absent.
		var count int64
		if err := gdb.Raw(
			"SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?",
			tableName, idx.Name,
		).Scan(&count).Error; err != nil {
			return fmt.Errorf("check index %s: %w", idx.Name, err)
		}
		if count > 0 {
			return nil // index already exists
		}
		// The check-then-create pattern races with concurrent migrations
		// (e.g. two workers starting simultaneously). Tolerate MySQL's
		// "duplicate key name" error (1061) which indicates the other
		// worker won; otherwise surface.
		if err := gdb.Exec(fmt.Sprintf("CREATE UNIQUE INDEX %s ON %s (%s)",
			qIndex, qTable, colList)).Error; err != nil {
			if strings.Contains(err.Error(), "Error 1061") ||
				strings.Contains(err.Error(), "duplicate key name") {
				return nil
			}
			return err
		}
		return nil
	default:
		// SQLite and others: IF NOT EXISTS is safe.
		return gdb.Exec(fmt.Sprintf("CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s (%s)",
			qIndex, qTable, colList)).Error
	}
}

// quoteIdent wraps an identifier in dialect-appropriate quotes.
// MySQL uses backticks; SQLite and others use double quotes.
func quoteIdent(name, dialect string) string {
	if dialect == "mysql" {
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
