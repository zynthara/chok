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
// Validation order (fail-fast):
//  1. SoftUnique used on non-SoftDeleteModel → error
//  2. SoftUnique columns must be NOT NULL → error for pointer/sql.Null* types or missing "not null" tag
//  3. AutoMigrate
//  4. Create SoftUnique indexes
//
// ctx flows into every DDL statement (AutoMigrate, Raw, Exec) via
// gdb.WithContext so that registry Init timeouts and shutdown cancellation
// can abort long-running migrations instead of blocking startup.
func Migrate(ctx context.Context, gdb *gorm.DB, specs ...TableSpec) error {
	gdb = gdb.WithContext(ctx)
	for _, spec := range specs {
		// Step 1: SoftUnique only valid for SoftDeleteModel.
		if len(spec.indexes) > 0 && !spec.soft {
			return fmt.Errorf("db.Migrate: SoftUnique is only valid for SoftDeleteModel, "+
				"model %T does not embed SoftDeleteModel", spec.model)
		}

		// Step 2: SoftUnique columns must be NOT NULL.
		for _, idx := range spec.indexes {
			if err := validateSoftUniqueColumns(spec.model, idx); err != nil {
				return err
			}
		}

		// Step 3: AutoMigrate.
		if err := gdb.AutoMigrate(spec.model); err != nil {
			return fmt.Errorf("db.Migrate: AutoMigrate %T: %w", spec.model, err)
		}

		// Step 4: Create SoftUnique indexes.
		for _, idx := range spec.indexes {
			if err := createSoftUniqueIndex(gdb, spec.model, idx); err != nil {
				return fmt.Errorf("db.Migrate: create index %s: %w", idx.Name, err)
			}
		}
	}
	return nil
}

// validateSoftUniqueColumns ensures all columns in a SoftUnique index are NOT NULL.
func validateSoftUniqueColumns(model any, idx SoftIndex) error {
	t := reflect.TypeOf(model)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	for _, col := range idx.Columns {
		field, found := findFieldByColumn(t, col)
		if !found {
			return fmt.Errorf("db.Migrate: SoftUnique %q references column %q not found in %s",
				idx.Name, col, t.Name())
		}

		// Pointer types are nullable.
		if field.Type.Kind() == reflect.Ptr {
			return fmt.Errorf("db.Migrate: SoftUnique column %q in %s is a pointer type (nullable); "+
				"SoftUnique columns must be NOT NULL", col, t.Name())
		}

		// sql.Null* types are nullable.
		typeName := field.Type.Name()
		if strings.HasPrefix(typeName, "Null") && field.Type.PkgPath() == "database/sql" {
			return fmt.Errorf("db.Migrate: SoftUnique column %q in %s uses sql.%s (nullable); "+
				"SoftUnique columns must be NOT NULL", col, t.Name(), typeName)
		}

		// Must have "not null" in gorm tag.
		gormTag := strings.ToLower(field.Tag.Get("gorm"))
		if !strings.Contains(gormTag, "not null") {
			return fmt.Errorf("db.Migrate: SoftUnique column %q in %s is missing 'not null' gorm tag; "+
				"SoftUnique columns must be NOT NULL", col, t.Name())
		}
	}
	return nil
}

// findFieldByColumn finds a struct field matching a gorm column name.
func findFieldByColumn(t reflect.Type, column string) (reflect.StructField, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			if sf, ok := findFieldByColumn(f.Type, column); ok {
				return sf, true
			}
			continue
		}
		// Check gorm column tag.
		gormTag := f.Tag.Get("gorm")
		for _, part := range strings.Split(gormTag, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "column:") {
				if strings.TrimPrefix(part, "column:") == column {
					return f, true
				}
			}
		}
		// Fallback: snake_case field name.
		if toSnakeCase(f.Name) == column {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// toSnakeCase converts CamelCase to snake_case.
// Handles consecutive uppercase correctly: "UserID" → "user_id".
func toSnakeCase(s string) string {
	var result []byte
	for i, c := range s {
		if c >= 'A' && c <= 'Z' {
			if i > 0 && s[i-1] >= 'a' && s[i-1] <= 'z' {
				result = append(result, '_')
			}
			result = append(result, byte(c)+32)
		} else {
			result = append(result, byte(c))
		}
	}
	return string(result)
}

// createSoftUniqueIndex creates a composite unique index including delete_token.
// Uses dialect-appropriate SQL: SQLite supports IF NOT EXISTS, MySQL does not.
func createSoftUniqueIndex(gdb *gorm.DB, model any, idx SoftIndex) error {
	stmt := &gorm.Statement{DB: gdb}
	if err := stmt.Parse(model); err != nil {
		return err
	}
	tableName := stmt.Schema.Table

	dialect := gdb.Dialector.Name()

	cols := make([]string, 0, len(idx.Columns)+1)
	for _, c := range idx.Columns {
		cols = append(cols, quoteIdent(c, dialect))
	}
	cols = append(cols, quoteIdent("delete_token", dialect))
	colList := strings.Join(cols, ", ")

	qTable := quoteIdent(tableName, dialect)
	qIndex := quoteIdent(idx.Name, dialect)

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
