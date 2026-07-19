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
		"Contact.Sticker: `store` tag ignored", // defined type over a Valuer loses the method set (round-3)
		"Contact.Purse: `store` tag ignored",   // same-depth Value ambiguity = no Valuer (round-4)
		"Contact.Chest: `store` tag ignored",   // shallow wrong-sig Value shadows the promoted one (round-4)
		"Contact.Satchel: `store` tag ignored", // two same-depth PATHS to one Valuer = ambiguous too (round-5)
		"Contact.Parcel: `store` tag ignored",  // alias to *driver.Value keeps the pointer = not a Valuer (round-5)
		"Parent.Children: `store` tag ignored", // defined slice = has-many relation (round-2)
		"Event: embedded AuditBase carries",    // whole surface promoted — no silent vanishing
		"Entry: embedded Extra carries",        // named gorm-embedded promotion
		"Ticket: embedded Extra carries",       // ...even with an unexported target type (round-2)
		"Player.Level: `store` tag ignored",    // anonymous GormDataType (non-time/bytes) still expands (round-3/4)
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
	if strings.Join(names, ",") != "Audit,AuditBase,Contact,Entry,Parent,Player,Ticket,hiddenAudit" {
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
		"Clock":     {value: "clock", query: true},                  // anonymous GormDataType "time" = exempt = a column (round-4)
		"Bytes":     {value: "chunk", query: true},                  // anonymous generic instantiation = bytes column (round-5)
		"Flags":     {value: "flags", query: true, update: true},    // named GormDataType struct = a column
		"Box":       {value: "box", query: true},                    // embed-promoted Valuer = a column (round-3)
		"Seal":      {value: "seal", query: true},                   // defined time.Time = convertible = a column (round-3)
		"Token":     {value: "token", query: true},                  // [16]byte = bytes column (round-3)
		"Meta":      {value: "meta", query: true},                   // gorm:"json" serializer shorthand (round-3)
		"Locker":    {value: "locker", query: true},                 // alias-signature Valuer (round-4)
		"Payload":   {value: "payload", query: true},                // Bytes[byte] generic instantiation (round-4)
		"Vault":     {value: "vault", query: true},                  // alias-embedded cross-package Valuer (round-5)
		"Strip":     {value: "strip", query: true},                  // defined slice over a byte alias (round-5)
		"Slab":      {value: "slab", query: true},                   // []Byte alias element (round-5)
		"Packed":    {value: "packed", query: true},                 // generic instantiated with the byte alias (round-5)
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

// TestScan_MethodSetSemantics is review round-3 finding 1: Go's real
// method-set rules. A defined type does NOT inherit its source type's
// methods (Sticker = relation), an alias DOES (identity), and a struct
// embedding a Valuer promotes the method (Box = column).
func TestScan_MethodSetSemantics(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"database/sql/driver"

	"github.com/zynthara/chok/v2/db"
)

type Money struct {
	ID    uint
	Cents int64
}

func (m Money) Value() (driver.Value, error) { return m.Cents, nil }

type Sticker Money

type Alias = Money

type Box struct {
	Money
}

type Wrapped Box

type Post struct {
	db.Model
	StickerID uint    `+"`json:\"sticker_id\" store:\"query\"`"+`
	Sticker   Sticker `+"`json:\"sticker\" store:\"query\"`"+`
	Twin      Alias   `+"`json:\"twin\" store:\"query\"`"+`
	Box       Box     `+"`json:\"box\" store:\"query\"`"+`
	Wrapped   Wrapped `+"`json:\"wrapped\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range pkg.Models {
		if m.Name != "Post" {
			continue
		}
		for _, f := range m.Fields {
			if !f.Base {
				got[f.GoName] = true
			}
		}
	}
	// Alias keeps Money's identity; Box promotes Value from the embed;
	// Wrapped (defined over Box) keeps the promotion — it rides Box's
	// struct shape, not Box's name.
	for _, want := range []string{"Twin", "Box", "Wrapped"} {
		if !got[want] {
			t.Errorf("%s must be a column (method set carries), got %v", want, got)
		}
	}
	if got["Sticker"] {
		t.Errorf("Sticker must be a relation — defined types do not inherit methods, got %v", got)
	}
	var warned bool
	for _, w := range pkg.Warnings {
		warned = warned || strings.Contains(w, "Post.Sticker")
	}
	if !warned {
		t.Fatalf("the de-methoded relation must warn, got %q", pkg.Warnings)
	}
}

