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

// TestScanAndRender_EdgeFixtureGolden pins the expected-warning fixture
// package the same way: exact rendered bytes AND the exact warning set —
// the diagnostics are part of the generator's contract, not noise.
func TestScanAndRender_EdgeFixtureGolden(t *testing.T) {
	pkg, err := Scan(filepath.Join("fixture", "edge"))
	if err != nil {
		t.Fatal(err)
	}

	wantWarnFragments := []string{
		"Contact.Profile: `store` tag ignored", // relation skip (review round-1 Medium)
		"Contact.Badge: `store` tag ignored",   // wrong-signature Value() proves nothing (round-2)
		"Parent.Children: `store` tag ignored", // defined slice = has-many relation (round-2)
		"Event: embedded AuditBase carries",    // whole surface promoted — no silent vanishing
		"Entry: embedded Extra carries",        // named gorm-embedded promotion
		"Ticket: embedded Extra carries",       // ...even with an unexported target type (round-2)
	}
	if len(pkg.Warnings) != len(wantWarnFragments) {
		t.Fatalf("edge fixture must warn exactly %d times, got %q", len(wantWarnFragments), pkg.Warnings)
	}
	for _, frag := range wantWarnFragments {
		found := false
		for _, w := range pkg.Warnings {
			if strings.Contains(w, frag) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a warning containing %q, got %q", frag, pkg.Warnings)
		}
	}

	var names []string
	for _, m := range pkg.Models {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "Audit,AuditBase,Contact,Entry,Parent,Ticket,hiddenAudit" {
		t.Fatalf("edge models = %v (Event must be absent — its surface is promoted)", names)
	}

	got, err := Render(pkg)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("fixture", "edge", GenFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("fixture/edge/%s is stale — regenerate it (or the renderer drifted):\n--- got ---\n%s\n--- want ---\n%s", GenFileName, got, want)
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
	if len(pkg.Models) != 3 || pkg.Models[0].Name != "Article" || pkg.Models[1].Name != "ShadowID" || pkg.Models[2].Name != "Wallet" {
		t.Fatalf("models = %+v, want [Article ShadowID Wallet]", pkg.Models)
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
		"Code":         {value: "code", query: true},                   // anonymous local scalar embed = a regular column
		"Title":        {value: "title", query: true, update: true},    // json name, comma option truncated
		"Body":         {value: "body", update: true},                  // update-only
		"Secret":       {value: "secret", query: true},                 // json:"-" → default column
		"InternalNote": {value: "internal_note", query: true},          // no json tag → default column
		"HTTPStatus":   {value: "http_status", query: true},            // acronym via NamingStrategy
		"LegacyBody":   {value: "body_raw", update: true},              // explicit gorm column
		"Aliased":      {value: "aliased", query: true},                // aliased at store.New in the latch test
		"PublishedAt":  {value: "published_at", query: true},           // known cross-package column type
		"ID":           {value: "id", query: true, base: true},         // base trio
		"CreatedAt":    {value: "created_at", query: true, base: true}, //
		"UpdatedAt":    {value: "updated_at", query: true, base: true}, //
	})
	assertFields(t, pkg.Models[1], map[string]face{
		"PublicID":  {value: "id", query: true},                     // user field owns the "id" key
		"Name":      {value: "name", query: true, update: true},     //
		"Kind":      {value: "kind", query: true},                   // local defined scalar as a named field
		"CreatedAt": {value: "created_at", query: true, base: true}, // base ID skipped — name taken
		"UpdatedAt": {value: "updated_at", query: true, base: true}, //
	})
	assertFields(t, pkg.Models[2], map[string]face{
		"Money":     {value: "money", query: true},                  // anonymous driver.Valuer embed = a column
		"Flags":     {value: "flags", query: true, update: true},    // GormDataType struct = a column
		"ID":        {value: "id", query: true, base: true},         //
		"CreatedAt": {value: "created_at", query: true, base: true}, //
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

// TestScan_PromotedEmbedWarns pins the promotion contract: an exported
// local struct embed carrying store tags warns (GORM promotes what the
// scan cannot expand), an unexported embed stays silent (GORM skips
// unexported fields entirely — verified against the runtime), and a
// verified tag-free embed stays silent too (nothing promotable).
func TestScan_PromotedEmbedWarns(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Base struct {
	Common string `+"`store:\"query\"`"+`
}

type hidden struct {
	Silent string `+"`store:\"query\"`"+`
}

type Mixin struct {
	Note string `+"`json:\"note\"`"+`
}

type Post struct {
	db.Model
	Base
	hidden
	Mixin
	Title string `+"`store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	var base, hid, mixin bool
	for _, w := range pkg.Warnings {
		if !strings.Contains(w, "Post") {
			continue
		}
		base = base || strings.Contains(w, "Base")
		hid = hid || strings.Contains(w, "hidden")
		mixin = mixin || strings.Contains(w, "Mixin")
	}
	if !base {
		t.Fatalf("tagged exported embed must warn about promotion, got %q", pkg.Warnings)
	}
	if hid {
		t.Fatalf("unexported embeds never reach GORM — warning is a false alarm: %q", pkg.Warnings)
	}
	if mixin {
		t.Fatalf("verified tag-free embeds must stay silent, got %q", pkg.Warnings)
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

// TestScan_SilentPromotionStillWarns is the review round-1 Medium: a
// struct whose WHOLE tag surface rides a promoted embed is a runtime
// model the generator cannot represent — it must warn, not vanish. A
// DTO wrapping a tagged model (no direct chok base embed) must NOT
// warn: model-ness is signaled by the db base.
func TestScan_SilentPromotionStillWarns(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type AuditBase struct {
	Actor string `+"`json:\"actor\" store:\"query\"`"+`
}

type Event struct {
	db.Model
	AuditBase
}

type Post struct {
	db.Model
	Title string `+"`json:\"title\" store:\"query\"`"+`
}

type PostResponse struct {
	Post
	Extra string `+"`json:\"extra\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range pkg.Models {
		if m.Name == "Event" || m.Name == "PostResponse" {
			t.Fatalf("%s must not scan as a model", m.Name)
		}
	}
	var eventWarn, dtoWarn bool
	for _, w := range pkg.Warnings {
		if strings.Contains(w, "Event") {
			eventWarn = true
		}
		if strings.Contains(w, "PostResponse") {
			dtoWarn = true
		}
	}
	if !eventWarn {
		t.Fatalf("a base-embedding struct with only promoted tags must warn, got %q", pkg.Warnings)
	}
	if dtoWarn {
		t.Fatalf("a DTO wrapping a model (no direct db base) must stay silent, got %q", pkg.Warnings)
	}
}

// TestScan_EmbeddedWrapperPromotionWarns covers the named form: a
// gorm-embedded field promotes its target's tags at runtime whether or
// not the wrapper itself carries a store tag.
func TestScan_EmbeddedWrapperPromotionWarns(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Audit struct {
	By string `+"`json:\"by\" store:\"query\"`"+`
}

type Entry struct {
	db.Model
	Extra Audit `+"`gorm:\"embedded\"`"+`
	Title string `+"`json:\"title\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	var warned bool
	for _, w := range pkg.Warnings {
		if strings.Contains(w, "Entry") && strings.Contains(w, "Extra") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("untagged gorm-embedded wrapper with tagged target must warn, got %q", pkg.Warnings)
	}
}

// TestScan_ColumnShapeClassification is the review round-1/round-2
// Medium in unit form: relation shapes — including defined slices and
// defined-type chains resolving to structs — are skipped with a
// warning; method-proven columns (exact driver.Valuer, GormDataType)
// and the known cross-package set are included; a Value method with
// the wrong signature proves nothing.
func TestScan_ColumnShapeClassification(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"database/sql"
	"database/sql/driver"
	"time"

	"github.com/zynthara/chok/v2/db"
	"gorm.io/gorm"
)

type Profile struct {
	ID uint
}

// Sibling resolves to Profile through a defined-type chain — still a
// relation.
type Sibling Profile

type Child struct {
	ID     uint
	PostID uint
}

// Children is a defined slice: a has-many relation, not a scalar.
type Children []Child

// Money implements driver.Valuer exactly — a column.
type Money struct {
	cents int64
}

func (m Money) Value() (driver.Value, error) { return m.cents, nil }

// Badge has a Value method with the WRONG signature — not a Valuer,
// still a relation (review round-2).
type Badge struct {
	ID uint
}

func (Badge) Value() (int, error) { return 0, nil }

// Flags carries its column type via GormDataType — a column even
// though it is a struct (review round-2).
type Flags struct {
	V uint8
}

func (Flags) GormDataType() string { return "smallint" }

type Post struct {
	db.Model
	Profile  Profile        `+"`json:\"profile\" store:\"query\"`"+`
	Twin     Sibling        `+"`json:\"twin\" store:\"query\"`"+`
	Children Children       `+"`json:\"children\" store:\"query\"`"+`
	Tags     []string       `+"`json:\"tags\" store:\"query\"`"+`
	Badge    Badge          `+"`json:\"badge\" store:\"query\"`"+`
	Price    Money          `+"`json:\"price\" store:\"query\"`"+`
	Flags    Flags          `+"`json:\"flags\" store:\"query\"`"+`
	Seen     sql.NullTime   `+"`json:\"seen\" store:\"query\"`"+`
	Gone     gorm.DeletedAt `+"`json:\"gone\" store:\"query\"`"+`
	Due      *time.Time     `+"`json:\"due\" store:\"query\"`"+`
	Raw      []byte         `+"`json:\"raw\" store:\"query\"`"+`
	Title    string         `+"`json:\"title\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	var post Model
	for _, m := range pkg.Models {
		if m.Name == "Post" {
			post = m
		}
	}
	got := map[string]bool{}
	for _, f := range post.Fields {
		if !f.Base {
			got[f.GoName] = true
		}
	}
	for _, want := range []string{"Price", "Flags", "Seen", "Gone", "Due", "Raw", "Title"} {
		if !got[want] {
			t.Errorf("%s must be classified as a column, got %v", want, got)
		}
	}
	for _, absent := range []string{"Profile", "Twin", "Children", "Tags", "Badge"} {
		if got[absent] {
			t.Errorf("%s is not a column at runtime and must be skipped, got %v", absent, got)
		}
	}
	for _, wantWarn := range []string{"Post.Profile", "Post.Twin", "Post.Children", "Post.Tags", "Post.Badge"} {
		var warned bool
		for _, w := range pkg.Warnings {
			warned = warned || strings.Contains(w, wantWarn)
		}
		if !warned {
			t.Errorf("skipped relation shape %s must warn, got %q", wantWarn, pkg.Warnings)
		}
	}
}

// TestScan_UnexportedEmbeddedTargetWarns is review round-2 finding 3:
// a named gorm-embedded wrapper promotes by FIELD name — the target
// TYPE's exportedness is irrelevant, so an unexported target with tags
// must still warn.
func TestScan_UnexportedEmbeddedTargetWarns(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type audit struct {
	Actor string `+"`json:\"actor\" store:\"query\"`"+`
}

type Entry struct {
	db.Model
	Extra audit `+"`gorm:\"embedded\"`"+`
	Title string `+"`json:\"title\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	var warned bool
	for _, w := range pkg.Warnings {
		if strings.Contains(w, "Entry") && strings.Contains(w, "Extra") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("gorm-embedded wrapper with an unexported tagged target must warn (GORM promotes by field name), got %q", pkg.Warnings)
	}
}

// TestScan_AnonymousValuerEmbedIsField is review round-2 finding 2c:
// an anonymous embed of a real driver.Valuer struct is a column at
// runtime, so it must generate a field instead of being treated as a
// promoted embed.
func TestScan_AnonymousValuerEmbedIsField(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"database/sql/driver"

	"github.com/zynthara/chok/v2/db"
)

type Money struct {
	cents int64
}

func (m Money) Value() (driver.Value, error) { return m.cents, nil }

type Wallet struct {
	db.Model
	Money `+"`json:\"money\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Warnings) != 0 {
		t.Fatalf("a Valuer embed is a plain column — no warnings, got %q", pkg.Warnings)
	}
	var found bool
	for _, m := range pkg.Models {
		if m.Name != "Wallet" {
			continue
		}
		for _, f := range m.Fields {
			if f.GoName == "Money" && f.Value == "money" && f.Query {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("anonymous Valuer embed must generate a query field, got %+v", pkg.Models)
	}
}

func TestScan_AmbiguousCrossPackageTypeFailsLoud(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/shopspring/decimal"

type Order struct {
	Amount decimal.Decimal `+"`json:\"amount\" store:\"query\"`"+`
}
`)
	_, err := Scan(dir)
	if err == nil || !strings.Contains(err.Error(), "Order.Amount") || !strings.Contains(err.Error(), `gorm:"type:`) {
		t.Fatalf("undecidable cross-package type must fail loud pointing at the static proof, got %v", err)
	}

	// The gorm type tag (or a serializer) is that proof.
	writeGo(t, dir, "m.go", `package m

import "github.com/shopspring/decimal"

type Order struct {
	Amount decimal.Decimal `+"`json:\"amount\" store:\"query\" gorm:\"type:decimal(20,8)\"`"+`
	Meta   map[string]any  `+"`json:\"meta\" store:\"query\" gorm:\"serializer:json\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range pkg.Models[0].Fields {
		got[f.GoName] = true
	}
	if !got["Amount"] || !got["Meta"] {
		t.Fatalf("type/serializer tags prove column-ness for any Go type, got %v", got)
	}
}

// TestScan_BuildConstraintsHonored: files Go itself would not build are
// invisible to the scan (review round-1 Low).
func TestScan_BuildConstraintsHonored(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

type Post struct {
	Title string `+"`json:\"title\" store:\"query\"`"+`
}
`)
	writeGo(t, dir, "excluded.go", `//go:build mips64 && ignore

package other

type Excluded struct {
	X string `+"`store:\"query\"`"+`
}

var PostFields = 1
`)
	writeGo(t, dir, "_hidden.go", `package other2

type Hidden struct {
	Y string `+"`store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Models) != 1 || pkg.Models[0].Name != "Post" {
		t.Fatalf("constraint-excluded and underscore files must be invisible (their package clause, models and symbols), got %+v", pkg.Models)
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
