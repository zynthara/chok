package db_test

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
)

// rbChild is a row of rb_children. Each parent model below declares the
// foreign key from rb_children.parent_id to rb_parents, which gorm puts on
// the child's table.
type rbChild struct {
	db.Model
	ParentID uint `gorm:"not null"`
}

// The parent models are rb_parents in the versions a migration goes
// through: rbParent, and models that differ from it in one place. The
// SQLite driver applies each difference by rebuilding the table.
type rbParent struct {
	db.Model
	Code     string    `gorm:"size:32;not null"`
	Name     string    `gorm:"size:32;not null"`
	Label    string    `gorm:"size:20"`
	Shelf    string    `gorm:"size:32"`
	Children []rbChild `gorm:"foreignKey:ParentID;constraint:OnDelete:CASCADE"`
}

// rbParentLabel makes label NOT NULL with a default, which gorm migrates
// with AlterColumn.
type rbParentLabel struct {
	db.Model
	Code     string    `gorm:"size:32;not null"`
	Name     string    `gorm:"size:32;not null"`
	Label    string    `gorm:"size:20;not null;default:'x'"`
	Shelf    string    `gorm:"size:32"`
	Children []rbChild `gorm:"foreignKey:ParentID;constraint:OnDelete:CASCADE"`
}

// rbParentUnique tags name unique, which gorm migrates with
// CreateConstraint (uni_rb_parents_name), and with DropConstraint when the
// tag goes again.
type rbParentUnique struct {
	db.Model
	Code     string    `gorm:"size:32;not null"`
	Name     string    `gorm:"size:32;not null;unique"`
	Label    string    `gorm:"size:20"`
	Shelf    string    `gorm:"size:32"`
	Children []rbChild `gorm:"foreignKey:ParentID;constraint:OnDelete:CASCADE"`
}

// rbParentNoAction and rbParentNoActionUnique are rbParent and
// rbParentUnique with the default action on the foreign key.
type rbParentNoAction struct {
	db.Model
	Code     string    `gorm:"size:32;not null"`
	Name     string    `gorm:"size:32;not null"`
	Label    string    `gorm:"size:20"`
	Shelf    string    `gorm:"size:32"`
	Children []rbChild `gorm:"foreignKey:ParentID"`
}

type rbParentNoActionUnique struct {
	db.Model
	Code     string    `gorm:"size:32;not null"`
	Name     string    `gorm:"size:32;not null;unique"`
	Label    string    `gorm:"size:20"`
	Shelf    string    `gorm:"size:32"`
	Children []rbChild `gorm:"foreignKey:ParentID"`
}

// rbParentAlone is rbParent without the has-many: rb_children gets no
// foreign key from it.
type rbParentAlone struct {
	db.Model
	Code  string `gorm:"size:32;not null"`
	Name  string `gorm:"size:32;not null"`
	Label string `gorm:"size:20"`
	Shelf string `gorm:"size:32"`
}

func (rbParent) TableName() string               { return "rb_parents" }
func (rbParentLabel) TableName() string          { return "rb_parents" }
func (rbParentUnique) TableName() string         { return "rb_parents" }
func (rbParentNoAction) TableName() string       { return "rb_parents" }
func (rbParentNoActionUnique) TableName() string { return "rb_parents" }
func (rbParentAlone) TableName() string          { return "rb_parents" }

// rbSpecs declares the parent model with UNIQUE(code), then the children:
// gorm adds the foreign key of the parent's has-many to the children's
// table once it has read the parent.
func rbSpecs(parent any) []db.TableSpec {
	return []db.TableSpec{db.Table(parent, db.Unique("uk_rb_parents_code", "code")), db.Table(&rbChild{})}
}

// rbTables is rbSpecs as an option of Open.
func rbTables(parent any) db.Option { return db.Tables(rbSpecs(parent)...) }

