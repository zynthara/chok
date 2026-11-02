package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
	"github.com/zynthara/chok/internal/dbx"
)

type note struct {
	db.Model
	Title string `gorm:"size:100;not null"`
}

func (note) RIDPrefix() string { return "nte" }

type tag struct {
	db.SoftDeleteModel
	Name  string `gorm:"size:50;not null"`
	Scope string `gorm:"size:50;not null;default:''"`
}

type mine struct {
	db.OwnedModel
	Body string `gorm:"size:100;not null"`
}

type mineSoft struct {
	db.OwnedSoftDeleteModel
	Body string `gorm:"size:100;not null"`
}

// Wrapped embeds SoftDeleteModel one level down.
type Base struct{ db.SoftDeleteModel }

type wrapped struct {
	Base
	Code string `gorm:"size:20;not null"`
}

type hidden struct{ db.SoftDeleteModel }

type viaUnexported struct {
	hidden
	Code string `gorm:"size:20;not null"`
}

func TestOpen_AutoMigratesTables(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&note{}), db.Table(&tag{}, db.SoftUnique("uk_tag_name", "name", "scope"))))
	m := d.Unsafe(t.Context()).Migrator()
	if !m.HasTable(&note{}) || !m.HasTable(&tag{}) || !m.HasIndex(&tag{}, "uk_tag_name") {
		t.Fatal("auto mode creates the tables and their SoftUnique indexes")
	}
	if err := d.Migrate(t.Context()); err != nil {
		t.Fatalf("migrating again is a no-op: %v", err)
	}
}

func TestOpen_ValidatesFirst(t *testing.T) {
	if _, err := db.Open(db.Config{Driver: "sqlite"}); err == nil || !strings.Contains(err.Error(), "sqlite.path") {
		t.Fatalf("got %v", err)
	}
	if _, err := db.Open(db.Config{Driver: "sqlite", SQLite: db.SQLiteConfig{Path: ":memory:"}}, db.Owner("Bad Owner")); err == nil {
		t.Fatal("an owner must be a plain lower-case name")
	}
}

func TestBeforeCreate_SetsVersionAndRID(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&note{}), db.Table(&tag{})))
	ctx := t.Context()
	n := &note{Title: "a"}
	if err := d.Unsafe(ctx).Create(n).Error; err != nil {
		t.Fatal(err)
	}
	if p, ok := db.ParseRID(n.RID); !ok || p != "nte" || n.Version != 1 {
		t.Fatalf("got %+v", n.Model)
	}
	batch := []*note{{Title: "b"}, {Title: "c"}}
	if err := d.Unsafe(ctx).Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(batch[1].RID, "nte_") || batch[0].RID == batch[1].RID {
		t.Fatalf("a batch gets distinct prefixed RIDs: %q %q", batch[0].RID, batch[1].RID)
	}
	tg := &tag{Name: "x"}
	if err := d.Unsafe(ctx).Create(tg).Error; err != nil || strings.Contains(tg.RID, "_") {
		t.Fatalf("no prefix, no underscore: %q %v", tg.RID, err)
	}
	if err := d.Unsafe(ctx).Create(&note{RID: "bad rid!", Title: "x"}).Error; err == nil {
		t.Fatal("a malformed preset RID is refused")
	}
}

func TestTable_RejectsNonModels(t *testing.T) {
	type plain struct{ ID uint }
	type badPrefix struct {
		db.Model
	}
	for name, fn := range map[string]func(){
		"not a struct":    func() { db.Table(42) },
		"no model base":   func() { db.Table(&plain{}) },
		"nil":             func() { db.Table(nil) },
		"bad prefix":      func() { db.Table(&prefixed{}) },
		"soft on hard":    func() { db.Table(&note{}, db.SoftUnique("uk", "title")) },
		"missing column":  func() { db.Table(&tag{}, db.SoftUnique("uk", "nope")) },
		"nullable column": func() { db.Table(&nullable{}, db.SoftUnique("uk", "label")) },
		"no not null":     func() { db.Table(&loose{}, db.SoftUnique("uk", "label")) },
		"empty index":     func() { db.SoftUnique("uk") },
		"unexported base": func() { db.Table(&viaUnexported{}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: must panic", name)
				}
			}()
			fn()
		}()
	}
	_ = badPrefix{}
}

