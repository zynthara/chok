package db_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/internal/testschema"
	"github.com/zynthara/chok/v2/kernel"
)

// External test package on purpose: the fixtures ride choktest, which
// imports store → db; an in-package test would cycle.

type Widget struct {
	db.Model
	Label string `json:"label" gorm:"size:100"`
}

func (Widget) RIDPrefix() string { return "wgt" }

const sqliteAutoYAML = `
db:
  driver: sqlite
  sqlite:
    path: ":memory:"
`

func TestModule_AutoMigratesAndServesHandle(t *testing.T) {
	tk := choktest.NewTestKernel(t, sqliteAutoYAML,
		db.Module(db.WithTables(db.Table(&Widget{}))),
	)

	h := db.From(tk)
	ctx := context.Background()
	if !h.Unsafe(ctx).Migrator().HasTable(&Widget{}) {
		t.Fatal("migrate: auto must AutoMigrate WithTables specs during the kernel Migrate phase")
	}
	if err := h.Unsafe(ctx).Create(&Widget{Label: "a"}).Error; err != nil {
		t.Fatal(err)
	}

	// Two-value path for absence-aware callers.
	c, ok := kernel.Get[*db.Component](tk, "db")
	if !ok || c.Handle() != h {
		t.Fatal("chok.Get two-value path must reach the same handle")
	}

	// Healther contract: bounded ping.
	if err := c.Health(ctx); err != nil {
		t.Fatalf("Health on a live pool: %v", err)
	}
}

func TestModule_VersionedAppliesEmbeddedMigrations(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_widgets.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE widgets (id BIGINT PRIMARY KEY, label VARCHAR(100));")},
	}
	tk := choktest.NewTestKernel(t, `
db:
  driver: sqlite
  migrate: versioned
  sqlite:
    path: ":memory:"
`,
		db.Module(db.WithMigrations(fsys)),
	)

	h := db.From(tk)
	ctx := context.Background()
	if !h.Unsafe(ctx).Migrator().HasTable("widgets") {
		t.Fatal("versioned mode must apply the embedded migrations at startup")
	}
	st, err := db.MigrationsStatus(ctx, h, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Applied) != 1 || len(st.Pending) != 0 {
		t.Fatalf("ledger after start: %+v", st)
	}
}

func TestModule_SchemaOwnershipMatchesVersionedLedger(t *testing.T) {
	fsys := fstest.MapFS{
		"README.txt": &fstest.MapFile{Data: []byte("no application migrations")},
	}
	component := db.Module(db.WithMigrations(fsys))
	tk := choktest.NewTestKernel(t, `
db:
  driver: sqlite
  migrate: versioned
  sqlite:
    path: ":memory:"
`, component)
	testschema.AssertOwnership(t, db.From(tk), component)
}

func TestModule_VersionedWithoutSourceFailsStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.yaml")
	if err := os.WriteFile(path, []byte("db:\n  driver: sqlite\n  migrate: versioned\n  sqlite:\n    path: \":memory:\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loader := conf.NewLoader("t", "TVERS")
	loader.SetPath(path)
	if err := loader.Register("db", db.Options{}); err != nil {
		t.Fatal(err)
	}
	store, err := conf.NewStore(loader)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := kernel.New(kernel.Config{Store: store, Components: []kernel.Component{db.Module()}})
	if err != nil {
		t.Fatal(err)
	}
	err = reg.Start(context.Background())
	if err == nil {
		_ = reg.Stop(context.Background())
		t.Fatal("versioned without WithMigrations must fail startup")
	}
	if !strings.Contains(err.Error(), "versioned requires db.WithMigrations") {
		t.Fatalf("error must say what is missing, got %v", err)
	}
}

func TestModule_OffTouchesNoSchema(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
db:
  driver: sqlite
  migrate: "off"
  sqlite:
    path: ":memory:"
`,
		db.Module(db.WithTables(db.Table(&Widget{}))),
	)

	h := db.From(tk)
	if h.Unsafe(context.Background()).Migrator().HasTable(&Widget{}) {
		t.Fatal("migrate: off means the framework touches no schema — WithTables must not AutoMigrate")
	}
}

func TestModule_NamedInstances(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
db:
  driver: sqlite
  sqlite:
    path: ":memory:"
  instances:
    read:
      driver: sqlite
      sqlite:
        path: ":memory:"
`,
		db.Module(db.WithTables(db.Table(&Widget{}))),
		db.Module(db.As("read")),
	)

	ctx := context.Background()
	main := db.From(tk)
	read := db.From(tk, "read")
	if main == read {
		t.Fatal("named instance must be a distinct pool")
	}
	// Auto-migrated table exists on default, not on read (separate
	// databases, separate schema strategies).
	if !main.Unsafe(ctx).Migrator().HasTable(&Widget{}) {
		t.Fatal("default instance should have the widget table")
	}
	if read.Unsafe(ctx).Migrator().HasTable(&Widget{}) {
		t.Fatal("read instance must be a different database without the table")
	}
	if err := read.Ping(ctx); err != nil {
		t.Fatalf("read instance ping: %v", err)
	}
}

func TestFrom_PanicsWithGuidanceWhenAbsent(t *testing.T) {
	tk := choktest.NewTestKernel(t, "") // no db module at all

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("db.From on a kernel without the db module must panic (assembly error)")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "chok.Use(db.Module") || !strings.Contains(msg, "chok.Get") {
			t.Fatalf("panic must carry assembly guidance and the graceful alternative, got %q", msg)
		}
	}()
	db.From(tk)
}

func TestModule_DisabledIsInvisible(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
db:
  enabled: false
  driver: sqlite
  sqlite:
    path: ":memory:"
`,
		db.Module(),
	)

	if _, ok := kernel.Get[*db.Component](tk, "db"); ok {
		t.Fatal("disabled db module must be invisible to Lookup")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("db.From must panic for a disabled module")
		}
	}()
	db.From(tk)
}