// TestScan_AnonymousStructEmbedRules is review round-3 finding 2: for
// ANONYMOUS struct-shaped fields only a real driver.Valuer stays a
// column — GormDataType, serializer and type tags do not prevent GORM's
// embedded expansion, so the store tag on the embed line is dead.
func TestScan_AnonymousStructEmbedRules(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"time"

	"github.com/zynthara/chok/v2/db"
)

type Flags struct{ V uint8 }

func (Flags) GormDataType() string { return "smallint" }

type Blob struct{ X string }

type Typed struct{ Y string }

type Clock time.Time

type Player struct {
	db.Model
	Flags `+"`json:\"flags\" store:\"query\"`"+`
	Blob  `+"`json:\"blob\" store:\"query\" gorm:\"serializer:json\"`"+`
	Typed `+"`json:\"typed\" store:\"query\" gorm:\"type:jsonb\"`"+`
	Clock `+"`json:\"clock\" store:\"query\"`"+`
	Nick  string `+"`json:\"nick\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range pkg.Models {
		if m.Name != "Player" {
			continue
		}
		for _, f := range m.Fields {
			if !f.Base {
				got[f.GoName] = true
			}
		}
	}
	for _, absent := range []string{"Flags", "Blob", "Typed"} {
		if got[absent] {
			t.Errorf("anonymous %s must expand as an embed (no quick proofs for anonymous structs), got %v", absent, got)
		}
	}
	// Clock (defined over time.Time) is time-convertible — GORM's embed
	// rule exempts the Time data type, so it stays a column.
	if !got["Clock"] || !got["Nick"] {
		t.Errorf("Clock and Nick must be columns, got %v", got)
	}
	for _, wantWarn := range []string{"Player.Flags", "Player.Blob", "Player.Typed"} {
		var warned bool
		for _, w := range pkg.Warnings {
			warned = warned || strings.Contains(w, wantWarn)
		}
		if !warned {
			t.Errorf("dead tag on anonymous embed %s must warn, got %q", wantWarn, pkg.Warnings)
		}
	}
}

// TestScan_BytesAndJSONSerializerShapes is review round-3 finding 3:
// fixed byte arrays are bytes columns like byte slices, and
// `gorm:"json"` is the serializer shorthand GORM accepts.
func TestScan_BytesAndJSONSerializerShapes(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Token [16]byte

type DefinedByte byte

type Post struct {
	db.Model
	Token   Token          `+"`json:\"token\" store:\"query\"`"+`
	Direct  [8]byte        `+"`json:\"direct\" store:\"query\"`"+`
	Meta    map[string]any `+"`json:\"meta\" store:\"query\" gorm:\"json\"`"+`
	Odd     [4]DefinedByte `+"`json:\"odd\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range pkg.Models[0].Fields {
		if !f.Base {
			got[f.GoName] = true
		}
	}
	for _, want := range []string{"Token", "Direct", "Meta"} {
		if !got[want] {
			t.Errorf("%s must be a column, got %v", want, got)
		}
	}
	// GORM compares the element type identity: a defined byte type is
	// not the predeclared byte, so the array is not a bytes column.
	if got["Odd"] {
		t.Errorf("[4]DefinedByte must not be blessed as bytes, got %v", got)
	}
}

// TestScan_SelectorRules is review round-4 finding 1: Go resolves a
// member at the shallowest depth, uniquely. Same-depth ambiguity, a
// wrong-signature direct method, or a mere field named Value all mean
// the type does NOT implement driver.Valuer — GORM's type assertion
// agrees, so these are relations.
func TestScan_SelectorRules(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"database/sql/driver"

	"github.com/zynthara/chok/v2/db"
)

type MoneyA struct{ Cents int64 }

func (m MoneyA) Value() (driver.Value, error) { return m.Cents, nil }

