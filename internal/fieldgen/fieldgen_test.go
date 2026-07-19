package fieldgen

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeGo drops one source file into dir for tempdir-based scan cases.
// go/parser never resolves imports, so the snippets don't need to compile
// as a module — only to parse.
func writeGo(t *testing.T, dir, name, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScanAndRender_FixturePackageGolden pins the generator's bytes: the
// checked-in fixture/chok_fields_gen.go must be exactly what Scan+Render
// produce from fixture/fixture.go today. The same file compiles into the
// fixture package, so the latch test in store/ exercises the very
// symbols this golden pins.
func TestScanAndRender_FixturePackageGolden(t *testing.T) {
	pkg, err := Scan("fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Warnings) != 0 {
		t.Fatalf("fixture scan must be warning-free, got %q", pkg.Warnings)
	}
	got, err := Render(pkg)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("fixture", GenFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("fixture/%s is stale — regenerate it (or the renderer drifted):\n--- got ---\n%s\n--- want ---\n%s", GenFileName, got, want)
	}

	again, err := Render(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, again) {
		t.Fatal("Render must be byte-stable across runs")
	}
}

// TestScan_FixtureSurfaces asserts the structured scan result — model
// set, per-field faces and public-name derivation — independent of the
// rendered bytes, so a failure names the exact field instead of a diff.
func TestScan_FixtureSurfaces(t *testing.T) {
	pkg, err := Scan("fixture")
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Name != "fixture" {
		t.Fatalf("package name = %q, want fixture", pkg.Name)
	}
	if len(pkg.Models) != 2 || pkg.Models[0].Name != "Article" || pkg.Models[1].Name != "ShadowID" {
		t.Fatalf("models = %+v, want [Article ShadowID]", pkg.Models)
	}

	type face struct {
		value         string
		query, update bool
		base          bool
	}
	assertFields := func(t *testing.T, m Model, want map[string]face) {
		t.Helper()
		if len(m.Fields) != len(want) {
			t.Errorf("%s: %d fields, want %d (%+v)", m.Name, len(m.Fields), len(want), m.Fields)
		}
		for _, f := range m.Fields {
			w, ok := want[f.GoName]
			if !ok {
				t.Errorf("%s.%s: unexpected generated field (value %q)", m.Name, f.GoName, f.Value)
				continue
			}
			if f.Value != w.value || f.Query != w.query || f.Update != w.update || f.Base != w.base {
				t.Errorf("%s.%s = {value:%q query:%v update:%v base:%v}, want %+v",
					m.Name, f.GoName, f.Value, f.Query, f.Update, f.Base, w)
			}
		}
	}

	assertFields(t, pkg.Models[0], map[string]face{
		"Title":        {value: "title", query: true, update: true},    // json name, comma option truncated
		"Body":         {value: "body", update: true},                  // update-only
		"Secret":       {value: "secret", query: true},                 // json:"-" → default column
		"InternalNote": {value: "internal_note", query: true},          // no json tag → default column
		"HTTPStatus":   {value: "http_status", query: true},            // acronym via NamingStrategy
		"LegacyBody":   {value: "body_raw", update: true},              // explicit gorm column
		"Aliased":      {value: "aliased", query: true},                // aliased at store.New in the latch test
		"ID":           {value: "id", query: true, base: true},         // base trio
		"CreatedAt":    {value: "created_at", query: true, base: true}, //
		"UpdatedAt":    {value: "updated_at", query: true, base: true}, //
	})
	assertFields(t, pkg.Models[1], map[string]face{
		"PublicID":  {value: "id", query: true},                     // user field owns the "id" key
		"Name":      {value: "name", query: true, update: true},     //
		"CreatedAt": {value: "created_at", query: true, base: true}, // base ID skipped — name taken
		"UpdatedAt": {value: "updated_at", query: true, base: true}, //
	})

	for _, m := range pkg.Models {
		for _, f := range m.Fields {
			if f.Value == "version" {
				t.Errorf("%s.%s: version must never be generated", m.Name, f.GoName)
			}
			if f.Base && f.Update {
				t.Errorf("%s.%s: base fields are query-only", m.Name, f.GoName)
			}
		}
	}
}

func TestScan_SkipsTestAndGenFiles(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "models.go", `package m

type Post struct {
	Title string `+"`json:\"title\" store:\"query\"`"+`
}
`)
	writeGo(t, dir, "decoy_test.go", `package m

type TestOnly struct {
	A string `+"`store:\"query\"`"+`
}
`)
	writeGo(t, dir, "decoy_gen.go", `package m

type GenOnly struct {
	B string `+"`store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Models) != 1 || pkg.Models[0].Name != "Post" {
		t.Fatalf("models = %+v, want [Post] only", pkg.Models)
	}
}

