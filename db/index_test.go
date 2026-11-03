package db_test

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
)

// testLogger sends a handle's warnings and errors to the test's output.
func testLogger(t *testing.T) db.Option {
	return db.Logger(slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelWarn})))
}

// withIndex returns the configuration of a fresh database holding the
// tables of models, migrated with no index declared, and what ddl made:
// the database an application meets after writing an index by hand.
func withIndex(t *testing.T, ddl string, models ...any) db.Config {
	t.Helper()
	cfg := dbtest.Config(t)
	specs := make([]db.TableSpec, len(models))
	for i, m := range models {
		specs[i] = db.Table(m)
	}
	d, err := db.Open(cfg, testLogger(t), db.Tables(specs...))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close(t.Context()) }()
	if err := d.Unsafe(t.Context()).Exec(ddl).Error; err != nil {
		t.Fatalf("%s: %v", ddl, err)
	}
	return cfg
}

// openErr opens cfg and returns the error of Open, closing the handle
// when there is none.
func openErr(t *testing.T, cfg db.Config, opts ...db.Option) error {
	t.Helper()
	d, err := db.Open(cfg, append([]db.Option{testLogger(t)}, opts...)...)
	if err == nil {
		_ = d.Close(t.Context())
	}
	return err
}

// TestCreateIndex_DifferentIndexFails: an index of the declared name
// that is not the declared index fails Open, which names both, instead
// of standing in for it.
func TestCreateIndex_DifferentIndexFails(t *testing.T) {
	ownerBook := db.Table(&bookmark{}, db.Unique("uk_bm", "owner_id", "book_id"))
	owner := db.Table(&bookmark{}, db.Unique("uk_bm_owner", "owner_id"))
	nameScope := db.Table(&tag{}, db.SoftUnique("uk_tag_name", "name", "scope"))
	email := db.Table(&member{}, db.SoftUnique("uk_member_email", "email"))
	for _, c := range []struct {
		name, ddl string
		table     db.TableSpec
		want      string
		partial   bool // MySQL has no partial index
	}{
		{"columns in another order", "CREATE UNIQUE INDEX uk_bm ON bookmarks (book_id, owner_id)", ownerBook,
			`db: index "uk_bm" exists on "bookmarks" over (book_id, owner_id), unique; declared on "bookmarks" over (owner_id, book_id), unique (drop the index for auto migrate to create it, or rename the declaration)`, false},
		{"other columns", "CREATE UNIQUE INDEX uk_bm ON bookmarks (owner_id, rid)", ownerBook,
			`index "uk_bm" exists on "bookmarks" over (owner_id, rid), unique; declared on "bookmarks" over (owner_id, book_id), unique`, false},
		{"not unique", "CREATE INDEX uk_bm ON bookmarks (owner_id, book_id)", ownerBook,
			`index "uk_bm" exists on "bookmarks" over (owner_id, book_id), not unique; declared on "bookmarks" over (owner_id, book_id), unique`, false},
		// gorm's MySQL migrator would drop this index before chok looks.
		{"another column", "CREATE UNIQUE INDEX uk_bm_owner ON bookmarks (book_id)", owner,
			`index "uk_bm_owner" exists on "bookmarks" over (book_id), unique; declared on "bookmarks" over (owner_id), unique`, false},
		{"a SoftUnique without delete_token", "CREATE UNIQUE INDEX uk_tag_name ON tags (name, scope)", nameScope,
			`index "uk_tag_name" exists on "tags" over (name, scope), unique; declared on "tags" over (name, scope, delete_token), unique`, false},
		{"partial", "CREATE UNIQUE INDEX uk_bm ON bookmarks (owner_id, book_id) WHERE book_id <> 'x'", ownerBook,
			`index "uk_bm" exists on "bookmarks" over (owner_id, book_id), unique, partial; declared on "bookmarks" over (owner_id, book_id), unique`, true},
		// What chok v2 created for a SoftUnique on Postgres.
		{"partial without delete_token", "CREATE UNIQUE INDEX uk_member_email ON members (email) WHERE deleted_at IS NULL", email,
			`index "uk_member_email" exists on "members" over (email), unique, partial; declared on "members" over (email, delete_token), unique`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.partial && dbtest.Driver() == "mysql" {
				t.Skip("MySQL has no partial index")
			}
			cfg := withIndex(t, c.ddl, &bookmark{}, &tag{}, &member{})
			if err := openErr(t, cfg, db.Tables(c.table)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v\nwant an error with %s", err, c.want)
			}
		})
	}
}