type MoneyB struct{ Euros int64 }

func (m MoneyB) Value() (driver.Value, error) { return m.Euros, nil }

type Ambig struct {
	ID uint
	MoneyA
	MoneyB
}

type Shadow struct {
	ID uint
	MoneyA
}

func (Shadow) Value() (int, error) { return 0, nil }

type FieldShadow struct {
	ID uint
	MoneyA
	Value string
}

type Deep struct {
	ID uint
	Shadow
}

type Post struct {
	db.Model
	AmbigID       uint        `+"`json:\"ambig_id\" store:\"query\"`"+`
	Ambig         Ambig       `+"`json:\"ambig\" store:\"query\"`"+`
	ShadowID      uint        `+"`json:\"shadow_id\" store:\"query\"`"+`
	Shadow        Shadow      `+"`json:\"shadow\" store:\"query\"`"+`
	FieldShadowID uint        `+"`json:\"field_shadow_id\" store:\"query\"`"+`
	FieldShadow   FieldShadow `+"`json:\"field_shadow\" store:\"query\"`"+`
	DeepID        uint        `+"`json:\"deep_id\" store:\"query\"`"+`
	Deep          Deep        `+"`json:\"deep\" store:\"query\"`"+`
	Fine          MoneyA      `+"`json:\"fine\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range pkg.Models {
		if m.Name != "Post" {
			continue
		}
		for _, f := range m.Fields {
			if !f.Base {
				got[f.GoName] = true
			}
		}
	}
	if !got["Fine"] {
		t.Errorf("the unambiguous Valuer must stay a column, got %v", got)
	}
	// Deep: Shadow's wrong-signature Value at depth 1 shadows MoneyA's
	// exact one at depth 2 — still not a Valuer.
	for _, absent := range []string{"Ambig", "Shadow", "FieldShadow", "Deep"} {
		if got[absent] {
			t.Errorf("%s must be a relation under Go selector rules, got %v", absent, got)
		}
	}
}

// TestScan_AliasSignatures is review round-4 finding 2: interface
// signatures written through type ALIASES are still exact
// implementations; defined types over the same are not.
func TestScan_AliasSignatures(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"database/sql/driver"

	"github.com/zynthara/chok/v2/db"
)

type DV = driver.Value

type Text = string

type DefinedDV driver.Value

type AliasSig struct{ Cents int64 }

func (v AliasSig) Value() (DV, error) { return v.Cents, nil }

type AliasGDT struct{ V uint8 }

func (AliasGDT) GormDataType() Text { return "smallint" }

type NotReally struct {
	ID uint
}

func (NotReally) Value() (DefinedDV, error) { return nil, nil }

type Post struct {
	db.Model
	AliasSig    AliasSig  `+"`json:\"alias_sig\" store:\"query\"`"+`
	AliasGDT    AliasGDT  `+"`json:\"alias_gdt\" store:\"query\"`"+`
	NotReallyID uint      `+"`json:\"not_really_id\" store:\"query\"`"+`
	NotReally   NotReally `+"`json:\"not_really\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range pkg.Models {
		if m.Name != "Post" {
			continue
		}
		for _, f := range m.Fields {
			if !f.Base {
				got[f.GoName] = true
			}
		}
	}
	if !got["AliasSig"] || !got["AliasGDT"] {
		t.Errorf("alias-written signatures are exact implementations, got %v", got)
	}
	if got["NotReally"] {
		t.Errorf("a DEFINED type over driver.Value is a different result type — not a Valuer, got %v", got)
	}
}