func TestScan_NoTaggedModels(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "dto.go", `package m

type Request struct {
	Title string `+"`json:\"title\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Models) != 0 || len(pkg.Warnings) != 0 {
		t.Fatalf("untagged structs must stay silent, got %+v %q", pkg.Models, pkg.Warnings)
	}
	if _, err := Render(pkg); err == nil {
		t.Fatal("Render on an empty scan must refuse")
	}
}

func TestScan_BadStoreTagValue(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

type Post struct {
	Title string `+"`store:\"qeury\"`"+`
}
`)
	_, err := Scan(dir)
	if err == nil || !strings.Contains(err.Error(), `"qeury"`) || !strings.Contains(err.Error(), "Post.Title") {
		t.Fatalf("bad tag value must fail loud naming the field, got %v", err)
	}
}

func TestScan_DuplicateNameDifferentColumns(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

type Post struct {
	A string `+"`json:\"same\" store:\"query\"`"+`
	B string `+"`json:\"same\" store:\"query\"`"+`
}
`)
	_, err := Scan(dir)
	if err == nil || !strings.Contains(err.Error(), `"same"`) {
		t.Fatalf("duplicate public name over two columns must error, got %v", err)
	}
}

func TestScan_ExistingFieldsSymbolConflicts(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

var PostFields = 1

type Post struct {
	Title string `+"`store:\"query\"`"+`
}
`)
	_, err := Scan(dir)
	if err == nil || !strings.Contains(err.Error(), "PostFields") {
		t.Fatalf("pre-existing <Model>Fields declaration must fail loud, got %v", err)
	}
}

func TestScan_UnknownAnonymousEmbedWarns(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type base struct {
	Common string `+"`store:\"query\"`"+`
}

type Post struct {
	db.Model
	base
	Title string `+"`store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	// base itself is unexported but still a tagged top-level struct — it
	// scans as a model too; the point here is Post's embed warning.
	var warned bool
	for _, w := range pkg.Warnings {
		if strings.Contains(w, "base") && strings.Contains(w, "Post") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("unknown anonymous embed must warn, got %q", pkg.Warnings)
	}
	for _, m := range pkg.Models {
		if m.Name == "Post" {
			for _, f := range m.Fields {
				if f.GoName == "Common" {
					t.Fatal("embedded struct fields must not be generated (syntax-level scan)")
				}
			}
		}
	}
}

func TestScan_AliasedDBImportRecognized(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import chokdb "github.com/zynthara/chok/v2/db"

type Post struct {
	chokdb.OwnedSoftDeleteModel
	Title string `+"`store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Warnings) != 0 {
		t.Fatalf("aliased chok db base embeds are known — no warning, got %q", pkg.Warnings)
	}
}

func TestScan_GormIgnoreVariants(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

type Post struct {
	Gone      string `+"`store:\"query\" gorm:\"-\"`"+`
	GoneAll   string `+"`store:\"query\" gorm:\"-:all\"`"+`
	Kept      string `+"`store:\"query\" gorm:\"-:migration\"`"+`
	Wrapped   string `+"`store:\"query\" gorm:\"embedded\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Models) != 1 {
		t.Fatalf("models = %+v", pkg.Models)
	}
	var names []string
	for _, f := range pkg.Models[0].Fields {
		if !f.Base {
			names = append(names, f.GoName)
		}
	}
	if len(names) != 1 || names[0] != "Kept" {
		t.Fatalf("gorm:\"-\"/-:all/embedded must be skipped (runtime never maps them), kept = %v", names)
	}
	var warned bool
	for _, w := range pkg.Warnings {
		if strings.Contains(w, "Wrapped") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("store tag on a gorm-embedded field must warn, got %q", pkg.Warnings)
	}
}

func TestScan_MultiNameAndUnexportedFields(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

type Post struct {
	A, B   string `+"`store:\"query\"`"+`
	hidden string `+"`store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range pkg.Models[0].Fields {
		if !f.Base {
			got[f.GoName] = f.Value
		}
	}
	if len(got) != 2 || got["A"] != "a" || got["B"] != "b" {
		t.Fatalf("multi-name declarations share the tag, unexported fields are dead to GORM; got %v", got)
	}
}

func TestScan_ParseErrorFailsLoud(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "broken.go", "package m\n\ntype Post struct {\n")
	if _, err := Scan(dir); err == nil {
		t.Fatal("syntax errors must fail the scan — silently dropping models would fake a clean --check")
	}
}

func TestScan_MixedPackagesFailLoud(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "a.go", "package a\n")
	writeGo(t, dir, "b.go", "package b\n")
	if _, err := Scan(dir); err == nil {
		t.Fatal("mixed package clauses in one directory must error")
	}
}
