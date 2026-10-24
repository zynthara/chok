package where_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
	"github.com/zynthara/chok/v3/store/where"
)

// item is the table the options run against on every lane.
type item struct {
	db.Model
	Name string  `gorm:"size:50;not null"`
	N    int     `gorm:"not null;default:0"`
	Note *string `gorm:"size:50"`
}

// fields is the public name → column map a store would pass.
var fields = map[string]string{"id": "rid", "name": "name", "n": "n", "note": "note"}

// seed opens a fresh database of the lane's dialect with the given names,
// item i holding N = i and a Note only on even i.
func seed(t *testing.T, names ...string) *gorm.DB {
	t.Helper()
	gdb := dbtest.Open(t, db.Tables(db.Table(&item{}))).Unsafe(t.Context())
	for i, name := range names {
		it := &item{Name: name, N: i}
		if i%2 == 0 {
			note := fmt.Sprintf("note %d", i)
			it.Note = &note
		}
		if err := gdb.Create(it).Error; err != nil {
			t.Fatal(err)
		}
	}
	return gdb
}

// apply runs opts the way a store does and returns what the query
// matched, in insertion order, with the Config the options filled in.
func apply(t *testing.T, gdb *gorm.DB, opts ...where.Option) ([]string, *where.Config, error) {
	t.Helper()
	cfg := &where.Config{}
	tx := gdb.Model(&item{})
	for _, o := range opts {
		var err error
		if tx, err = o(tx, cfg, fields); err != nil {
			return nil, cfg, err
		}
	}
	var rows []item
	if err := tx.Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	names := []string{}
	for _, r := range rows {
		names = append(names, r.Name)
	}
	return names, cfg, nil
}

func match(t *testing.T, gdb *gorm.DB, want []string, opts ...where.Option) *where.Config {
	t.Helper()
	got, cfg, err := apply(t, gdb, opts...)
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("matched %q, want %q", got, want)
	}
	return cfg
}

func TestComparisons(t *testing.T) {
	gdb := seed(t, "a", "b", "c", "d")
	match(t, gdb, []string{"c"}, where.Eq("n", 2))
	match(t, gdb, []string{"a", "b", "d"}, where.Ne("n", 2))
	match(t, gdb, []string{"c", "d"}, where.Gt("n", 1))
	match(t, gdb, []string{"b", "c", "d"}, where.Gte("n", 1))
	match(t, gdb, []string{"a"}, where.Lt("n", 1))
	match(t, gdb, []string{"a", "b"}, where.Lte("n", 1))
	match(t, gdb, []string{"b"}, where.Gt("n", 0), where.Lt("n", 2)) // options AND together
	cfg := match(t, gdb, []string{"b"}, where.Eq("id", must(t, gdb, "b").RID))
	if !cfg.HasFilter || cfg.Degenerate {
		t.Fatalf("a filter is recorded: %+v", cfg)
	}
	// A nil value matches nothing rather than rendering col = NULL.
	cfg = match(t, gdb, []string{}, where.Eq("note", nil))
	if !cfg.HasFilter || !cfg.Degenerate {
		t.Fatalf("Eq(nil) is degenerate: %+v", cfg)
	}
}

func must(t *testing.T, gdb *gorm.DB, name string) item {
	t.Helper()
	var it item
	if err := gdb.Where("name = ?", name).First(&it).Error; err != nil {
		t.Fatal(err)
	}
	return it
}