// TestScan_AnonymousGormDataTypeLiterals is review round-4 finding 3:
// GORM's embed condition exempts GORMDataType values "time" and
// "bytes", so those anonymous embeds stay columns; other literals
// expand; a dynamic return is statically undecidable and fails loud
// when tagged.
func TestScan_AnonymousGormDataTypeLiterals(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type PseudoTime struct{ Unix int64 }

func (PseudoTime) GormDataType() string { return "time" }

type Level struct{ V uint8 }

func (Level) GormDataType() string { return "smallint" }

type Player struct {
	db.Model
	PseudoTime `+"`json:\"pseudo_time\" store:\"query\"`"+`
	Level      `+"`json:\"level\" store:\"query\"`"+`
	Nick       string `+"`json:\"nick\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range pkg.Models {
		if m.Name != "Player" {
			continue
		}
		for _, f := range m.Fields {
			if !f.Base {
				got[f.GoName] = true
			}
		}
	}
	if !got["PseudoTime"] {
		t.Errorf("GormDataType \"time\" is exempt from the embed rule — a column, got %v", got)
	}
	if got["Level"] {
		t.Errorf("other GormDataType literals still expand as embeds, got %v", got)
	}

	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Dyn struct{ V uint8 }

func (d Dyn) GormDataType() string {
	if d.V > 0 {
		return "time"
	}
	return "smallint"
}

type Player struct {
	db.Model
	Dyn  `+"`json:\"dyn\" store:\"query\"`"+`
	Nick string `+"`json:\"nick\" store:\"query\"`"+`
}
`)
	if _, err := Scan(dir); err == nil || !strings.Contains(err.Error(), "Dyn") {
		t.Fatalf("a dynamic GormDataType on a tagged embed is undecidable and must fail loud, got %v", err)
	}
}

// TestScan_DatatypesStorageWhitelist is review round-4 finding 4: only
// the storage types of gorm.io/datatypes are columns; the query
// expression types are neither Valuers nor columns.
func TestScan_DatatypesStorageWhitelist(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"gorm.io/datatypes"

	"github.com/zynthara/chok/v2/db"
)

type Post struct {
	db.Model
	Attrs datatypes.JSON    `+"`json:\"attrs\" store:\"query\"`"+`
	Tags  datatypes.JSONMap `+"`json:\"tags\" store:\"query\"`"+`
	Nick  string            `+"`json:\"nick\" store:\"query\"`"+`
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
	if !got["Attrs"] || !got["Tags"] {
		t.Errorf("datatypes storage types are columns, got %v", got)
	}

	// A named expression-type field is undecidable → loud.
	writeGo(t, dir, "m.go", `package m

import (
	"gorm.io/datatypes"

	"github.com/zynthara/chok/v2/db"
)

type Post struct {
	db.Model
	Expr datatypes.JSONQueryExpression `+"`json:\"expr\" store:\"query\"`"+`
}
`)
	if _, err := Scan(dir); err == nil || !strings.Contains(err.Error(), "Expr") {
		t.Fatalf("a datatypes expression type must not be blessed as a column, got %v", err)
	}

	// A tagged anonymous expression embed is equally undecidable.
	writeGo(t, dir, "m.go", `package m

import (
	"gorm.io/datatypes"

	"github.com/zynthara/chok/v2/db"
)

type Post struct {
	db.Model
	datatypes.JSONQueryExpression `+"`json:\"expr\" store:\"query\"`"+`
	Nick                          string `+"`json:\"nick\" store:\"query\"`"+`
}
`)
	if _, err := Scan(dir); err == nil {
		t.Fatal("a tagged anonymous expression embed must fail loud, not mint a dead reference")
	}
}

// TestScan_GenericInstantiation is review round-4 finding 5: local
// generic types classify by their instantiated shape, with type
// arguments substituted.
func TestScan_GenericInstantiation(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Bytes[T any] []T

type Wrap[T any] Bytes[T]

type Pair[K any, V any] map[K]V

type Post struct {
	db.Model
	Payload Bytes[byte]        `+"`json:\"payload\" store:\"query\"`"+`
	Chained Wrap[byte]         `+"`json:\"chained\" store:\"query\"`"+`
	Names   Bytes[string]      `+"`json:\"names\" store:\"query\"`"+`
	Lookup  Pair[string, int]  `+"`json:\"lookup\" store:\"query\" gorm:\"serializer:json\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range pkg.Models[0].Fields {
		if !f.Base {
			got[f.GoName] = true
		}
	}
	if !got["Payload"] || !got["Chained"] {
		t.Errorf("byte-instantiated generics are bytes columns, got %v", got)
	}
	if !got["Lookup"] {
		t.Errorf("serializer proof applies to generic types too, got %v", got)
	}
	if got["Names"] {
		t.Errorf("Bytes[string] is []string — not a column, got %v", got)
	}
	var warned bool
	for _, w := range pkg.Warnings {
		warned = warned || strings.Contains(w, "Post.Names")
	}
	if !warned {
		t.Fatalf("the non-byte instantiation must warn like any relation shape, got %q", pkg.Warnings)
	}
}