type prefixed struct{ db.Model }

func (prefixed) RIDPrefix() string { return "Bad-Prefix" }

type nullable struct {
	db.SoftDeleteModel
	Label *string `gorm:"size:20"`
}

type loose struct {
	db.SoftDeleteModel
	Label string `gorm:"size:20"`
}

type acronym struct {
	db.SoftDeleteModel
	APIKey string `gorm:"size:40;not null"`
}

func TestTable_SoftUniqueUsesDatabaseColumnNames(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&acronym{}, db.SoftUnique("uk_api_key", "api_key"))))
	if !d.Unsafe(t.Context()).Migrator().HasIndex(&acronym{}, "uk_api_key") {
		t.Fatal("columns are named as gorm names them: APIKey is api_key")
	}
}

// TestSoftUnique_Behaviour: live rows conflict, a soft-deleted row frees
// its values, and the index has the same shape on every dialect.
func TestSoftUnique_Behaviour(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&tag{}, db.SoftUnique("uk_tag_name", "name", "scope"))))
	gdb := d.Unsafe(t.Context())
	first := &tag{Name: "go", Scope: "lang"}
	if err := gdb.Create(first).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&tag{Name: "go", Scope: "lang"}).Error; err == nil {
		t.Fatal("a second live row with the same values must conflict")
	}
	if err := gdb.Create(&tag{Name: "go", Scope: "game"}).Error; err != nil {
		t.Fatalf("other values do not conflict: %v", err)
	}
	// What store's soft delete does: stamp a token, then delete.
	if err := gdb.Model(first).Update("delete_token", db.NewRID("del")[4:]).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Delete(first).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&tag{Name: "go", Scope: "lang"}).Error; err != nil {
		t.Fatalf("a deleted row frees its values: %v", err)
	}
	cols := indexColumns(t, gdb, "tags", "uk_tag_name")
	slices.Sort(cols)
	if !slices.Equal(cols, []string{"delete_token", "name", "scope"}) {
		t.Fatalf("UNIQUE(name, scope, delete_token) on %s, got %v", dbtest.Driver(), cols)
	}
}

func indexColumns(t *testing.T, gdb *gorm.DB, table, index string) []string {
	t.Helper()
	idx, err := gdb.Migrator().GetIndexes(table)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range idx {
		if i.Name() == index {
			if u, ok := i.Unique(); ok && !u {
				t.Fatalf("%s is not unique", index)
			}
			return i.Columns()
		}
	}
	t.Fatalf("no index %s on %s", index, table)
	return nil
}

func TestSoftUnique_IndirectSoftDelete(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&wrapped{}, db.SoftUnique("uk_wrapped_code", "code"))))
	if !d.Unsafe(t.Context()).Migrator().HasIndex(&wrapped{}, "uk_wrapped_code") {
		t.Fatal("a soft-delete base embedded one level down counts")
	}
}

// bookmark has no soft delete and an owner: the framework declares
// owner_id, so only Table can put it in a composite unique index.
type bookmark struct {
	db.OwnedModel
	BookID string `gorm:"size:24;not null"`
}

func newBookmark(owner, book string) *bookmark {
	b := &bookmark{BookID: book}
	b.SetOwnerID(owner)
	return b
}