func TestIn(t *testing.T) {
	gdb := seed(t, "a", "b", "c")
	match(t, gdb, []string{"a", "c"}, where.In("n", 0, 2))
	match(t, gdb, []string{"a", "c"}, where.In("name", []string{"a", "c"})) // one slice is unwrapped
	match(t, gdb, []string{"b"}, where.In("n", []int{1}))
	for _, empty := range []where.Option{where.In("n"), where.In("n", nil), where.In("n", []int{})} {
		if cfg := match(t, gdb, []string{}, empty); !cfg.Degenerate || !cfg.HasFilter {
			t.Fatalf("an empty IN matches nothing: %+v", cfg)
		}
	}
	at := make([]int, 500)
	for i := range at {
		at[i] = i
	}
	match(t, gdb, []string{"a", "b", "c"}, where.In("n", at))
	over := append(at, 500)
	if _, _, err := apply(t, gdb, where.In("n", over)); !errors.Is(err, where.ErrInvalidParam) || !strings.Contains(err.Error(), "501") {
		t.Fatalf("501 values: %v", err)
	}
	args := make([]any, 501)
	for i := range args {
		args[i] = i
	}
	if _, _, err := apply(t, gdb, where.In("n", args...)); !errors.Is(err, where.ErrInvalidParam) {
		t.Fatalf("501 variadic values: %v", err)
	}
}

func TestNullAndNotNull(t *testing.T) {
	gdb := seed(t, "a", "b", "c")
	match(t, gdb, []string{"b"}, where.Null("note"))
	cfg := match(t, gdb, []string{"a", "c"}, where.NotNull("note"))
	if !cfg.HasFilter || cfg.Degenerate {
		t.Fatalf("a null test is a filter: %+v", cfg)
	}
}

// TestLike_WildcardsAndLiterals pins the two families on every lane:
// Like keeps the caller's wildcards, the literal matchers escape them —
// with '!' as the escape character, which MySQL, PostgreSQL and SQLite
// all read the same way (a backslash breaks MySQL's string literal).
func TestLike_WildcardsAndLiterals(t *testing.T) {
	gdb := seed(t, "100%_off", "100xyoff", "a!b", `a\b`, "Hello")
	match(t, gdb, []string{"100%_off", "100xyoff"}, where.Like("name", "100%"))
	match(t, gdb, []string{"100%_off", "100xyoff"}, where.Like("name", "100__off"))
	match(t, gdb, []string{"100%_off"}, where.Contains("name", "%_"))
	match(t, gdb, []string{"100%_off"}, where.StartsWith("name", "100%"))
	match(t, gdb, []string{"100%_off", "100xyoff"}, where.EndsWith("name", "off"))
	match(t, gdb, []string{"a!b"}, where.Contains("name", "!"))
	match(t, gdb, []string{"a!b"}, where.EndsWith("name", "!b"))
	match(t, gdb, []string{`a\b`}, where.EndsWith("name", `\b`))
	match(t, gdb, []string{}, where.Contains("name", "_x"))
	// Case follows the dialect, as the godoc of Like says.
	want := []string{"Hello"}
	if dbtest.Driver() == "postgres" {
		want = []string{}
	}
	match(t, gdb, want, where.Contains("name", "hello"))
	match(t, gdb, want, where.Like("name", "hel%"))
}