// TestCreateIndex_NameOnAnotherTable: SQLite and Postgres name indexes
// per schema, so an index of the declared name on another table is not
// the declared index; MySQL names them per table and creates it.
func TestCreateIndex_NameOnAnotherTable(t *testing.T) {
	cfg := withIndex(t, "CREATE UNIQUE INDEX uk_owner_x ON members (login)", &bookmark{}, &member{})
	tables := db.Tables(db.Table(&bookmark{}, db.Unique("uk_owner_x", "owner_id", "book_id")))
	if dbtest.Driver() != "mysql" {
		want := `db: index "uk_owner_x" exists on "members" over (login), unique; declared on "bookmarks" over (owner_id, book_id), unique (SQLite and Postgres name indexes per schema; rename the declaration)`
		if err := openErr(t, cfg, tables); err == nil || err.Error() != want {
			t.Fatalf("got %v\nwant %s", err, want)
		}
		return
	}
	gdb := dbtest.OpenConfig(t, cfg, tables).Unsafe(t.Context())
	if cols := indexColumns(t, gdb, "bookmarks", "uk_owner_x"); !slices.Equal(slices.Sorted(slices.Values(cols)), []string{"book_id", "owner_id"}) {
		t.Fatalf("MySQL creates the declared index beside the other: %v", cols)
	}
	if cols := indexColumns(t, gdb, "members", "uk_owner_x"); !slices.Equal(cols, []string{"login"}) {
		t.Fatalf("and leaves the other as it is: %v", cols)
	}
}

// TestCreateIndex_NameOfATable: a table named as the declared index stops
// the index on SQLite, which refuses to create it, and on Postgres, which
// skips it; MySQL keeps index names apart from table names.
func TestCreateIndex_NameOfATable(t *testing.T) {
	cfg := withIndex(t, "CREATE TABLE uk_taken (id INT PRIMARY KEY)", &bookmark{})
	tables := db.Tables(db.Table(&bookmark{}, db.Unique("uk_taken", "owner_id", "book_id")))
	switch dbtest.Driver() {
	case "mysql":
		gdb := dbtest.OpenConfig(t, cfg, tables).Unsafe(t.Context())
		if cols := indexColumns(t, gdb, "bookmarks", "uk_taken"); len(cols) != 2 {
			t.Fatalf("MySQL creates the index: %v", cols)
		}
	case "postgres":
		if err := openErr(t, cfg, tables); err == nil || !strings.Contains(err.Error(), `db: index "uk_taken" is missing after CREATE INDEX`) {
			t.Fatalf("got %v", err)
		}
	default:
		if err := openErr(t, cfg, tables); err == nil || !strings.Contains(err.Error(), "already a table named uk_taken") {
			t.Fatalf("got %v", err)
		}
	}
}

// bookshelfItem is the bookshelf of an application that wrote its
// UNIQUE(owner_id, book_id) by hand, then declared it with db.Unique.
type bookshelfItem struct {
	db.OwnedModel
	BookID string `gorm:"size:24;not null"`
}

