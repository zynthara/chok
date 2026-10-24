package db_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
	"github.com/zynthara/chok/v3/internal/dbx"
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
