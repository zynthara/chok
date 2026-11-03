package db

import (
	"context"
	"database/sql"
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
//
// In migrate: auto, migrating creates each index unless one of its name
// exists, and an existing one must be the declared index: on the same
// table, over the same columns in the same order (delete_token last for
// a SoftUnique), unique and without a WHERE clause. Otherwise Open,
// RegisterSchema or Migrate fails with both in the error, rather than
// drop or rebuild the index. MySQL names indexes per table, SQLite and
// Postgres per schema: there an index of the name on another table
// differs too.
//
// On Postgres, migrate: auto also fails when search_path finds the table
// in a schema other than current_schema(), where migrating would create
// an empty table of the name that hides it.
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

// autoMigrate creates or alters the tables, then creates their indexes
// or checks the ones that exist. On Postgres it first checks where
// search_path finds each table, before any DDL.
func autoMigrate(ctx context.Context, gdb *gorm.DB, specs []TableSpec) error {
	gdb = gdb.WithContext(ctx)
	if gdb.Name() == "postgres" {
		for _, spec := range specs {
			if err := checkSearchPath(gdb, spec); err != nil {
				return err
			}
		}
	}
	for _, spec := range specs {
		if err := gdb.AutoMigrate(spec.model); err != nil {
			return fmt.Errorf("db: auto migrate %s: %w", spec.typ.Name(), err)
		}
		for _, idx := range spec.indexes {
			if err := createIndex(gdb, spec, idx); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkSearchPath refuses a table that search_path finds in a schema
// other than current_schema(). gorm's Postgres migrator looks for a table
// in current_schema() alone (HasTable, gorm.io/driver/postgres v1.6.2
// migrator.go:283), so AutoMigrate would create an empty one there, ahead
// of the table holding the rows on search_path, and every query after
// would use the new one. A table that search_path finds nowhere is left
// to AutoMigrate to create, and a temporary table, which only its session
// sees, is not where an application keeps its rows.
func checkSearchPath(gdb *gorm.DB, spec TableSpec) error {
	stmt := &gorm.Statement{DB: gdb}
	if err := stmt.Parse(spec.model); err != nil {
		return fmt.Errorf("db: auto migrate %s: %w", spec.typ.Name(), err)
	}
	table := stmt.Schema.Table
	var nsp, path string
	var current sql.NullString
	// to_regclass resolves the name as a query does; quoted, it keeps the
	// case gorm created the table with. Only a relation that a query reads
	// through the name counts: a table, partitioned table, view,
	// materialized view or foreign table. An index, sequence or composite
	// type of the name holds no rows for a new table to hide.
	err := gdb.Raw(`SELECT n.nspname, current_schema(), current_setting('search_path')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass(?) AND c.relkind IN ('r', 'p', 'v', 'm', 'f')`, quote("postgres", table)).Row().Scan(&nsp, &current, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("db: auto migrate %s: find table %q on search_path: %w", spec.typ.Name(), table, err)
	}
	if nsp == current.String || strings.HasPrefix(nsp, "pg_temp") {
		return nil
	}
	return fmt.Errorf("db: table %q is in schema %q, but current_schema() is %q (search_path %s): auto migrate would create an empty %q there that hides it (put the application's schema first on search_path, or set search_path in the DSN)", table, nsp, current.String, path, table)
}

// indexShape is an index as Table declares it or as the catalog holds it.
type indexShape struct {
	table   string
	columns []string
	unique  bool
	partial bool // a WHERE clause leaves rows out of it
}

func (s *indexShape) String() string {
	kind := "unique"
	if !s.unique {
		kind = "not unique"
	}
	if s.partial {
		kind += ", partial"
	}
	return fmt.Sprintf("on %q over (%s), %s", s.table, strings.Join(s.columns, ", "), kind)
}

// matches reports whether s and o are one index, comparing identifiers
// as the dialect does.
func (s *indexShape) matches(o *indexShape, dialect string) bool {
	same := func(a, b string) bool { return sameIdent(dialect, a, b) }
	return s.unique == o.unique && s.partial == o.partial && same(s.table, o.table) &&
		slices.EqualFunc(s.columns, o.columns, same)
}

// sameIdent compares two identifiers as the dialect does: Postgres keeps
// the case of the quoted names chok creates, SQLite and MySQL ignore it.
func sameIdent(dialect, a, b string) bool {
	if dialect == "postgres" {
		return a == b
	}
	return strings.EqualFold(a, b)
}

// createIndex makes sure idx exists as declared: UNIQUE(columns...), with
// delete_token last for a SoftUnique. It reads the index of that name
// from the catalog and creates it when there is none. What the catalog
// then holds must be the declaration, on the same table, over the same
// columns in the same order, unique and with no WHERE clause; otherwise
// it fails naming both, rather than let another index stand in for the
// declared one. It never drops or rebuilds an index.
func createIndex(gdb *gorm.DB, spec TableSpec, idx Index) error {
	stmt := &gorm.Statement{DB: gdb}
	if err := stmt.Parse(spec.model); err != nil {
		return fmt.Errorf("db: index %s: %w", idx.name, err)
	}
	dialect := gdb.Name()
	want := &indexShape{table: stmt.Schema.Table, columns: slices.Clone(idx.columns), unique: true}
	if idx.soft {
		want.columns = append(want.columns, "delete_token")
	}
	have, err := readIndex(gdb, want.table, idx.name)
	if err != nil {
		return fmt.Errorf("db: index %s: read the catalog: %w", idx.name, err)
	}
	if have == nil {
		cols := make([]string, len(want.columns))
		for i, c := range want.columns {
			cols[i] = quote(dialect, c)
		}
		ddl := fmt.Sprintf("CREATE UNIQUE INDEX %s ON %s (%s)", quote(dialect, idx.name), quote(dialect, want.table), strings.Join(cols, ", "))
		if dialect != "mysql" {
			ddl = strings.Replace(ddl, "INDEX", "INDEX IF NOT EXISTS", 1)
		}
		// MySQL has no IF NOT EXISTS: error 1061 is a concurrent process
		// that created the index first.
		if err := gdb.Exec(ddl).Error; err != nil && (dialect != "mysql" || !strings.Contains(err.Error(), "Error 1061")) {
			return fmt.Errorf("db: create index %s: %w", idx.name, err)
		}
		// Read it back: a concurrent process may have created it first,
		// from another declaration, and Postgres's IF NOT EXISTS skips a
		// name that any relation of the schema holds.
		if have, err = readIndex(gdb, want.table, idx.name); err != nil {
			return fmt.Errorf("db: index %s: read the catalog: %w", idx.name, err)
		}
		if have == nil {
			return fmt.Errorf("db: index %q is missing after CREATE INDEX: another object of the schema has that name (rename the declaration)", idx.name)
		}
	}
	if have.matches(want, dialect) {
		return nil
	}
	hint := "drop the index for auto migrate to create it, or rename the declaration"
	if !sameIdent(dialect, have.table, want.table) {
		hint = "SQLite and Postgres name indexes per schema; rename the declaration"
	}
	return fmt.Errorf("db: index %q exists %s; declared %s (%s)", idx.name, have, want, hint)
}

// readIndex reads the index named name from the catalog, or nil when there
// is none: on table for MySQL, which names indexes per table, and in the
// current schema for SQLite and Postgres, which name them per schema, so
// the index found may be on another table. A key part that is an
// expression reads as (expression), and a MySQL prefix as column(length).
func readIndex(gdb *gorm.DB, table, name string) (*indexShape, error) {
	var (
		rows *sql.Rows
		err  error
	)
	switch gdb.Name() {
	case "mysql":
		rows, err = gdb.Raw(`SELECT table_name, non_unique = 0, FALSE,
				IF(sub_part IS NULL, column_name, CONCAT(column_name, '(', sub_part, ')'))
			FROM information_schema.statistics
			WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?
			ORDER BY seq_in_index`, table, name).Rows()
	case "postgres":
		// indkey holds the key columns, 0 for an expression, then the
		// INCLUDE columns, which take no part in uniqueness.
		rows, err = gdb.Raw(`SELECT t.relname, i.indisunique, i.indpred IS NOT NULL, a.attname
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			JOIN pg_index i ON i.indexrelid = c.oid
			JOIN pg_class t ON t.oid = i.indrelid
			CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
			LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
			WHERE n.nspname = current_schema() AND c.relname = ? AND k.ord <= i.indnkeyatts
			ORDER BY k.ord`, name).Rows()
	default: // sqlite, which compares index names without case
		rows, err = gdb.Raw(`SELECT m.tbl_name, l."unique", l.partial, i.name
			FROM sqlite_master m
			JOIN pragma_index_list(m.tbl_name) l ON l.name = m.name
			JOIN pragma_index_info(m.name) i
			WHERE m.type = 'index' AND m.name = ? COLLATE NOCASE
			ORDER BY i.seqno`, name).Rows()
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var have *indexShape
	for rows.Next() {
		var s indexShape
		var col sql.NullString
		if err := rows.Scan(&s.table, &s.unique, &s.partial, &col); err != nil {
			return nil, err
		}
		if have == nil {
			have = &s
		}
		if !col.Valid {
			col.String = "(expression)"
		}
		have.columns = append(have.columns, col.String)
	}
	return have, rows.Err()
}

func quote(dialect, ident string) string {
	if dialect == "mysql" {
		return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}
