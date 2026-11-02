package db_test

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

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
		// Two columns: gorm's MySQL AutoMigrate drops a one-column unique
		// index on a column without a unique tag before chok looks.
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
	// Statements are logged with their placeholders, so only DDL names
	// the index.
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
