package db

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
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
	indexes []Index
	soft    bool
}

// Index is one index declaration of Table: SoftUnique or Unique.
type Index struct {
	name    string
	columns []string
	soft    bool // SoftUnique: delete_token joins the columns
}

// Table declares the table of model, a struct or pointer to one that
// embeds a model base, with its indexes: Unique on any model, SoftUnique
// on a model with soft delete. It panics when the model is not a model,
// when a SoftUnique is given to a model without soft delete, or when an
// index names a column that is missing or nullable: the mistake is in
// the declaration and surfaces at startup.
func Table(model any, indexes ...Index) TableSpec {
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
	for _, idx := range indexes {
		if idx.soft && !spec.soft {
			panic(fmt.Sprintf("db.Table: %s: SoftUnique needs a model with soft delete (db.SoftDeleteModel or db.OwnedSoftDeleteModel); db.Unique takes any model and keeps a deleted row's values", t.Name()))
		}
		if err := checkIndex(s, idx); err != nil {
			panic(fmt.Sprintf("db.Table: %s: %v", t.Name(), err))
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
// dialect; Table accepts it only on a model with soft delete. It panics
// without a name or a column.
func SoftUnique(name string, columns ...string) Index {
	return newIndex("db.SoftUnique", name, columns, true)
}

// Unique is a unique index over columns among all rows: UNIQUE(columns...),
// the same index on every dialect, on a model with any base. An owned
// model without soft delete keeps a value unique per owner:
//
//	db.Table(&Like{}, db.Unique("uk_like_owner_post", "owner_id", "post_id"))
//
// On a soft-delete model a deleted row still holds its Unique values: use
// Unique for a value that is never reused and SoftUnique for one a delete
// releases. It panics without a name or a column.
func Unique(name string, columns ...string) Index {
	return newIndex("db.Unique", name, columns, false)
}

func newIndex(fn, name string, columns []string, soft bool) Index {
	if name == "" || len(columns) == 0 {
		panic(fn + ": needs an index name and at least one column")
	}
	return Index{name: name, columns: slices.Clone(columns), soft: soft}
}

// kind names the declaration in Table's panics.
func (idx Index) kind() string {
	if idx.soft {
		return "SoftUnique"
	}
	return "Unique"
}

var schemaCache sync.Map

func parseSchema(model any) (*schema.Schema, error) {
	return schema.Parse(model, &schemaCache, schema.NamingStrategy{})
}

// checkIndex checks the columns of either kind of index alike: each is a
// column of the model, NOT NULL and not a nullable Go type.
func checkIndex(s *schema.Schema, idx Index) error {
	if idx.name == "" || len(idx.columns) == 0 {
		return errors.New("an index needs a name and at least one column; declare it with db.Unique or db.SoftUnique")
	}
	for _, col := range idx.columns {
		f := s.LookUpField(col)
		if f == nil || f.DBName != col {
			f = s.FieldsByDBName[col]
		}
		if f == nil {
			return fmt.Errorf("%s %s: no column %q (columns are database names, such as owner_id)", idx.kind(), idx.name, col)
		}
		ft := f.FieldType
		if ft.Kind() == reflect.Pointer || (ft.PkgPath() == "database/sql" && strings.HasPrefix(ft.Name(), "Null")) {
			return fmt.Errorf("%s %s: column %q is nullable; a unique index treats NULLs as distinct, so its columns must be NOT NULL values", idx.kind(), idx.name, col)
		}
		if !f.NotNull && !f.PrimaryKey {
			return fmt.Errorf("%s %s: column %q needs gorm:\"not null\"", idx.kind(), idx.name, col)
		}
	}
	return nil
}

// autoMigrate creates or alters the tables, then their indexes.
func autoMigrate(ctx context.Context, gdb *gorm.DB, specs []TableSpec) error {
	gdb = gdb.WithContext(ctx)
	for _, spec := range specs {
		if err := gdb.AutoMigrate(spec.model); err != nil {
			return fmt.Errorf("db: auto migrate %s: %w", spec.typ.Name(), err)
		}
		for _, idx := range spec.indexes {
			if err := createIndex(gdb, spec, idx); err != nil {
				return fmt.Errorf("db: create index %s: %w", idx.name, err)
			}
		}
	}
	return nil
}

// createIndex creates idx, UNIQUE(columns...) with delete_token last for
// a SoftUnique, unless an index of its name exists: on the table for
// MySQL, anywhere in the schema for SQLite and Postgres, whose index
// names are schema-wide. An existing index's columns are not compared.
func createIndex(gdb *gorm.DB, spec TableSpec, idx Index) error {
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
	if idx.soft {
		cols = append(cols, quote(dialect, "delete_token"))
	}
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