// TestScan_UintptrFailsLoud is review round-4 finding 6: the pinned
// GORM has no mapping for uintptr — schema parsing fails at runtime, so
// blessing it as a scalar would be a lie.
func TestScan_UintptrFailsLoud(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Post struct {
	db.Model
	Ptr uintptr `+"`json:\"ptr\" store:\"query\"`"+`
}
`)
	if _, err := Scan(dir); err == nil || !strings.Contains(err.Error(), "Ptr") {
		t.Fatalf("uintptr is unsupported by GORM and must fail loud, got %v", err)
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

// TestScan_Round5DiamondPathMultiplicity is review round-5 finding 1:
// the same Valuer reached through TWO embedding paths at one depth is
// ambiguous under Go's selector rules — the runtime type assertion
// fails, so the field is a relation. The BFS must count same-depth path
// multiplicity instead of deduplicating the shared type; a single-path
// chain of the same depth stays promoted, and two paths to a KNOWN
// cross-package Valuer are ambiguous the same way.
func TestScan_Round5DiamondPathMultiplicity(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"database/sql"
	"database/sql/driver"

	"github.com/zynthara/chok/v2/db"
)

type Money struct {
	ID    uint
	Cents int64
}

func (m Money) Value() (driver.Value, error) { return m.Cents, nil }

type Left struct{ Money }

type Right struct{ Money }

type Diamond struct {
	ID uint
	Left
	Right
}

type NullLeft struct{ sql.NullString }

type NullRight struct{ sql.NullString }

type NullDiamond struct {
	ID uint
	NullLeft
	NullRight
}

type Deep struct{ Left }

type Post struct {
	db.Model
	DiamondID     uint        `+"`json:\"diamond_id\" store:\"query\"`"+`
	Diamond       Diamond     `+"`json:\"diamond\" store:\"query\"`"+`
	NullDiamondID uint        `+"`json:\"null_diamond_id\" store:\"query\"`"+`
	NullDiamond   NullDiamond `+"`json:\"null_diamond\" store:\"query\"`"+`
	Deep          Deep        `+"`json:\"deep\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range pkg.Models {
		if m.Name != "Post" {
			continue
		}
		for _, f := range m.Fields {
			if !f.Base {
				got[f.GoName] = true
			}
		}
	}
	for _, absent := range []string{"Diamond", "NullDiamond"} {
		if got[absent] {
			t.Errorf("%s must be a relation — two same-depth paths to Value are ambiguous, got %v", absent, got)
		}
	}
	// One path at depth three: Deep→Left→Money.Value promotes fine.
	if !got["Deep"] || !got["DiamondID"] || !got["NullDiamondID"] {
		t.Errorf("single-path promotion and FK columns must survive, got %v", got)
	}
	for _, wantWarn := range []string{"Post.Diamond:", "Post.NullDiamond:"} {
		var warned bool
		for _, w := range pkg.Warnings {
			warned = warned || strings.Contains(w, wantWarn)
		}
		if !warned {
			t.Errorf("the ambiguous diamond %s must warn as a skipped relation, got %q", wantWarn, pkg.Warnings)
		}
	}
}

// TestScan_Round5AliasSignatureConstructors is review round-5 finding
// 2: alias resolution in signatures must keep the type constructor —
// an alias to *driver.Value (or *string) denotes the POINTER type, so
// a method returning it does not implement the interface and the type
// stays a relation. The plain alias chain keeps matching.
func TestScan_Round5AliasSignatureConstructors(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"database/sql/driver"

	"github.com/zynthara/chok/v2/db"
)

type PV = *driver.Value

type Parcel struct{ ID uint }

func (Parcel) Value() (PV, error) { return nil, nil }

type PS = *string

type Gauge struct{ ID uint }

func (Gauge) GormDataType() PS { return nil }

type DV = driver.Value

type DV2 = DV

type Sealed struct{ S string }

func (s Sealed) Value() (DV2, error) { return s.S, nil }

type Post struct {
	db.Model
	ParcelID uint   `+"`json:\"parcel_id\" store:\"query\"`"+`
	Parcel   Parcel `+"`json:\"parcel\" store:\"query\"`"+`
	GaugeID  uint   `+"`json:\"gauge_id\" store:\"query\"`"+`
	Gauge    Gauge  `+"`json:\"gauge\" store:\"query\"`"+`
	Locker   Sealed `+"`json:\"locker\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range pkg.Models {
		if m.Name != "Post" {
			continue
		}
		for _, f := range m.Fields {
			if !f.Base {
				got[f.GoName] = true
			}
		}
	}
	for _, absent := range []string{"Parcel", "Gauge"} {
		if got[absent] {
			t.Errorf("%s must be a relation — the aliased pointer result is not the interface signature, got %v", absent, got)
		}
	}
	if !got["Locker"] {
		t.Errorf("the constructor-free alias chain must still prove driver.Valuer, got %v", got)
	}
	for _, wantWarn := range []string{"Post.Parcel:", "Post.Gauge:"} {
		var warned bool
		for _, w := range pkg.Warnings {
			warned = warned || strings.Contains(w, wantWarn)
		}
		if !warned {
			t.Errorf("the false implementer %s must warn as a skipped relation, got %q", wantWarn, pkg.Warnings)
		}
	}
}