// TestUnique_OwnedModel: Unique gives a model without soft delete
// UNIQUE(owner_id, book_id), the same on every dialect. Migrating again,
// on the handle or from a second process, finds the index in the catalog
// as declared and keeps it.
func TestUnique_OwnedModel(t *testing.T) {
	cfg := dbtest.Config(t)
	tables := db.Tables(db.Table(&bookmark{}, db.Unique("uk_bookmark_owner_book", "owner_id", "book_id")))
	d := dbtest.OpenConfig(t, cfg, tables)
	gdb := d.Unsafe(t.Context())
	if err := gdb.Create(newBookmark("u1", "b1")).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(newBookmark("u1", "b1")).Error; err == nil {
		t.Fatal("an owner holds a book once")
	}
	if err := gdb.Create(newBookmark("u2", "b1")).Error; err != nil {
		t.Fatalf("another owner may hold the same book: %v", err)
	}
	cols := indexColumns(t, gdb, "bookmarks", "uk_bookmark_owner_book")
	slices.Sort(cols)
	if !slices.Equal(cols, []string{"book_id", "owner_id"}) {
		t.Fatalf("UNIQUE(owner_id, book_id) on %s, got %v", dbtest.Driver(), cols)
	}
	if err := d.Migrate(t.Context()); err != nil {
		t.Fatalf("migrating again is a no-op: %v", err)
	}
	again := dbtest.OpenConfig(t, cfg, tables)
	if err := again.Unsafe(t.Context()).Create(newBookmark("u1", "b1")).Error; err == nil {
		t.Fatal("a second open keeps the index")
	}
	if n := len(indexColumns(t, again.Unsafe(t.Context()), "bookmarks", "uk_bookmark_owner_book")); n != 2 {
		t.Fatalf("a second open leaves the index as it is: %d columns", n)
	}
}

// member declares both kinds on one soft-delete model: a login that is
// never reused and an email a delete releases.
type member struct {
	db.SoftDeleteModel
	Login string `gorm:"size:50;not null"`
	Email string `gorm:"size:100;not null"`
}

// TestUnique_HeldAfterSoftDelete: on a soft-delete model Unique counts
// every row, UNIQUE(login), so a deleted row keeps its login, while
// SoftUnique counts live rows, UNIQUE(email, delete_token), so the same
// delete releases the email.
func TestUnique_HeldAfterSoftDelete(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&member{},
		db.Unique("uk_member_login", "login"),
		db.SoftUnique("uk_member_email", "email"))))
	gdb := d.Unsafe(t.Context())
	first := &member{Login: "ann", Email: "ann@x"}
	if err := gdb.Create(first).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&member{Login: "ann", Email: "other@x"}).Error; err == nil {
		t.Fatal("a live login conflicts")
	}
	// What store's soft delete does: stamp a token, then delete.
	if err := gdb.Model(first).Update("delete_token", db.NewRID("del")[4:]).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Delete(first).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&member{Login: "ann", Email: "new@x"}).Error; err == nil {
		t.Fatal("a deleted row still holds its Unique login")
	}
	if err := gdb.Create(&member{Login: "bea", Email: "ann@x"}).Error; err != nil {
		t.Fatalf("a deleted row frees its SoftUnique email: %v", err)
	}
	login := indexColumns(t, gdb, "members", "uk_member_login")
	email := indexColumns(t, gdb, "members", "uk_member_email")
	slices.Sort(email)
	if !slices.Equal(login, []string{"login"}) || !slices.Equal(email, []string{"delete_token", "email"}) {
		t.Fatalf("UNIQUE(login) and UNIQUE(email, delete_token) on %s, got %v and %v", dbtest.Driver(), login, email)
	}
}

// nullSQL is a model without soft delete and a sql.Null column.
type nullSQL struct {
	db.Model
	Label sql.NullString `gorm:"size:20"`
}