// logBuffer collects log lines; SQLite's maintenance logs from its own
// goroutine.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestUnique_AdoptsTheIndexMadeByHand: an index made by hand under the
// declared name, on the declared table and columns in their order, is
// the declared index. Open runs no DDL on it, it stays the table's one
// index over those columns, and it holds.
func TestUnique_AdoptsTheIndexMadeByHand(t *testing.T) {
	cfg := withIndex(t, "CREATE UNIQUE INDEX uk_bsi_owner_book ON bookshelf_items (owner_id, book_id)", &bookshelfItem{})
	var log logBuffer
	d := dbtest.OpenConfig(t, cfg, db.Logger(slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		db.Tables(db.Table(&bookshelfItem{}, db.Unique("uk_bsi_owner_book", "owner_id", "book_id"))))
	// Statements, queries read with Scan included, are logged with their
	// placeholders, so only DDL names the index.
	if s := log.String(); !strings.Contains(s, `msg="db: query"`) || strings.Contains(s, "uk_bsi_owner_book") {
		t.Fatalf("Open logs its statements and runs none on the index:\n%s", s)
	}
	gdb := d.Unsafe(t.Context())
	idx, err := gdb.Migrator().GetIndexes("bookshelf_items")
	if err != nil {
		t.Fatal(err)
	}
	var over []string
	for _, i := range idx {
		if slices.Equal(slices.Sorted(slices.Values(i.Columns())), []string{"book_id", "owner_id"}) {
			over = append(over, i.Name())
		}
	}
	if !slices.Equal(over, []string{"uk_bsi_owner_book"}) {
		t.Fatalf("one index over (owner_id, book_id) on %s, got %v", dbtest.Driver(), over)
	}
	item := func() *bookshelfItem {
		b := &bookshelfItem{BookID: "b1"}
		b.SetOwnerID("u1")
		return b
	}
	if err := gdb.Create(item()).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(item()).Error; err == nil {
		t.Fatal("the index holds")
	}
}

// setting is one row per owner, as an application keeps each user's
// settings: UNIQUE(owner_id), one column, which the model base tags.
type setting struct {
	db.OwnedModel
	Theme string `gorm:"size:20;not null"`
}

// TestUnique_OneColumnKeptAcrossOpens: the next Open finds a one-column
// Unique that the first made as declared and runs no DDL on it, so the
// table never goes without it; it stays the column's one unique index and
// holds. gorm's MySQL migrator drops a one-column unique index it did not
// make; chok's keeps it.
func TestUnique_OneColumnKeptAcrossOpens(t *testing.T) {
	cfg := dbtest.Config(t)
	tables := db.Tables(db.Table(&setting{}, db.Unique("uk_setting_owner", "owner_id")))
	if err := openErr(t, cfg, tables); err != nil {
		t.Fatal(err)
	}
	var log logBuffer
	d := dbtest.OpenConfig(t, cfg, db.Logger(slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))), tables)
	// Statements, queries read with Scan included, are logged with their
	// placeholders, so only DDL names the index.
	if s := log.String(); !strings.Contains(s, `msg="db: query"`) || strings.Contains(s, "uk_setting_owner") {
		t.Fatalf("the second Open runs no statement on the index:\n%s", s)
	}
	gdb := d.Unsafe(t.Context())
	if names := uniqueOver(t, gdb, "settings", "owner_id"); !slices.Equal(names, []string{"uk_setting_owner"}) {
		t.Fatalf("one unique index over (owner_id) on %s, got %v", dbtest.Driver(), names)
	}
	row := func() *setting {
		s := &setting{Theme: "dark"}
		s.SetOwnerID("u1")
		return s
	}
	if err := gdb.Create(row()).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(row()).Error; err == nil {
		t.Fatal("the index holds")
	}
}

// uniqueOver returns the unique indexes of table over column alone, as
// GetIndexes lists them: MySQL lists the index behind a unique
// constraint, Postgres and SQLite leave it out.
func uniqueOver(t *testing.T, gdb *gorm.DB, table, column string) []string {
	t.Helper()
	idx, err := gdb.Migrator().GetIndexes(table)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, i := range idx {
		if u, _ := i.Unique(); u && slices.Equal(i.Columns(), []string{column}) {
			names = append(names, i.Name())
		}
	}
	return names
}

// handle and plainHandle are one table, handles, with and without gorm's
// unique tag on name, which gorm migrates as the constraint
// uni_handles_name.
type handle struct {
	db.Model
	Name string `gorm:"size:50;not null;unique"`
}

type plainHandle struct {
	db.Model
	Name string `gorm:"size:50;not null"`
}

func (plainHandle) TableName() string { return "handles" }