// TestScan_Round5AliasToSelectorTerminals is review round-5 finding 3:
// a local alias denoting a cross-package type embeds that type itself.
// A known Valuer target promotes Value (the embedder IS a column), a
// time.Time target stays invisible to method resolution without taint,
// and an UNKNOWN target taints — silently calling the embedder a
// relation would be a guess.
func TestScan_Round5AliasToSelectorTerminals(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"database/sql"
	"time"

	"github.com/zynthara/chok/v2/db"
)

type Null = sql.NullString

type Box struct{ Null }

type When = time.Time

type Slot struct{ When }

type Post struct {
	db.Model
	Null   `+"`json:\"tag\" store:\"query\"`"+`
	Box    Box  `+"`json:\"box\" store:\"query\"`"+`
	SlotID uint `+"`json:\"slot_id\" store:\"query\"`"+`
	Slot   Slot `+"`json:\"slot\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range pkg.Models {
		if m.Name != "Post" {
			continue
		}
		for _, f := range m.Fields {
			if !f.Base {
				got[f.GoName] = true
			}
		}
	}
	// Box promotes NullString.Value through the alias embed; the model's
	// own anonymous alias embed is a column under the alias name.
	if !got["Box"] || !got["Null"] {
		t.Errorf("alias-to-known-Valuer embeds must prove columns, got %v", got)
	}
	if got["Slot"] {
		t.Errorf("Slot promotes nothing from time.Time — a plain struct relation, got %v", got)
	}
	var warned bool
	for _, w := range pkg.Warnings {
		warned = warned || strings.Contains(w, "Post.Slot:")
	}
	if !warned {
		t.Fatalf("Slot must warn as a skipped relation (not fail loud — time.Time cannot shadow), got %q", pkg.Warnings)
	}
}

// TestScan_Round5AliasToUnknownSelectorFailsLoud is the taint half of
// round-5 finding 3: an alias to an unclassifiable cross-package type
// could promote anything, so a tagged field over the embedder must
// fail loud exactly like the selector embedded directly (round-4).
func TestScan_Round5AliasToUnknownSelectorFailsLoud(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"example.com/other"

	"github.com/zynthara/chok/v2/db"
)

type Ext = other.Thing

type Crate struct{ Ext }

type Post struct {
	db.Model
	Crate Crate `+"`json:\"crate\" store:\"query\"`"+`
}
`)
	_, err := Scan(dir)
	if err == nil || !strings.Contains(err.Error(), "cannot statically decide") {
		t.Fatalf("the opaque alias embed must fail loud, got %v", err)
	}
}