// TestTable_IndexDeclarations: Unique takes a model with any base,
// SoftUnique only one with soft delete; both check their columns alike
// and their panics name the kind.
func TestTable_IndexDeclarations(t *testing.T) {
	db.Table(&note{}, db.Unique("uk_note_title", "title")) // a model without soft delete takes Unique
	db.Table(&tag{}, db.Unique("uk_tag_name", "name"), db.SoftUnique("uk_tag_scope", "scope"))
	for _, c := range []struct {
		name, want string
		fn         func()
	}{
		{"unique without a name", "db.Unique: needs an index name and at least one column", func() { db.Unique("", "title") }},
		{"unique without columns", "db.Unique: needs an index name and at least one column", func() { db.Unique("n") }},
		{"unique without either", "db.Unique: needs an index name and at least one column", func() { db.Unique("") }},
		{"soft unique without a name", "db.SoftUnique: needs an index name and at least one column", func() { db.SoftUnique("", "name") }},
		{"the zero Index", "db.Table: note: an index needs a name and at least one column", func() { db.Table(&note{}, db.Index{}) }},
		{"unique, missing column", `db.Table: note: Unique uk_x: no column "nope"`, func() { db.Table(&note{}, db.Unique("uk_x", "nope")) }},
		{"soft unique, missing column", `db.Table: tag: SoftUnique uk_x: no column "nope"`, func() { db.Table(&tag{}, db.SoftUnique("uk_x", "nope")) }},
		{"unique, pointer column", `db.Table: nullable: Unique uk_x: column "label" is nullable`, func() { db.Table(&nullable{}, db.Unique("uk_x", "label")) }},
		{"unique, sql.Null column", `db.Table: nullSQL: Unique uk_x: column "label" is nullable`, func() { db.Table(&nullSQL{}, db.Unique("uk_x", "label")) }},
		{"soft unique, pointer column", `db.Table: nullable: SoftUnique uk_x: column "label" is nullable`, func() { db.Table(&nullable{}, db.SoftUnique("uk_x", "label")) }},
		{"unique, no not null", `db.Table: loose: Unique uk_x: column "label" needs gorm:"not null"`, func() { db.Table(&loose{}, db.Unique("uk_x", "label")) }},
		{"soft unique without soft delete", "db.Table: note: SoftUnique needs a model with soft delete", func() { db.Table(&note{}, db.SoftUnique("uk_x", "title")) }},
		{"soft unique after a unique, without soft delete", "db.Table: note: SoftUnique needs a model with soft delete", func() {
			db.Table(&note{}, db.Unique("uk_ok", "title"), db.SoftUnique("uk_x", "title"))
		}},
	} {
		func() {
			defer func() {
				r := recover()
				if msg := fmt.Sprint(r); r == nil || !strings.Contains(msg, c.want) {
					t.Errorf("%s: panic %v, want one containing %q", c.name, r, c.want)
				}
			}()
			c.fn()
		}()
	}
}

func TestOwned_ColumnsAndAccessors(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&mine{}), db.Table(&mineSoft{})))
	m := d.Unsafe(t.Context()).Migrator()
	if !m.HasColumn(&mine{}, "owner_id") || !m.HasColumn(&mineSoft{}, "owner_id") || !m.HasColumn(&mineSoft{}, "delete_token") {
		t.Fatal("owned bases add owner_id")
	}
	var o interface {
		GetOwnerID() string
		SetOwnerID(string)
	} = &mine{}
	o.SetOwnerID("u1")
	if o.GetOwnerID() != "u1" {
		t.Fatal("owner accessors")
	}
	o = &mineSoft{}
	o.SetOwnerID("u2")
	if o.GetOwnerID() != "u2" {
		t.Fatal("owner accessors on the soft base")
	}
}

func TestRunInTx_CommitRollbackAndPanic(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&note{})))
	ctx := t.Context()
	count := func() int64 {
		var n int64
		if err := d.Unsafe(ctx).Model(&note{}).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := d.RunInTx(ctx, func(tx context.Context) error {
		if !d.InTx(tx) || d.InTx(ctx) {
			t.Error("InTx sees only the transaction's context")
		}
		return d.Unsafe(tx).Create(&note{Title: "kept"}).Error
	}); err != nil || count() != 1 {
		t.Fatalf("commit: %v %d", err, count())
	}
	boom := errors.New("boom")
	if err := d.RunInTx(ctx, func(tx context.Context) error {
		_ = d.Unsafe(tx).Create(&note{Title: "dropped"}).Error
		return boom
	}); !errors.Is(err, boom) || count() != 1 {
		t.Fatalf("rollback: %v %d", err, count())
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("a panic is re-raised")
			}
		}()
		_ = d.RunInTx(ctx, func(tx context.Context) error {
			_ = d.Unsafe(tx).Create(&note{Title: "panicked"}).Error
			panic("inside")
		})
	}()
	if count() != 1 {
		t.Fatal("a panic rolls back")
	}
}