func TestFields_UnknownAndTainted(t *testing.T) {
	gdb := seed(t, "a")
	for name, o := range map[string]where.Option{
		"Eq":         where.Eq("nope", 1),
		"In":         where.In("nope", 1),
		"Like":       where.Like("nope", "x"),
		"Contains":   where.Contains("nope", "x"),
		"StartsWith": where.StartsWith("nope", "x"),
		"EndsWith":   where.EndsWith("nope", "x"),
		"Null":       where.Null("nope"),
		"NotNull":    where.NotNull("nope"),
		"Order":      where.Order("nope"),
	} {
		if _, _, err := apply(t, gdb, o); !errors.Is(err, where.ErrUnknownField) || !strings.Contains(err.Error(), `"nope"`) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A column that is not a plain identifier never reaches the SQL, even
	// when the field map declares it.
	for _, col := range []string{"name; DROP TABLE items", "name'", "name--", "na me", "0col", "a.b.c", "a..b", ""} {
		o := where.Eq("f", 1)
		_, err := o(gdb.Model(&item{}), &where.Config{}, map[string]string{"f": col})
		if !errors.Is(err, where.ErrUnknownField) {
			t.Errorf("column %q: %v", col, err)
		}
	}
	for _, col := range []string{"name", "items.name", "_name"} {
		o := where.Eq("f", "a")
		if _, err := o(gdb.Model(&item{}), &where.Config{}, map[string]string{"f": col}); err != nil {
			t.Errorf("safe column %q: %v", col, err)
		}
	}
}

// TestOrder_RecordsClauses: Order changes no SQL by itself; it records
// the clauses, in the order given, for the store to render with its
// tie-break.
func TestOrder_RecordsClauses(t *testing.T) {
	gdb := seed(t, "b", "a")
	cfg := match(t, gdb, []string{"b", "a"}, where.Order("name"), where.Order("n", true), where.Order("id", false))
	type clause struct {
		Field, Column string
		Desc          bool
	}
	var got []clause
	for _, o := range cfg.Orders {
		got = append(got, clause{o.Field, o.Column, o.Desc})
	}
	want := []clause{{"name", "name", false}, {"n", "n", true}, {"id", "rid", false}}
	if !slices.Equal(got, want) || !cfg.HasOrder || cfg.HasFilter {
		t.Fatalf("orders %+v (%+v)", got, cfg)
	}
}

func TestPagination(t *testing.T) {
	gdb := seed(t, "a")
	for name, o := range map[string]where.Option{
		"page 0":          where.Page(0, 10),
		"negative size":   where.Page(1, -1),
		"size over 10000": where.Page(1, 10_001),
		"offset overflow": where.Page(1<<40, 10_000),
		"limit 0":         where.Limit(0),
		"limit 10001":     where.Limit(10_001),
		"negative offset": where.Offset(-1),
		"cursor size 0":   where.Cursor("", 0),
		"cursor size big": where.Cursor("x", 10_001),
		"version 0":       where.Version(0),
	} {
		if _, _, err := apply(t, gdb, o); !errors.Is(err, where.ErrInvalidParam) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Pagination is recorded for the store to render; size 0 is the
	// store's default page size.
	_, cfg, _ := apply(t, gdb, where.Page(3, 10))
	if !cfg.HasPage || cfg.PageNumber != 3 || cfg.Limit != 10 || cfg.Offset != 20 {
		t.Fatalf("Page(3, 10): %+v", cfg)
	}
	_, cfg, _ = apply(t, gdb, where.Page(2, 0))
	if !cfg.HasPage || cfg.PageNumber != 2 || cfg.Limit != 0 {
		t.Fatalf("Page(2, 0) defers the size: %+v", cfg)
	}
	_, cfg, _ = apply(t, gdb, where.Offset(7), where.Limit(5))
	if cfg.PageNumber != 0 || cfg.Offset != 7 || cfg.Limit != 5 {
		t.Fatalf("Offset and Limit: %+v", cfg)
	}
	_, cfg, _ = apply(t, gdb, where.Cursor("", 5))
	if !cfg.FirstPage || cfg.Cursor != "" || cfg.CursorSize != 5 || !cfg.HasPage {
		t.Fatalf("first cursor page: %+v", cfg)
	}
	_, cfg, _ = apply(t, gdb, where.Cursor("tok", 5))
	if cfg.FirstPage || cfg.Cursor != "tok" {
		t.Fatalf("next cursor page: %+v", cfg)
	}
}

func TestModifiers(t *testing.T) {
	gdb := seed(t, "a")
	_, cfg, _ := apply(t, gdb, where.Count(), where.Preload("Author"), where.Preload("Tags"), where.WithTrashed(), where.Version(3))
	if !cfg.Count || !slices.Equal(cfg.Preloads, []string{"Author", "Tags"}) || !cfg.WithTrashed || cfg.OnlyTrashed || cfg.Version != 3 {
		t.Fatalf("modifiers: %+v", cfg)
	}
	_, cfg, _ = apply(t, gdb, where.OnlyTrashed())
	if !cfg.OnlyTrashed || cfg.HasFilter {
		t.Fatalf("OnlyTrashed: %+v", cfg)
	}
	// A count or an update asks for filters only: Count is then a no-op.
	cfg = &where.Config{FiltersOnly: true}
	if _, err := where.Count()(gdb, cfg, fields); err != nil || cfg.Count {
		t.Fatalf("Count under FiltersOnly: %+v %v", cfg, err)
	}
}