// rbSeed opens cfg with the parent model the schema has, adds a parent, a
// child of it, and an index and the trigger rbTrigger made by hand on
// rb_parents, and returns the parent's RID.
func rbSeed(t *testing.T, cfg db.Config, parent any) string {
	t.Helper()
	d, err := db.Open(cfg, testLogger(t), rbTables(parent))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close(t.Context()) }()
	gdb := d.Unsafe(t.Context())
	p := &rbParent{Code: "c1", Name: "n1", Label: "l1", Shelf: "s1"}
	if err := gdb.Create(p).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&rbChild{ParentID: p.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec("CREATE INDEX idx_rb_parents_hand ON rb_parents (shelf)").Error; err != nil {
		t.Fatal(err)
	}
	for _, ddl := range rbTriggerSQL() {
		if err := gdb.Exec(ddl).Error; err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	return p.RID
}

// rbTrigger is a trigger made by hand on rb_parents: updating a row sets
// its label to touched. rbTriggerSQL makes it in the dialect's syntax. A
// SQLite trigger cannot set NEW, so it updates the row again after the
// update, and its body names the label column; Postgres runs a function
// before the update, and MySQL sets NEW.
const rbTrigger = "trg_rb_parents_touch"

func rbTriggerSQL() []string {
	switch dbtest.Driver() {
	case "postgres":
		return []string{
			"CREATE FUNCTION rb_parents_touch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.label := 'touched'; RETURN NEW; END $$",
			"CREATE TRIGGER " + rbTrigger + " BEFORE UPDATE ON rb_parents FOR EACH ROW EXECUTE FUNCTION rb_parents_touch()",
		}
	case "mysql":
		return []string{"CREATE TRIGGER " + rbTrigger + " BEFORE UPDATE ON rb_parents FOR EACH ROW SET NEW.label = 'touched'"}
	}
	return []string{"CREATE TRIGGER " + rbTrigger + " AFTER UPDATE ON rb_parents BEGIN UPDATE rb_parents SET label = 'touched' WHERE id = NEW.id AND label <> 'touched'; END"}
}

// hasTrigger reports whether trigger name is on table, as the dialect's
// catalog lists it.
func hasTrigger(t *testing.T, gdb *gorm.DB, table, name string) bool {
	t.Helper()
	q := "SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND tbl_name = ? AND name = ?"
	switch dbtest.Driver() {
	case "postgres":
		q = "SELECT count(*) FROM information_schema.triggers WHERE trigger_schema = current_schema() AND event_object_table = ? AND trigger_name = ?"
	case "mysql":
		q = "SELECT count(*) FROM information_schema.triggers WHERE trigger_schema = DATABASE() AND event_object_table = ? AND trigger_name = ?"
	}
	var n int64
	if err := gdb.Raw(q, table, name).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// rbTouched updates the parent with code c1 and returns its label then,
// which rbTrigger sets to touched.
func rbTouched(t *testing.T, gdb *gorm.DB) string {
	t.Helper()
	if err := gdb.Exec("UPDATE rb_parents SET shelf = ? WHERE code = ?", "s2", "c1").Error; err != nil {
		t.Fatalf("update the parent: %v", err)
	}
	var label string
	if err := gdb.Raw("SELECT label FROM rb_parents WHERE code = ?", "c1").Scan(&label).Error; err != nil {
		t.Fatal(err)
	}
	return label
}

// indexesOver returns the indexes of table over column alone, as
// GetIndexes lists them.
func indexesOver(t *testing.T, gdb *gorm.DB, table, column string) []string {
	t.Helper()
	idx, err := gdb.Migrator().GetIndexes(table)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, i := range idx {
		if slices.Equal(i.Columns(), []string{column}) {
			names = append(names, i.Name())
		}
	}
	return names
}

// TestOpen_ChangingAModelKeepsIndexesAndRows: a change to a model that the
// SQLite driver migrates by rebuilding the table (copying it into
// rb_parents__temp, dropping it and renaming the copy) keeps what ALTER
// TABLE keeps on Postgres and MySQL. Open succeeds; the index and the
// trigger made by hand on the table stay, and the trigger still fires;
// the rows of the table and of the table whose foreign key refers to it
// stay; the index of the model's tag and the declared one are there once
// each and hold, as do the foreign key and the unique tag's constraint
// while the tag is on. Before, the rebuild lost the index and the trigger
// made by hand, and with foreign keys on, its DROP TABLE deleted the
// child rows where the key cascades and failed Open (787) where it takes
// the default action.
func TestOpen_ChangingAModelKeepsIndexesAndRows(t *testing.T) {
	for _, c := range []struct {
		name   string
		before []any // the parent models opened in turn before the rows and the index are added
		after  any   // the parent model opened then
		unique bool  // whether uni_rb_parents_name is on after
	}{
		{"alter column", []any{&rbParent{}}, &rbParentLabel{}, false},
		{"add unique tag, cascading key", []any{&rbParent{}}, &rbParentUnique{}, true},
		{"add unique tag, key with the default action", []any{&rbParentNoAction{}}, &rbParentNoActionUnique{}, true},
		{"remove unique tag", []any{&rbParent{}, &rbParentUnique{}}, &rbParent{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := dbtest.Config(t)
			for _, m := range c.before {
				if err := openErr(t, cfg, rbTables(m)); err != nil {
					t.Fatal(err)
				}
			}
			rid := rbSeed(t, cfg, c.before[len(c.before)-1])
			gdb := dbtest.OpenConfig(t, cfg, rbTables(c.after)).Unsafe(t.Context())

			var parents, children int64
			if err := gdb.Table("rb_parents").Count(&parents).Error; err != nil {
				t.Fatal(err)
			}
			if err := gdb.Table("rb_children").Count(&children).Error; err != nil {
				t.Fatal(err)
			}
			if parents != 1 || children != 1 {
				t.Errorf("the parent and its child stay on %s; got %d parents and %d children", dbtest.Driver(), parents, children)
			}
			for _, w := range []struct{ column, index string }{
				{"rid", "idx_rb_parents_r_id"},   // the uniqueIndex tag of the model base
				{"code", "uk_rb_parents_code"},   // declared with db.Unique
				{"shelf", "idx_rb_parents_hand"}, // made by hand
			} {
				if got := indexesOver(t, gdb, "rb_parents", w.column); !slices.Equal(got, []string{w.index}) {
					t.Errorf("one index over (%s), %s, on %s; got %v", w.column, w.index, dbtest.Driver(), got)
				}
			}

			if gdb.Create(&rbParent{RID: rid, Code: "c2", Name: "n2"}).Error == nil {
				t.Error("the index of the tag holds")
			}
			if gdb.Create(&rbParent{Code: "c1", Name: "n3"}).Error == nil {
				t.Error("the declared index holds")
			}
			if gdb.Create(&rbChild{ParentID: 1 << 30}).Error == nil {
				t.Error("the foreign key holds")
			}
			if has := gdb.Migrator().HasConstraint(&rbParentUnique{}, "uni_rb_parents_name"); has != c.unique {
				t.Errorf("uni_rb_parents_name is on: %v, want %v", has, c.unique)
			}
			if refused := gdb.Create(&rbParent{Code: "c4", Name: "n1"}).Error != nil; refused != c.unique {
				t.Errorf("a second parent named n1 refused: %v, want %v", refused, c.unique)
			}
			if !hasTrigger(t, gdb, "rb_parents", rbTrigger) {
				t.Errorf("the trigger made by hand, %s, stays on %s", rbTrigger, dbtest.Driver())
			}
			if got := rbTouched(t, gdb); got != "touched" {
				t.Errorf("the trigger made by hand sets the label of an updated row on %s; got %q", dbtest.Driver(), got)
			}
		})
	}
}

// TestDropColumn_UsedByATriggerFails: on SQLite, dropping a column that a
// trigger made by hand on the table uses fails and names the trigger, as
// SQLite's own ALTER TABLE DROP COLUMN does. The driver drops the column
// by rebuilding the table, whose DROP TABLE drops the trigger, and the
// trigger, made again, does not compile over the columns left, so it is
// dropped again. The error names each index and trigger that is not made
// again, with its SQL, and the ones after it are still made again.
// Afterwards the table can be updated, and another table can be rebuilt:
// SQLite checks every trigger of the schema when a rebuild renames its
// copy. Before, the trigger vanished without a word, and the first index
// or trigger that could not be made again kept the ones after it from
// being made again.
func TestDropColumn_UsedByATriggerFails(t *testing.T) {
	if dbtest.Driver() != "sqlite" {
		t.Skip("only SQLite drops a column by rebuilding the table, which drops its triggers")
	}
	labelIndex := "CREATE INDEX idx_rb_parents_label ON rb_parents (label)"
	insertTrigger := "CREATE TRIGGER trg_rb_parents_insert AFTER INSERT ON rb_parents BEGIN SELECT NEW.code; END"
	for _, c := range []struct {
		name string
		ddl  []string // made by hand after rbSeed, after its index and trigger
		lost []string // what the error says: each object not made again and its SQL
		kept []string // the objects made by hand that are made again
	}{
		{
			"a trigger", nil,
			[]string{"trigger " + rbTrigger, rbTriggerSQL()[0]},
			[]string{"idx_rb_parents_hand"},
		},
		{
			"a trigger and an index, then a trigger that compiles", []string{labelIndex, insertTrigger},
			[]string{"trigger " + rbTrigger, rbTriggerSQL()[0], "index idx_rb_parents_label", labelIndex},
			[]string{"idx_rb_parents_hand", "trg_rb_parents_insert"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := dbtest.Config(t)
			rbSeed(t, cfg, &rbParent{})
			gdb := dbtest.OpenConfig(t, cfg, rbTables(&rbParent{})).Unsafe(t.Context())
			for _, ddl := range c.ddl {
				if err := gdb.Exec(ddl).Error; err != nil {
					t.Fatalf("%s: %v", ddl, err)
				}
			}

			err := gdb.Migrator().DropColumn(&rbParent{}, "label")
			for _, w := range c.lost {
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Errorf("dropping label fails, and the error says %q; got %v", w, err)
				}
			}
			if hasTrigger(t, gdb, "rb_parents", rbTrigger) {
				t.Errorf("%s, which uses label, is not made again", rbTrigger)
			}
			for _, name := range c.kept {
				var n int64
				if err := gdb.Raw("SELECT count(*) FROM sqlite_master WHERE tbl_name = 'rb_parents' AND name = ?", name).Scan(&n).Error; err != nil {
					t.Fatal(err)
				}
				if n != 1 {
					t.Errorf("%s is made again", name)
				}
			}
			if err := gdb.Exec("UPDATE rb_parents SET shelf = ? WHERE code = ?", "s2", "c1").Error; err != nil {
				t.Errorf("the parent can be updated afterwards: %v", err)
			}
			if err := gdb.Migrator().AlterColumn(&rbChild{}, "parent_id"); err != nil {
				t.Errorf("rb_children can be rebuilt afterwards: %v", err)
			}
			var parents, children int64
			if err := gdb.Table("rb_parents").Count(&parents).Error; err != nil {
				t.Fatal(err)
			}
			if err := gdb.Table("rb_children").Count(&children).Error; err != nil {
				t.Fatal(err)
			}
			if parents != 1 || children != 1 {
				t.Errorf("the parent and its child stay; got %d parents and %d children", parents, children)
			}
		})
	}
}

// TestOpen_TriggerSpellingItsTableInAnotherCaseStays: on SQLite, a
// trigger whose CREATE TRIGGER spells its table in another case is made
// again when a change to the model rebuilds the table. The catalog keeps
// the table name of a trigger as the statement wrote it, and the rebuild
// looked the table's triggers up by the table's own spelling, so it lost
// this one.
func TestOpen_TriggerSpellingItsTableInAnotherCaseStays(t *testing.T) {
	if dbtest.Driver() != "sqlite" {
		t.Skip("only SQLite migrates a table by rebuilding it, which drops its triggers")
	}
	cfg := dbtest.Config(t)
	d, err := db.Open(cfg, testLogger(t), rbTables(&rbParent{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Unsafe(t.Context()).Exec("CREATE TRIGGER trg_rb_parents_spelled AFTER UPDATE ON RB_Parents BEGIN SELECT 1; END").Error; err != nil {
		t.Fatal(err)
	}
	_ = d.Close(t.Context())

	gdb := dbtest.OpenConfig(t, cfg, rbTables(&rbParentLabel{})).Unsafe(t.Context())
	var n int64
	if err := gdb.Raw("SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'trg_rb_parents_spelled'").Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the trigger made by hand on RB_Parents stays")
	}
}

// TestOpen_ForeignKeyOverRowsWithoutAParentFails: a relation added to a
// model whose children hold a row that refers to no parent asks for a
// foreign key the row breaks, and migrating fails without adding it, on
// every dialect: Postgres (23503) and MySQL (1452) refuse to add the key,
// and SQLite, whose driver adds it by rebuilding the table with foreign
// keys off, finds the row and drops the key again. The rows and the index
// made by hand on the table stay, and the SQLite writer has foreign keys
// on afterwards. Before, SQLite added the key over the row.
func TestOpen_ForeignKeyOverRowsWithoutAParentFails(t *testing.T) {
	cfg := dbtest.Config(t)
	d, err := db.Open(cfg, testLogger(t), rbTables(&rbParentAlone{}))
	if err != nil {
		t.Fatal(err)
	}
	gdb := d.Unsafe(t.Context())
	p := &rbParentAlone{Code: "c1", Name: "n1"}
	if err := gdb.Create(p).Error; err != nil {
		t.Fatal(err)
	}
	for _, c := range []*rbChild{{ParentID: p.ID}, {ParentID: 1 << 30}} { // the second has no parent
		if err := gdb.Create(c).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := gdb.Exec("CREATE INDEX idx_rb_children_hand ON rb_children (parent_id)").Error; err != nil {
		t.Fatal(err)
	}
	_ = d.Close(t.Context())

	switch err := openErr(t, cfg, rbTables(&rbParentNoAction{})); {
	case err == nil:
		t.Errorf("Open adds the foreign key over a row without a parent on %s", dbtest.Driver())
	case dbtest.Driver() == "sqlite":
		for _, want := range []string{"table rb_children has 1 row", "rb_parents", "fk_rb_parents_children"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error says %q: %v", want, err)
			}
		}
	}

	// RegisterSchema migrates an open handle the same way, which stays open
	// to look at afterwards.
	d = dbtest.OpenConfig(t, cfg, rbTables(&rbParentAlone{}))
	gdb = d.Unsafe(t.Context())
	if err := d.RegisterSchema("related", rbSpecs(&rbParentNoAction{}), nil); err == nil {
		t.Errorf("RegisterSchema adds the foreign key over a row without a parent on %s", dbtest.Driver())
	}
	if gdb.Migrator().HasConstraint(&rbChild{}, "fk_rb_parents_children") {
		t.Errorf("fk_rb_parents_children is not added on %s", dbtest.Driver())
	}
	var children int64
	if err := gdb.Table("rb_children").Count(&children).Error; err != nil {
		t.Fatal(err)
	}
	if children != 2 {
		t.Errorf("both children stay on %s; got %d", dbtest.Driver(), children)
	}
	if got := indexesOver(t, gdb, "rb_children", "parent_id"); !slices.Equal(got, []string{"idx_rb_children_hand"}) {
		t.Errorf("the index made by hand stays on %s; got %v", dbtest.Driver(), got)
	}
	if dbtest.Driver() == "sqlite" {
		var on int
		if err := gdb.Raw("PRAGMA foreign_keys").Scan(&on).Error; err != nil || on != 1 {
			t.Errorf("the writer has foreign keys on afterwards; got %d (%v)", on, err)
		}
	}
}

// cancelAt is a slog.Handler that calls cancel when the handle logs a
// statement starting with prefix, and records in seen that it did.
type cancelAt struct {
	prefix string
	cancel context.CancelFunc
	seen   *atomic.Bool
}

func (h cancelAt) Enabled(context.Context, slog.Level) bool { return true }
func (h cancelAt) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h cancelAt) WithGroup(string) slog.Handler            { return h }

func (h cancelAt) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "sql" && strings.HasPrefix(a.Value.String(), h.prefix) {
			h.seen.Store(true)
			h.cancel()
		}
		return true
	})
	return nil
}

