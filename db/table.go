package db

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/internal/dbx"
)

// TableSpec declares a model's table for auto migration.
type TableSpec struct {
	model   any
	typ     reflect.Type
	indexes []SoftIndex
	soft    bool
}

// SoftIndex is a unique index that soft-deleted rows do not hold.
type SoftIndex struct {
	name    string
	columns []string
}

// Table declares the table of model, a struct or pointer to one that
// embeds a model base, with its SoftUnique indexes. It panics when the
// model is not a model, when a SoftUnique is given to a model without
// soft delete, or when an index names a column that is missing or
// nullable: the mistake is in the declaration and surfaces at startup.
func Table(model any, indexes ...SoftIndex) TableSpec {
	t := reflect.TypeOf(model)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("db.Table: the model must be a struct, got %T", model))
	}
	ptr := reflect.New(t).Interface()
	if _, ok := ptr.(Modeler); !ok {
		panic(fmt.Sprintf("db.Table: %s must embed db.Model, db.SoftDeleteModel, db.OwnedModel or db.OwnedSoftDeleteModel", t.Name()))
	}
	if p, ok := ptr.(RIDPrefixer); ok {
		if err := validatePrefix(p.RIDPrefix()); err != nil {
			panic(fmt.Sprintf("db.Table: %s: %v", t.Name(), err))
		}
	}
	s, err := parseSchema(ptr)
	if err != nil {
		panic(fmt.Sprintf("db.Table: %s: %v", t.Name(), err))
	}
	if s.FieldsByDBName["rid"] == nil {
		panic(fmt.Sprintf("db.Table: %s embeds its model base through an unexported type; gorm maps only exported embedded structs to columns", t.Name()))
	}
	spec := TableSpec{model: ptr, typ: t, indexes: indexes, soft: isSoftDelete(t)}
	if len(indexes) > 0 {
		if !spec.soft {
			panic(fmt.Sprintf("db.Table: %s: SoftUnique needs a model with soft delete (db.SoftDeleteModel or db.OwnedSoftDeleteModel)", t.Name()))
		}
		for _, idx := range indexes {
			if err := checkSoftIndex(s, idx); err != nil {
				panic(fmt.Sprintf("db.Table: %s: %v", t.Name(), err))
			}
		}
	}
	return spec
}

func init() {
	dbx.PlainTable = func(model any) any { return plainTable(model) }
}

// plainTable declares the table of a model without a model base: no RID,
// version or timestamps, only the columns it declares. Batteries reach it
// through dbx.PlainTable for an append-only log or a schema another
// library owns; applications declare tables with Table.
func plainTable(model any) TableSpec {
	t := reflect.TypeOf(model)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("dbx.PlainTable: the model must be a struct, got %T", model))
	}
	ptr := reflect.New(t).Interface()
	if _, ok := ptr.(Modeler); ok {
		panic(fmt.Sprintf("dbx.PlainTable: %s embeds a model base; declare it with db.Table", t.Name()))
	}
	s, err := parseSchema(ptr)
	if err != nil {
		panic(fmt.Sprintf("dbx.PlainTable: %s: %v", t.Name(), err))
	}
	if len(s.PrimaryFields) == 0 {
		panic(fmt.Sprintf("dbx.PlainTable: %s has no primary key", t.Name()))
	}
	return TableSpec{model: ptr, typ: t}
}

// SoftUnique is a unique index over columns among live rows:
// UNIQUE(columns..., delete_token). A soft delete writes a random token,
// so the deleted row stops holding the values. The same index on every
// dialect.
func SoftUnique(name string, columns ...string) SoftIndex {
	if name == "" || len(columns) == 0 {
		panic("db.SoftUnique: needs an index name and at least one column")
	}
	return SoftIndex{name: name, columns: columns}
}

var schemaCache sync.Map

func parseSchema(model any) (*schema.Schema, error) {
	return schema.Parse(model, &schemaCache, schema.NamingStrategy{})
}

func checkSoftIndex(s *schema.Schema, idx SoftIndex) error {
	for _, col := range idx.columns {
		f := s.LookUpField(col)
		if f == nil || f.DBName != col {
			f = s.FieldsByDBName[col]
		}
		if f == nil {
			return fmt.Errorf("SoftUnique %s: no column %q (columns are database names, such as owner_id)", idx.name, col)
		}
		ft := f.FieldType
		if ft.Kind() == reflect.Pointer || (ft.PkgPath() == "database/sql" && strings.HasPrefix(ft.Name(), "Null")) {
			return fmt.Errorf("SoftUnique %s: column %q is nullable; a unique index treats NULLs as distinct, so its columns must be NOT NULL values", idx.name, col)
		}
		if !f.NotNull && !f.PrimaryKey {
			return fmt.Errorf("SoftUnique %s: column %q needs gorm:\"not null\"", idx.name, col)
		}
	}
	return nil
}

// autoMigrate creates or alters the tables, then their SoftUnique indexes.
func autoMigrate(ctx context.Context, gdb *gorm.DB, specs []TableSpec) error {
	gdb = gdb.WithContext(ctx)
	for _, spec := range specs {
		if err := gdb.AutoMigrate(spec.model); err != nil {
			return fmt.Errorf("db: auto migrate %s: %w", spec.typ.Name(), err)
		}
		for _, idx := range spec.indexes {
			if err := createSoftIndex(gdb, spec, idx); err != nil {
				return fmt.Errorf("db: create index %s: %w", idx.name, err)
			}
		}
	}
	return nil
}

func createSoftIndex(gdb *gorm.DB, spec TableSpec, idx SoftIndex) error {
	stmt := &gorm.Statement{DB: gdb}
	if err := stmt.Parse(spec.model); err != nil {
		return err
	}
	dialect := gdb.Name()
	table := stmt.Schema.Table
	cols := make([]string, 0, len(idx.columns)+1)
	for _, c := range idx.columns {
		cols = append(cols, quote(dialect, c))
	}
	cols = append(cols, quote(dialect, "delete_token"))
	ddl := fmt.Sprintf("CREATE UNIQUE INDEX %s ON %s (%s)", quote(dialect, idx.name), quote(dialect, table), strings.Join(cols, ", "))
	if dialect != "mysql" {
		ddl = strings.Replace(ddl, "INDEX", "INDEX IF NOT EXISTS", 1)
		return gdb.Exec(ddl).Error
	}
	// MySQL has no CREATE INDEX IF NOT EXISTS.
	var n int64
	if err := gdb.Raw("SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?",
		table, idx.name).Scan(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	err := gdb.Exec(ddl).Error
	if err != nil && strings.Contains(err.Error(), "Error 1061") { // created by a concurrent process
		return nil
	}
	return err
}

func quote(dialect, ident string) string {
	if dialect == "mysql" {
		return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}