func TestRunInTx_NestedJoinsOuter(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&note{})))
	ctx := t.Context()
	err := d.RunInTx(ctx, func(outer context.Context) error {
		if err := d.RunInTx(outer, func(inner context.Context) error {
			return d.Unsafe(inner).Create(&note{Title: "inner"}).Error
		}); err != nil {
			return err
		}
		return errors.New("outer fails")
	})
	var n int64
	d.Unsafe(ctx).Model(&note{}).Count(&n)
	if err == nil || n != 0 {
		t.Fatalf("the inner work belongs to the outer transaction: %v %d", err, n)
	}
}

func TestRunInTx_OtherHandleIsNotJoined(t *testing.T) {
	a := dbtest.Open(t, db.Tables(db.Table(&note{})))
	b := dbtest.Open(t, db.Tables(db.Table(&note{})))
	_ = a.RunInTx(t.Context(), func(tx context.Context) error {
		if b.InTx(tx) {
			t.Error("a transaction of a belongs to a only")
		}
		return nil
	})
}

func TestAfterCommit(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&note{})))
	ctx := t.Context()
	if d.AfterCommit(ctx, func(context.Context) {}) {
		t.Fatal("outside a transaction AfterCommit does nothing")
	}
	var mu sync.Mutex
	var ran []string
	record := func(s string) func(context.Context) {
		return func(c context.Context) {
			mu.Lock()
			ran = append(ran, s)
			mu.Unlock()
			if d.InTx(c) {
				t.Error("callbacks run outside the transaction")
			}
		}
	}
	_ = d.RunInTx(ctx, func(tx context.Context) error {
		d.AfterCommit(tx, record("a"))
		_ = d.RunInTx(tx, func(inner context.Context) error {
			d.AfterCommit(inner, record("b"))
			return nil
		})
		if len(ran) != 0 {
			t.Error("nothing runs before the outer commit")
		}
		return nil
	})
	if !slices.Equal(ran, []string{"a", "b"}) {
		t.Fatalf("after commit, in order, once: %v", ran)
	}
	ran = nil
	_ = d.RunInTx(ctx, func(tx context.Context) error {
		d.AfterCommit(tx, record("dropped"))
		return errors.New("rollback")
	})
	func() {
		defer func() { _ = recover() }()
		_ = d.RunInTx(ctx, func(tx context.Context) error {
			d.AfterCommit(tx, record("panicked"))
			panic("x")
		})
	}()
	if len(ran) != 0 {
		t.Fatalf("rollback and panic drop the callbacks: %v", ran)
	}
}