// TestMigrate_EndedContextLeavesForeignKeysOn: a migration whose context
// ends while SQLite rebuilds a table fails and leaves the writer with
// foreign keys on, whether the context ends once they are off or inside
// the rebuild's transaction, which rolls back. Before, the keys went on
// again under the ended context, which failed without a word, and the
// writer went on without them.
func TestMigrate_EndedContextLeavesForeignKeysOn(t *testing.T) {
	if dbtest.Driver() != "sqlite" {
		t.Skip("only SQLite migrates a table by rebuilding it with foreign keys off")
	}
	for _, c := range []struct{ name, at string }{
		{"keys off", "PRAGMA foreign_keys = OFF"},
		{"inside the rebuild", "DROP TABLE `rb_parents`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := dbtest.Config(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var seen atomic.Bool
			d := dbtest.OpenConfig(t, cfg, db.Logger(slog.New(cancelAt{c.at, cancel, &seen})), rbTables(&rbParentLabel{}))
			gdb := d.Unsafe(t.Context())
			p := &rbParent{Code: "c1", Name: "n1", Label: "l1"}
			if err := gdb.Create(p).Error; err != nil {
				t.Fatal(err)
			}
			if err := gdb.Create(&rbChild{ParentID: p.ID}).Error; err != nil {
				t.Fatal(err)
			}
			// Another process migrates the table back, which leaves the
			// handle's Migrate a rebuild to do.
			if err := openErr(t, cfg, rbTables(&rbParent{})); err != nil {
				t.Fatal(err)
			}
			if seen.Load() {
				t.Fatal("the context ended before Migrate")
			}
			if err := d.Migrate(ctx); err == nil || !seen.Load() {
				t.Fatalf("Migrate fails once the context ends at %s; got %v", c.at, err)
			}

			var on int
			if err := gdb.Raw("PRAGMA foreign_keys").Scan(&on).Error; err != nil || on != 1 {
				t.Errorf("the writer has foreign keys on; got %d (%v)", on, err)
			}
			var parents, children int64
			if err := gdb.Table("rb_parents").Count(&parents).Error; err != nil {
				t.Fatal(err)
			}
			if err := gdb.Table("rb_children").Count(&children).Error; err != nil {
				t.Fatal(err)
			}
			if parents != 1 || children != 1 {
				t.Errorf("the rows stay; got %d parents and %d children", parents, children)
			}
			if gdb.Create(&rbChild{ParentID: 1 << 30}).Error == nil {
				t.Error("the foreign key holds")
			}
		})
	}
}