// TestScan_Round5ByteIdentity is review round-5 finding 4: GORM matches
// the element type against uint8 by REFLECT IDENTITY, which local alias
// chains and generic substitution preserve — []Byte, [4]Chain and
// Bytes[Byte] are bytes columns. Defined byte types and pointer
// aliases stay rejected.
func TestScan_Round5ByteIdentity(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Byte = byte

type Chain = Byte

type Token []Byte

type Bytes[T any] []T

type Defined byte

type PB = *byte

type Blob struct {
	db.Model
	Token  Token          `+"`json:\"token\" store:\"query\"`"+`
	Raw    []Byte         `+"`json:\"raw\" store:\"query\"`"+`
	Arr    [4]Chain       `+"`json:\"arr\" store:\"query\"`"+`
	Packed Bytes[Byte]    `+"`json:\"packed\" store:\"query\"`"+`
	Bad    []Defined      `+"`json:\"bad\" store:\"query\"`"+`
	Worse  Bytes[Defined] `+"`json:\"worse\" store:\"query\"`"+`
	Ptr    []PB           `+"`json:\"ptr\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range pkg.Models {
		if m.Name != "Blob" {
			continue
		}
		for _, f := range m.Fields {
			if !f.Base {
				got[f.GoName] = true
			}
		}
	}
	for _, want := range []string{"Token", "Raw", "Arr", "Packed"} {
		if !got[want] {
			t.Errorf("%s rides alias chains to the predeclared byte — a bytes column, got %v", want, got)
		}
	}
	for _, absent := range []string{"Bad", "Worse", "Ptr"} {
		if got[absent] {
			t.Errorf("%s must stay rejected — its element is not identical to uint8, got %v", absent, got)
		}
	}
	for _, wantWarn := range []string{"Blob.Bad:", "Blob.Worse:", "Blob.Ptr:"} {
		var warned bool
		for _, w := range pkg.Warnings {
			warned = warned || strings.Contains(w, wantWarn)
		}
		if !warned {
			t.Errorf("the rejected element shape %s must warn, got %q", wantWarn, pkg.Warnings)
		}
	}
}

// TestScan_Round5AnonymousGenericInstantiation is review round-5
// finding 5: an anonymously embedded generic INSTANTIATION keeps its
// arguments for classification — Bytes[byte] is a bytes column under
// the type's base name, not a bare generic error. A generic STRUCT
// instantiation still expands as an embed like any struct.
func TestScan_Round5AnonymousGenericInstantiation(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Byte = byte

type Bytes[T any] []T

type Pair[T any] struct{ A, B T }

type Doc struct {
	db.Model
	Bytes[byte] `+"`json:\"payload\" store:\"query\"`"+`
	Pair[int]   `+"`json:\"pair\" store:\"query\"`"+`
	Name string `+"`json:\"name\" store:\"query\"`"+`
}

type Doc2 struct {
	db.Model
	Bytes[Byte] `+"`json:\"chunk\" store:\"query\"`"+`
	Note string `+"`json:\"note\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]map[string]string{}
	for _, m := range pkg.Models {
		fields[m.Name] = map[string]string{}
		for _, f := range m.Fields {
			if !f.Base {
				fields[m.Name][f.GoName] = f.Value
			}
		}
	}
	if fields["Doc"]["Bytes"] != "payload" {
		t.Errorf("the embedded instantiation must be a column under the base name, got %v", fields["Doc"])
	}
	if fields["Doc2"]["Bytes"] != "chunk" {
		t.Errorf("an aliased byte argument classifies the same way, got %v", fields["Doc2"])
	}
	if _, ok := fields["Doc"]["Pair"]; ok {
		t.Errorf("a generic struct instantiation still expands as an embed, got %v", fields["Doc"])
	}
	var warned bool
	for _, w := range pkg.Warnings {
		warned = warned || strings.Contains(w, "Doc.Pair:")
	}
	if !warned {
		t.Fatalf("the dead tag on the expanding struct embed must warn, got %q", pkg.Warnings)
	}
}