// TestOpen_MigratesTheUniqueTag: a unique tag added to a column adds the
// constraint uni_<table>_<column>, migrating again keeps it as the
// column's one unique index, and removing the tag drops it, on MySQL as
// on the other dialects.
func TestOpen_MigratesTheUniqueTag(t *testing.T) {
	cfg := dbtest.Config(t)
	plain, tagged := db.Tables(db.Table(&plainHandle{})), db.Tables(db.Table(&handle{}))
	for _, tables := range []db.Option{plain, tagged} {
		if err := openErr(t, cfg, tables); err != nil {
			t.Fatal(err)
		}
	}
	gdb := dbtest.OpenConfig(t, cfg, tagged).Unsafe(t.Context())
	if !gdb.Migrator().HasConstraint(&handle{}, "uni_handles_name") {
		t.Fatal("the tag adds the constraint")
	}
	other := slices.DeleteFunc(uniqueOver(t, gdb, "handles", "name"), func(n string) bool { return n == "uni_handles_name" })
	if len(other) > 0 {
		t.Fatalf("the constraint is the one unique index over (name) on %s; also %v", dbtest.Driver(), other)
	}
	if err := gdb.Create(&handle{Name: "ann"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&handle{Name: "ann"}).Error; err == nil {
		t.Fatal("the constraint holds")
	}
	if err := openErr(t, cfg, plain); err != nil {
		t.Fatal(err)
	}
	if gdb.Migrator().HasConstraint(&handle{}, "uni_handles_name") {
		t.Fatal("removing the tag drops the constraint")
	}
}

// withUniqueConstraint returns the configuration of a fresh database
// holding the table of model, migrated, with column made unique by hand
// under the constraint name: ALTER TABLE adds it on Postgres and MySQL.
// SQLite cannot add a constraint to a table, so the table is made again
// from the DDL gorm wrote with the constraint added, and then its indexes.
func withUniqueConstraint(t *testing.T, model any, table, name, column string) db.Config {
	t.Helper()
	constraint := "CONSTRAINT " + name + " UNIQUE (" + column + ")"
	if dbtest.Driver() != "sqlite" {
		return withIndex(t, "ALTER TABLE "+table+" ADD "+constraint, model)
	}
	cfg := dbtest.Config(t)
	d, err := db.Open(cfg, testLogger(t), db.Tables(db.Table(model)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close(t.Context()) }()
	gdb := d.Unsafe(t.Context())
	var ddl string
	var indexes []string
	if err := gdb.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&ddl).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND sql IS NOT NULL", table).Scan(&indexes).Error; err != nil {
		t.Fatal(err)
	}
	for _, s := range append([]string{"DROP TABLE " + table, strings.TrimSuffix(ddl, ")") + "," + constraint + ")"}, indexes...) {
		if err := gdb.Exec(s).Error; err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return cfg
}

// TestOpen_KeepsAForeignUniqueConstraint: a column unique by a
// constraint of another name than the unique tag's, made by hand or by
// an earlier schema, while the model declares nothing on it. Open runs
// no DDL on the table, and the constraint stays and holds. gorm's
// migrator for Postgres and SQLite dropped the unique tag's constraint,
// uni_members_login, without looking for it: Postgres failed Open, as
// there is none, and SQLite rebuilt the table on every Open.
func TestOpen_KeepsAForeignUniqueConstraint(t *testing.T) {
	cfg := withUniqueConstraint(t, &member{}, "members", "uq_members_login", "login")
	var log logBuffer
	d := dbtest.OpenConfig(t, cfg, db.Logger(slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		db.Tables(db.Table(&member{})))
	// Look at how each statement starts rather than for names: stricter,
	// as it catches DDL whatever it names; a SQLite rebuild also copies
	// the rows through members__temp.
	s := log.String()
	if !strings.Contains(s, `msg="db: query"`) {
		t.Fatalf("Open logs its statements:\n%s", s)
	}
	for _, ddl := range []string{`sql="ALTER `, `sql="CREATE `, `sql="DROP `, "members__temp"} {
		if strings.Contains(s, ddl) {
			t.Fatalf("Open runs no DDL on the table; found %q in:\n%s", ddl, s)
		}
	}
	gdb := d.Unsafe(t.Context())
	if !gdb.Migrator().HasConstraint(&member{}, "uq_members_login") {
		t.Fatal("the constraint stays")
	}
	if err := gdb.Create(&member{Login: "ann", Email: "ann@example.com"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&member{Login: "ann", Email: "ann2@example.com"}).Error; err == nil {
		t.Fatal("the constraint holds")
	}
}
