package db_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
)

// searchPathAhead returns a configuration whose search_path puts a new,
// empty schema first and the test's own schema second, the names of
// both, and a handle on the test's schema to look with. It opens the
// test's schema with opts first, so their tables are made there.
func searchPathAhead(t *testing.T, opts ...db.Option) (cfg db.Config, home, ahead string, gdb *gorm.DB) {
	t.Helper()
	if dbtest.Driver() != "postgres" {
		t.Skip("Postgres only: SQLite and MySQL have no search_path")
	}
	cfg = dbtest.Config(t)
	d := dbtest.OpenConfig(t, cfg, opts...)
	gdb = d.Unsafe(t.Context())
	if err := gdb.Raw("SELECT current_schema()").Scan(&home).Error; err != nil {
		t.Fatal(err)
	}
	ahead = home + "_ahead"
	if err := gdb.Exec(`CREATE SCHEMA "` + ahead + `"`).Error; err != nil {
		t.Fatal(err)
	}
	// t.Context() is canceled before the cleanups run.
	t.Cleanup(func() {
		if err := d.Unsafe(context.WithoutCancel(t.Context())).Exec(`DROP SCHEMA "` + ahead + `" CASCADE`).Error; err != nil {
			t.Logf("drop schema %s: %v", ahead, err)
		}
	})
	from := "search_path=" + home
	if !strings.Contains(cfg.Postgres.DSN, from) {
		t.Fatalf("dbtest's DSN sets %s", from)
	}
	cfg.Postgres.DSN = strings.Replace(cfg.Postgres.DSN, from, "search_path="+ahead+","+home, 1)
	return cfg, home, ahead, gdb
}

// tableSchemas returns which of schemas hold a table named table.
func tableSchemas(t *testing.T, gdb *gorm.DB, table string, schemas ...string) []string {
	t.Helper()
	var in []string
	if err := gdb.Raw("SELECT schemaname FROM pg_tables WHERE tablename = ? AND schemaname IN ? ORDER BY schemaname", table, schemas).Scan(&in).Error; err != nil {
		t.Fatal(err)
	}
	return in
}

// refusesTheHiddenTable checks that err refuses the bookmarks table of
// home, which ahead comes before on search_path, and that no bookmarks
// table was made in ahead.
func refusesTheHiddenTable(t *testing.T, err error, gdb *gorm.DB, home, ahead string) {
	t.Helper()
	want := fmt.Sprintf(`db: table "bookmarks" is in schema %q, but current_schema() is %q (search_path %s,%s): auto migrate would create an empty "bookmarks" there that hides it (put the application's schema first on search_path, or set search_path in the DSN)`, home, ahead, ahead, home)
	if err == nil || err.Error() != want {
		t.Errorf("got %v\nwant %s", err, want)
	}
	if in := tableSchemas(t, gdb, "bookmarks", home, ahead); !slices.Equal(in, []string{home}) {
		t.Errorf("no table is made in %s; bookmarks is in %v", ahead, in)
	}
}

// TestOpen_RefusesATableSearchPathHides: Postgres finds a table through
// search_path, while gorm's auto migrate looks for it in current_schema()
// alone. With the application's schema second on search_path, Open made
// an empty table of the name in the first, which hid the one holding the
// rows; now it fails, naming the table, both schemas and search_path,
// and makes nothing.
func TestOpen_RefusesATableSearchPathHides(t *testing.T) {
	cfg, home, ahead, gdb := searchPathAhead(t, db.Tables(db.Table(&bookmark{})))
	err := openErr(t, cfg, db.Tables(db.Table(&bookmark{}, db.Unique("uk_bm", "owner_id", "book_id"))))
	refusesTheHiddenTable(t, err, gdb, home, ahead)
}

// TestRegisterSchema_RefusesATableSearchPathHides: a battery's table is
// refused as the application's is.
func TestRegisterSchema_RefusesATableSearchPathHides(t *testing.T) {
	cfg, home, ahead, gdb := searchPathAhead(t, db.Tables(db.Table(&bookmark{})))
	d := dbtest.OpenConfig(t, cfg)
	err := d.RegisterSchema("battery", []db.TableSpec{db.Table(&bookmark{})}, nil)
	refusesTheHiddenTable(t, err, gdb, home, ahead)
}

// TestOpen_CreatesANewTableInCurrentSchema: a table that search_path
// finds nowhere is created in current_schema(), the first schema of
// search_path, with its index, and the next Open finds both there. A
// sequence of the name in a later schema is no table a query reads, so
// the new table hides nothing and Open creates it the same way.
func TestOpen_CreatesANewTableInCurrentSchema(t *testing.T) {
	t.Run("no relation of the name", func(t *testing.T) {
		cfg, home, ahead, gdb := searchPathAhead(t)
		tables := db.Tables(db.Table(&bookmark{}, db.Unique("uk_bm", "owner_id", "book_id")))
		for range 2 {
			if err := openErr(t, cfg, tables); err != nil {
				t.Fatal(err)
			}
		}
		if in := tableSchemas(t, gdb, "bookmarks", home, ahead); !slices.Equal(in, []string{ahead}) {
			t.Fatalf("bookmarks is created in current_schema() %s alone; it is in %v", ahead, in)
		}
		var in []string
		if err := gdb.Raw("SELECT schemaname FROM pg_indexes WHERE indexname = 'uk_bm' AND schemaname IN ?", []string{home, ahead}).Scan(&in).Error; err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(in, []string{ahead}) {
			t.Fatalf("uk_bm is on the table in %s; it is in %v", ahead, in)
		}
	})
	t.Run("a sequence of the name in a later schema", func(t *testing.T) {
		cfg, home, ahead, gdb := searchPathAhead(t)
		if err := gdb.Exec("CREATE SEQUENCE tags").Error; err != nil {
			t.Fatal(err)
		}
		if err := openErr(t, cfg, db.Tables(db.Table(&tag{}))); err != nil {
			t.Fatal(err)
		}
		if in := tableSchemas(t, gdb, "tags", home, ahead); !slices.Equal(in, []string{ahead}) {
			t.Fatalf("tags is created in current_schema() %s alone; it is in %v", ahead, in)
		}
	})
}