func TestHealthCloseNameLogger(t *testing.T) {
	d := dbtest.Open(t)
	if err := d.Health(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d.Name() != "db" || d.Logger() == nil {
		t.Fatal("name and logger")
	}
	if err := d.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := d.Health(t.Context()); err == nil {
		t.Fatal("a closed handle is unhealthy")
	}
}

func TestStorePolicy_IsACopy(t *testing.T) {
	cfg := dbtest.Config(t)
	on := true
	cfg.Store.AdminRoles = []string{"admin"}
	cfg.Store.OwnerScope = &on
	d := dbtest.OpenConfig(t, cfg)
	p := d.StorePolicy()
	p.AdminRoles[0] = "changed"
	*p.OwnerScope = false
	if got := d.StorePolicy(); got.AdminRoles[0] != "admin" || !*got.OwnerScope {
		t.Fatal("the policy handed out must not alias the handle's")
	}
	// Nor does the handle alias the configuration it was opened with.
	cfg.Store.AdminRoles[0] = "mallory"
	on = false
	if got := d.StorePolicy(); got.AdminRoles[0] != "admin" || !*got.OwnerScope {
		t.Fatalf("editing the config after Open changed the handle: %+v", got)
	}
}

// TestOpen_UTCSession pins the time baseline: MySQL sessions and Postgres
// connections run in UTC whatever the host's zone.
func TestOpen_UTCSession(t *testing.T) {
	d := dbtest.Open(t)
	gdb := d.Unsafe(t.Context())
	var zone string
	switch dbtest.Driver() {
	case "mysql":
		if err := gdb.Raw("SELECT @@session.time_zone").Row().Scan(&zone); err != nil {
			t.Fatal(err)
		}
		if zone != "+00:00" {
			t.Fatalf("session time_zone %q", zone)
		}
	case "postgres":
		if err := gdb.Raw("SHOW TIME ZONE").Row().Scan(&zone); err != nil {
			t.Fatal(err)
		}
		if zone != "UTC" && zone != "Etc/UTC" {
			t.Fatalf("TimeZone %q", zone)
		}
	default:
		t.Skip("SQLite has no session time zone")
	}
	_ = time.UTC
}

// TestCreate_StampsUTC pins gorm's clock to UTC: SQLite keeps timestamps
// as text with their offset and compares them as text, so stamps taken in
// the local zone would misorder across a DST change. The location check
// holds on any host: time.Now() carries time.Local, never the time.UTC
// pointer, even where the local zone is UTC.
func TestCreate_StampsUTC(t *testing.T) {
	d := dbtest.Open(t, db.Tables(db.Table(&note{})))
	gdb := d.Unsafe(t.Context())
	if loc := gdb.NowFunc().Location(); loc != time.UTC {
		t.Fatalf("gorm's clock runs in %v", loc)
	}
	n := &note{Title: "a"}
	if err := gdb.Create(n).Error; err != nil {
		t.Fatal(err)
	}
	if n.CreatedAt.Location() != time.UTC || n.UpdatedAt.Location() != time.UTC {
		t.Fatalf("stamped in %v / %v", n.CreatedAt.Location(), n.UpdatedAt.Location())
	}
	if dbtest.Driver() == "sqlite" {
		var raw string
		if err := gdb.Raw("SELECT CAST(created_at AS TEXT) FROM notes").Row().Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(raw, "+00:00") {
			t.Fatalf("stored %q", raw)
		}
	}
}

// entry is an append-only row with no model base, the shape of the
// batteries' audit log.
type entry struct {
	ID     string `gorm:"primaryKey;size:24"`
	Action string `gorm:"size:64;not null;index"`
}

type noKey struct {
	Action string `gorm:"size:64"`
}

func TestPlainTable_DeclaresABaselessTable(t *testing.T) {
	spec, ok := dbx.PlainTable(&entry{}).(db.TableSpec)
	if !ok {
		t.Fatalf("dbx.PlainTable returns a db.TableSpec, got %T", dbx.PlainTable(&entry{}))
	}
	d := dbtest.Open(t)
	if err := d.RegisterSchema("battery", []db.TableSpec{spec}, nil); err != nil {
		t.Fatal(err)
	}
	gdb := d.Unsafe(t.Context())
	if !gdb.Migrator().HasTable(&entry{}) || gdb.Migrator().HasColumn(&entry{}, "rid") {
		t.Fatal("auto mode creates the table with only its own columns")
	}
	if err := gdb.Create(&entry{ID: db.NewRID("ent"), Action: "login"}).Error; err != nil {
		t.Fatal(err)
	}
	for name, model := range map[string]any{"a model": &note{}, "no primary key": &noKey{}, "not a struct": new(int)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: dbx.PlainTable must panic", name)
				}
			}()
			dbx.PlainTable(model)
		}()
	}
}
