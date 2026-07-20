package fieldgen

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"github.com/zynthara/chok/v2/db"
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
	for _, absent := range []string{"Profile", "Twin", "Children", "Badge"} {
		if got[absent] {
			t.Errorf("%s is not a column at runtime and must be skipped, got %v", absent, got)
		}
	}
	for _, wantWarn := range []string{"Post.Profile", "Post.Twin", "Post.Children", "Post.Badge"} {
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

type Post struct {
	db.Model
	Token   Token          `+"`json:\"token\" store:\"query\"`"+`
	Direct  [8]byte        `+"`json:\"direct\" store:\"query\"`"+`
	Meta    map[string]any `+"`json:\"meta\" store:\"query\" gorm:\"json\"`"+`
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
	// not the predeclared byte, so the array is no bytes column — and a
	// non-column container aborts the model outright (review round-7).
	odd := t.TempDir()
	writeGo(t, odd, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type DefinedByte byte

type Post struct {
	db.Model
	Odd [4]DefinedByte `+"`json:\"odd\" store:\"query\"`"+`
}
`)
	if _, err := Scan(odd); err == nil || !strings.Contains(err.Error(), "Post.Odd") {
		t.Fatalf("[4]DefinedByte has no schema shape and must fail loud, got %v", err)
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

	// Bytes[string] IS []string: no schema shape, so on a model the
	// substituted instantiation fails loud like the written-out slice
	// (review round-7).
	names := t.TempDir()
	writeGo(t, names, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Bytes[T any] []T

type Post struct {
	db.Model
	Names Bytes[string] `+"`json:\"names\" store:\"query\"`"+`
}
`)
	if _, err := Scan(names); err == nil || !strings.Contains(err.Error(), "Post.Names") {
		t.Fatalf("the non-byte instantiation is a scalar container and must fail loud, got %v", err)
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
	if strings.Join(names, ",") != "Kept,Wrapped" {
		t.Fatalf("gorm:\"-\"/-:all are erased; -:migration is kept; embedded on a SCALAR is a no-op and the field stays a column (review round-7, latched), kept = %v", names)
	}
	for _, w := range pkg.Warnings {
		if strings.Contains(w, "Wrapped") {
			t.Fatalf("embedded on a scalar kind is inert — the store tag works and must not warn, got %q", pkg.Warnings)
		}
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
// aliases stay rejected — and since round-7 a rejected scalar container
// is a hard error on a model (the runtime aborts on it), not a warning.
func TestScan_Round5ByteIdentity(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Byte = byte

type Chain = Byte

type Token []Byte

type Bytes[T any] []T

type Blob struct {
	db.Model
	Token  Token       `+"`json:\"token\" store:\"query\"`"+`
	Raw    []Byte      `+"`json:\"raw\" store:\"query\"`"+`
	Arr    [4]Chain    `+"`json:\"arr\" store:\"query\"`"+`
	Packed Bytes[Byte] `+"`json:\"packed\" store:\"query\"`"+`
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

	for field, src := range map[string]string{
		"Bad":   "type Defined byte\n\ntype Blob struct {\n\tdb.Model\n\tBad []Defined `store:\"query\"`\n}",
		"Worse": "type Defined byte\n\ntype Bytes[T any] []T\n\ntype Blob struct {\n\tdb.Model\n\tWorse Bytes[Defined] `store:\"query\"`\n}",
		"Ptr":   "type PB = *byte\n\ntype Blob struct {\n\tdb.Model\n\tPtr []PB `store:\"query\"`\n}",
	} {
		bad := t.TempDir()
		writeGo(t, bad, "m.go", "package m\n\nimport \"github.com/zynthara/chok/v2/db\"\n\n"+src+"\n")
		if _, err := Scan(bad); err == nil || !strings.Contains(err.Error(), "Blob."+field) {
			t.Errorf("%s: the element is not identical to uint8 — a scalar container, which aborts the model and must fail loud, got %v", field, err)
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

// TestScan_Round6ParenTypes is review round-6 finding 1: grouping
// parentheses are legal, gofmt-preserved and PURE SYNTAX — `(string)`
// is string, `type Byte = (byte)` keeps the predeclared identity, and
// `type PV = (driver.Value)` denotes the interface itself. Every
// classification path must see straight through them.
func TestScan_Round6ParenTypes(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"database/sql/driver"

	"github.com/zynthara/chok/v2/db"
)

type PV = (driver.Value)

type Sealed struct{ S string }

func (s Sealed) Value() (PV, error) { return s.S, nil }

type Byte = (byte)

type Token []Byte

type Direct [](byte)

type Post struct {
	db.Model
	Scalar (string)  `+"`json:\"scalar\" store:\"query\"`"+`
	Direct Direct    `+"`json:\"direct\" store:\"query\"`"+`
	Token  Token     `+"`json:\"token\" store:\"query\"`"+`
	Sealed Sealed    `+"`json:\"sealed\" store:\"query\"`"+`
	Ptr    (*string) `+"`json:\"ptr\" store:\"query,update\"`"+`
}
`)
	pkg, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Warnings) != 0 {
		t.Fatalf("parenthesized column types must classify clean, got %q", pkg.Warnings)
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
	for _, want := range []string{"Scalar", "Direct", "Token", "Sealed", "Ptr"} {
		if !got[want] {
			t.Errorf("%s must be a column — parens never change what a type denotes, got %v", want, got)
		}
	}
}

// TestScan_Round6GenericAliasInstantiation is review round-6 finding 2:
// a generic ALIAS instantiation (`type ByteOf[T any] = byte`) denotes
// its right-hand side with the arguments substituted — full identity,
// exactly like a plain alias — so byte-element resolution and signature
// resolution must expand it. A DEFINED generic type's instantiation
// stays its own identity, and a package-scope alias target must NOT see
// the instantiation environment (Sly below: its []Alias lands on the
// package-level defined type T, not on the argument bound to the
// parameter of the same name).
func TestScan_Round6GenericAliasInstantiation(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "m.go", `package m

import (
	"database/sql/driver"

	"github.com/zynthara/chok/v2/db"
)

type ByteOf[T any] = byte

type Token []ByteOf[int]

type ValueOf[T any] = driver.Value

type Sealed struct{ S string }

func (s Sealed) Value() (ValueOf[int], error) { return s.S, nil }

type Pick[T any] = T

type Boxed struct{ S string }

func (b Boxed) Value() (Pick[driver.Value], error) { return b.S, nil }

type Post struct {
	db.Model
	Token  Token             `+"`json:\"token\" store:\"query\"`"+`
	Chunk  [4]ByteOf[string] `+"`json:\"chunk\" store:\"query\"`"+`
	Sealed Sealed            `+"`json:\"sealed\" store:\"query\"`"+`
	Boxed  Boxed             `+"`json:\"boxed\" store:\"query\"`"+`
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
	for _, want := range []string{"Token", "Chunk", "Sealed", "Boxed"} {
		if !got[want] {
			t.Errorf("%s rides a generic alias instantiation to the denoted type — a column, got %v", want, got)
		}
	}

	// The rejected identities are scalar containers, which abort the
	// model at runtime — hard errors since round-7. Bad instantiates a
	// DEFINED generic byte type (its own identity, not byte); Sly's
	// []Alias lands on the package-level defined type T, NOT on the
	// argument bound to the parameter of the same name.
	for field, src := range map[string]string{
		"Bad": "type Defined[T any] byte\n\ntype Bad []Defined[int]\n\ntype Post struct {\n\tdb.Model\n\tBad Bad `store:\"query\"`\n}",
		"Sly": "type T int8\n\ntype Alias = T\n\ntype Sly[T any] []Alias\n\ntype Post struct {\n\tdb.Model\n\tSly Sly[byte] `store:\"query\"`\n}",
	} {
		bad := t.TempDir()
		writeGo(t, bad, "m.go", "package m\n\nimport \"github.com/zynthara/chok/v2/db\"\n\n"+src+"\n")
		if _, err := Scan(bad); err == nil || !strings.Contains(err.Error(), "Post."+field) {
			t.Errorf("%s: not the argument's identity — a scalar container that aborts the model, must fail loud, got %v", field, err)
		}
	}
}

// Round6Names/round6Host feed the runtime latch in
// TestScan_Round6AnonymousContainerFailsLoud: GORM must actually ABORT
// on an anonymous non-struct shape — the behavior the scan's colHardNo
// verdict mirrors. round6Serialized pins round-7 finding 1 on top: a
// serializer tag does NOT rescue the anonymous embed (the EMBEDDED
// branch fires on the kind regardless of the DataType it sets).
type Round6Names []string

type round6Host struct{ Round6Names }

type round6Serialized struct {
	Round6Names `gorm:"serializer:json"`
}

// TestScan_Round6AnonymousContainerFailsLoud is review round-6 finding
// 3: an anonymous local slice/array/map embed is NOT a skipped relation
// — GORM aborts schema parsing on it (unsupported data type / invalid
// embedded struct). On a runtime model (a store tag, or a chok base
// embed) the scan must fail the same way instead of warning that the
// runtime skips it; a plain DTO never meets a schema parse and stays
// silent. A serializer tag does NOT rescue the embed — round-7 finding
// 1 corrected round-6's expectation here, see the latch below.
func TestScan_Round6AnonymousContainerFailsLoud(t *testing.T) {
	quiet := logger.Default
	logger.Default = logger.Discard // the latch's expected parse failure logs through the global logger
	t.Cleanup(func() { logger.Default = quiet })
	if _, err := schema.Parse(&round6Host{}, &sync.Map{}, schema.NamingStrategy{}); err == nil || !strings.Contains(err.Error(), "unsupported data type") {
		t.Fatalf("latch: GORM must abort on the anonymous slice embed with the error the scan mirrors, got %v", err)
	}
	if _, err := schema.Parse(&round6Serialized{}, &sync.Map{}, schema.NamingStrategy{}); err == nil || !strings.Contains(err.Error(), "invalid embedded struct") {
		t.Fatalf("latch: the serializer tag must not rescue the anonymous embed (round-7), got %v", err)
	}

	tagged := t.TempDir()
	writeGo(t, tagged, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Names []string

type Doc struct {
	db.Model
	Names `+"`json:\"names\" store:\"query\"`"+`
}
`)
	_, err := Scan(tagged)
	if err == nil || !strings.Contains(err.Error(), "Doc.Names") || !strings.Contains(err.Error(), "unsupported data type") {
		t.Fatalf("the tagged anonymous slice embed must fail loud with the runtime's diagnosis, got %v", err)
	}

	untagged := t.TempDir()
	writeGo(t, untagged, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Tags map[string]string

type Post struct {
	db.Model
	Tags
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	_, err = Scan(untagged)
	if err == nil || !strings.Contains(err.Error(), "Post.Tags") {
		t.Fatalf("the untagged fatal embed on a runtime model must still fail the scan, got %v", err)
	}

	mixed := t.TempDir()
	writeGo(t, mixed, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Names []string

type Meta struct{ A string }

type DTO struct {
	Names
	Other string
}

type Post2 struct {
	db.Model
	Meta
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(mixed)
	if err != nil {
		t.Fatalf("a DTO's fatal embed never meets a schema parse — the scan must pass, got %v", err)
	}
	if len(pkg.Warnings) != 0 {
		t.Fatalf("nothing here warrants a warning, got %q", pkg.Warnings)
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
	if fields["Post2"]["Name"] != "name" {
		t.Errorf("the struct embed keeps expanding as before, got %v", fields["Post2"])
	}
	if _, ok := fields["DTO"]; ok {
		t.Errorf("the DTO must not become a model, got %v", fields["DTO"])
	}

	// Round-7 finding 1: the serializer tag does NOT rescue an anonymous
	// embed — GORM's embedded branch fires on the KIND (see the
	// round6Serialized latch above), so the scan fails loud too.
	serialized := t.TempDir()
	writeGo(t, serialized, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Names []string

type Post3 struct {
	db.Model
	Names `+"`json:\"names\" store:\"query\" gorm:\"serializer:json\"`"+`
}
`)
	if _, err := Scan(serialized); err == nil || !strings.Contains(err.Error(), "Post3.Names") {
		t.Fatalf("the serializer-tagged anonymous container must fail loud (round-7), got %v", err)
	}
}

// round-7 runtime-latch types: each pins one verified GORM v1.31.2
// behavior the round-7 scan changes mirror.
type Round7JList []string

// GormDataType implements the GORM data-type hook with a non-exempt
// literal — the anonymous embed branch still fires on the slice kind.
func (Round7JList) GormDataType() string { return "json" }

type round7JSONHost struct{ Round7JList }

// Round7Code is a scalar type whose GormDataType returns the EMPTY
// string: GORM overwrites the string DataType with it, the relation
// gate picks the field up and getOrParse aborts.
type Round7Code string

// GormDataType implements the GORM data-type hook, destructively.
func (Round7Code) GormDataType() string { return "" }

type round7GDTHost struct{ Code Round7Code }

type round7NamedHost struct{ Tags []string }

type round7EmbeddedHost struct {
	N Round6Names `gorm:"embedded;->:false;<-:false"`
}

type round7ClosedHost struct {
	Round6Names `gorm:"->:false"`
}

// TestScan_Round7AnonymousProofsNeverRescue is review round-7 finding
// 1: serializer / `gorm:"type:..."` / json tag proofs apply to NAMED
// fields only — an anonymous field enters GORM's embedded branch on its
// KIND, whatever DataType those tags set, and a non-exempt GormDataType
// literal ("json") does not help either. Only real Valuers and the
// literal "time"/"bytes" snapshots stay columns; scalar kinds fall
// through the branch and stay columns too.
func TestScan_Round7AnonymousProofsNeverRescue(t *testing.T) {
	quiet := logger.Default
	logger.Default = logger.Discard
	t.Cleanup(func() { logger.Default = quiet })
	if _, err := schema.Parse(&round7JSONHost{}, &sync.Map{}, schema.NamingStrategy{}); err == nil || !strings.Contains(err.Error(), "invalid embedded struct") {
		t.Fatalf("latch: a non-exempt GormDataType must not rescue the anonymous slice, got %v", err)
	}

	typed := t.TempDir()
	writeGo(t, typed, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Names []string

type Post struct {
	db.Model
	Names `+"`json:\"names\" store:\"query\" gorm:\"type:json\"`"+`
}
`)
	if _, err := Scan(typed); err == nil || !strings.Contains(err.Error(), "Post.Names") {
		t.Fatalf("gorm:\"type:...\" must not rescue an anonymous container, got %v", err)
	}

	gdt := t.TempDir()
	writeGo(t, gdt, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type JList []string

func (JList) GormDataType() string { return "json" }

type JLevel int

func (JLevel) GormDataType() string { return "json" }

type Post struct {
	db.Model
	JLevel `+"`json:\"j_level\" store:\"query\"`"+`
	Name   string `+"`json:\"name\" store:\"query\"`"+`
}

type Broken struct {
	db.Model
	JList `+"`json:\"j_list\" store:\"query\"`"+`
}
`)
	if _, err := Scan(gdt); err == nil || !strings.Contains(err.Error(), "Broken.JList") {
		t.Fatalf("a non-exempt GormDataType literal on an anonymous slice must fail loud, got %v", err)
	}

	scalar := t.TempDir()
	writeGo(t, scalar, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type JLevel int

func (JLevel) GormDataType() string { return "json" }

type Post struct {
	db.Model
	JLevel `+"`json:\"j_level\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(scalar)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Models) != 1 || len(pkg.Warnings) != 0 {
		t.Fatalf("an anonymous SCALAR with a GormDataType literal falls through the embed switch and stays a column, got models=%d warnings=%q", len(pkg.Models), pkg.Warnings)
	}
	var jlevel bool
	for _, f := range pkg.Models[0].Fields {
		jlevel = jlevel || f.GoName == "JLevel"
	}
	if !jlevel {
		t.Fatalf("JLevel must generate as a column, got %+v", pkg.Models[0].Fields)
	}
}

// TestScan_Round7EmbeddedTagKinds is review round-7 finding 2: the
// `gorm:"embedded"` tag forces GORM's embedded branch unconditionally —
// no permission gate, no bytes exemption. Struct targets promote,
// scalar targets make the tag a NO-OP (the field stays a plain column
// and its store tag works — latched), and every other kind aborts
// schema parsing, named or anonymous.
func TestScan_Round7EmbeddedTagKinds(t *testing.T) {
	quiet := logger.Default
	logger.Default = logger.Discard
	t.Cleanup(func() { logger.Default = quiet })
	if _, err := schema.Parse(&round7EmbeddedHost{}, &sync.Map{}, schema.NamingStrategy{}); err == nil || !strings.Contains(err.Error(), "invalid embedded struct") {
		t.Fatalf("latch: closed permissions must not rescue an EMBEDDED-tagged container, got %v", err)
	}

	for field, src := range map[string]string{
		"N2": "type Names []string\n\ntype Post struct {\n\tdb.Model\n\tN2 Names `store:\"query\" gorm:\"embedded\"`\n}",
		"N3": "type Names []string\n\ntype Post struct {\n\tdb.Model\n\tN3 Names `store:\"query\" gorm:\"embedded;->:false;<-:false\"`\n}",
		"U8": "type Bin []byte\n\ntype Post struct {\n\tdb.Model\n\tU8 Bin `store:\"query\" gorm:\"embedded\"`\n}",
	} {
		dir := t.TempDir()
		writeGo(t, dir, "m.go", "package m\n\nimport \"github.com/zynthara/chok/v2/db\"\n\n"+src+"\n")
		if _, err := Scan(dir); err == nil || !strings.Contains(err.Error(), "Post."+field) || !strings.Contains(err.Error(), "not a struct") {
			t.Errorf("%s: embedded on a non-struct kind must fail loud (even byte slices, even with closed perms), got %v", field, err)
		}
	}

	anon := t.TempDir()
	writeGo(t, anon, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Names []string

type Post struct {
	db.Model
	Names `+"`gorm:\"embedded\"`"+`
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	if _, err := Scan(anon); err == nil || !strings.Contains(err.Error(), "Post.Names") {
		t.Fatalf("an untagged EMBEDDED container on a runtime model must still fail the scan, got %v", err)
	}

	scalar := t.TempDir()
	writeGo(t, scalar, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Level int

type Post struct {
	db.Model
	L2 Level `+"`json:\"l2\" store:\"query\" gorm:\"embedded\"`"+`
}
`)
	pkg, err := Scan(scalar)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Warnings) != 0 {
		t.Fatalf("embedded on a scalar is a no-op — the store tag works, no warning, got %q", pkg.Warnings)
	}
	var l2 bool
	for _, f := range pkg.Models[0].Fields {
		l2 = l2 || f.GoName == "L2"
	}
	if !l2 {
		t.Fatalf("L2 must generate — the runtime keeps it a plain column (latched), got %+v", pkg.Models[0].Fields)
	}
}

// TestScan_Round7GormDataTypeStates is review round-7 finding 3: the
// GormDataType RETURN VALUE is the DataType, unguarded. A provably
// non-empty literal proves a column (even on a slice type — latched); a
// provably EMPTY literal erases the DataType, which aborts the model on
// every non-struct shape and downgrades a struct to a plain relation;
// a dynamic body could be either and must stay unknowable.
func TestScan_Round7GormDataTypeStates(t *testing.T) {
	quiet := logger.Default
	logger.Default = logger.Discard
	t.Cleanup(func() { logger.Default = quiet })
	if _, err := schema.Parse(&round7GDTHost{}, &sync.Map{}, schema.NamingStrategy{}); err == nil || !strings.Contains(err.Error(), "unsupported data type") {
		t.Fatalf("latch: an empty GormDataType must abort the model, got %v", err)
	}

	tagged := t.TempDir()
	writeGo(t, tagged, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Code string

func (Code) GormDataType() string { return "" }

type Post struct {
	db.Model
	Code Code `+"`json:\"code\" store:\"query\"`"+`
}
`)
	if _, err := Scan(tagged); err == nil || !strings.Contains(err.Error(), "Post.Code") || !strings.Contains(err.Error(), "empty string") {
		t.Fatalf("a tagged empty-literal GormDataType scalar must fail loud, got %v", err)
	}

	untagged := t.TempDir()
	writeGo(t, untagged, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Code string

func (Code) GormDataType() string { return "" }

type Post struct {
	db.Model
	Code Code
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	if _, err := Scan(untagged); err == nil || !strings.Contains(err.Error(), "Post.Code") {
		t.Fatalf("an untagged empty-literal GormDataType field still aborts the model, got %v", err)
	}

	dynamic := t.TempDir()
	writeGo(t, dynamic, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Code string

func (c Code) GormDataType() string {
	if c == "" {
		return ""
	}
	return "text"
}

type Post struct {
	db.Model
	Code Code `+"`json:\"code\" store:\"query\"`"+`
}
`)
	if _, err := Scan(dynamic); err == nil || !strings.Contains(err.Error(), "cannot statically decide") {
		t.Fatalf("a dynamic GormDataType could be empty (fatal) or not (column) — must stay unknowable, got %v", err)
	}

	column := t.TempDir()
	writeGo(t, column, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type JList []string

func (JList) GormDataType() string { return "json" }

type SCode struct{ S string }

func (SCode) GormDataType() string { return "" }

type Post struct {
	db.Model
	J  JList `+"`json:\"j\" store:\"query\"`"+`
	SC SCode `+"`json:\"sc\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(column)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range pkg.Models[0].Fields {
		got[f.GoName] = true
	}
	if !got["J"] {
		t.Errorf("a NAMED field with a non-empty GormDataType literal is a column even on a slice type (latched), got %v", got)
	}
	if got["SC"] {
		t.Errorf("an empty GormDataType on a STRUCT downgrades it to a relation — no column, got %v", got)
	}
	var warned bool
	for _, w := range pkg.Warnings {
		warned = warned || strings.Contains(w, "Post.SC")
	}
	if !warned {
		t.Fatalf("the struct relation must warn as a skipped tag, got %q", pkg.Warnings)
	}
}

// TestScan_Round7NamedFatalShapes is the round-7 same-class sweep of
// finding 4: EVERY DataType-less field whose terminal shape is not a
// struct — named or anonymous, tagged or not — feeds GORM's relation
// gate into getOrParse, which aborts the model (latched). uintptr and
// complex kinds (finding 4's exact case) are fatal the same way. A
// struct without a chok base never meets a direct schema parse through
// the store contract, so an embeddable's named containers stay quiet.
func TestScan_Round7NamedFatalShapes(t *testing.T) {
	quiet := logger.Default
	logger.Default = logger.Discard
	t.Cleanup(func() { logger.Default = quiet })
	if _, err := schema.Parse(&round7NamedHost{}, &sync.Map{}, schema.NamingStrategy{}); err == nil || !strings.Contains(err.Error(), "unsupported data type") {
		t.Fatalf("latch: a NAMED scalar container must abort the model at runtime, got %v", err)
	}

	for field, src := range map[string]string{
		"Tags": "type Post struct {\n\tdb.Model\n\tTags []string\n\tName string `store:\"query\"`\n}",
		"M":    "type Tags map[string]string\n\ntype Post struct {\n\tdb.Model\n\tM Tags\n\tName string `store:\"query\"`\n}",
		"AnyF": "type Post struct {\n\tdb.Model\n\tAnyF any\n\tName string `store:\"query\"`\n}",
		"X":    "type Post struct {\n\tdb.Model\n\tX complex64 `store:\"query\"`\n}",
		"Word": "type Word uintptr\n\ntype Post struct {\n\tdb.Model\n\tWord\n\tName string `store:\"query\"`\n}",
	} {
		dir := t.TempDir()
		writeGo(t, dir, "m.go", "package m\n\nimport \"github.com/zynthara/chok/v2/db\"\n\n"+src+"\n")
		if _, err := Scan(dir); err == nil || !strings.Contains(err.Error(), "Post."+field) {
			t.Errorf("%s: a shape GORM cannot schema-parse must fail the scan on a runtime model, got %v", field, err)
		}
	}

	embeddable := t.TempDir()
	writeGo(t, embeddable, "m.go", `package m

type Meta struct {
	Tags  []string
	Actor string `+"`json:\"actor\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(embeddable)
	if err != nil {
		t.Fatalf("a base-less tagged struct may live inside embeds, where the relation gate never runs — must scan clean, got %v", err)
	}
	if len(pkg.Models) != 1 || pkg.Models[0].Name != "Meta" {
		t.Fatalf("Meta still generates its declared surface, got %+v", pkg.Models)
	}
}

// TestScan_Round7ClosedPermissions is review round-7 finding 5: the
// `->` / `<-` permission algebra can close every permission — then the
// field never enters the anonymous-embed or relation branches and the
// model parses fine (latched), so the scan must not reject it. Note
// `->:false` ALONE clears create and update too. A closed struct embed
// also stops expanding, so nothing promotes; and a field that stays
// READABLE keeps its fatal shape fatal.
func TestScan_Round7ClosedPermissions(t *testing.T) {
	if _, err := schema.Parse(&round7ClosedHost{}, &sync.Map{}, schema.NamingStrategy{}); err != nil {
		t.Fatalf("latch: a fully closed anonymous container must parse cleanly, got %v", err)
	}

	for label, src := range map[string]string{
		"anon-closed-pair":  "type Names []string\n\ntype Post struct {\n\tdb.Model\n\tNames `gorm:\"->:false;<-:false\"`\n\tName string `store:\"query\"`\n}",
		"anon-closed-read":  "type Names []string\n\ntype Post struct {\n\tdb.Model\n\tNames `gorm:\"->:false\"`\n\tName string `store:\"query\"`\n}",
		"named-closed-pair": "type Post struct {\n\tdb.Model\n\tTags []string `gorm:\"->:false;<-:false\"`\n\tName string `store:\"query\"`\n}",
	} {
		dir := t.TempDir()
		writeGo(t, dir, "m.go", "package m\n\nimport \"github.com/zynthara/chok/v2/db\"\n\n"+src+"\n")
		pkg, err := Scan(dir)
		if err != nil {
			t.Errorf("%s: fully closed permissions keep the field inert — the model is legal and must scan, got %v", label, err)
			continue
		}
		if len(pkg.Warnings) != 0 {
			t.Errorf("%s: untagged closed fields warrant no warning, got %q", label, pkg.Warnings)
		}
	}

	tagged := t.TempDir()
	writeGo(t, tagged, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Names []string

type Post struct {
	db.Model
	Names `+"`json:\"names\" store:\"query\" gorm:\"->:false;<-:false\"`"+`
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(tagged)
	if err != nil {
		t.Fatalf("the closed embed is inert — a dead tag, not a fatal shape, got %v", err)
	}
	var deadWarn bool
	for _, w := range pkg.Warnings {
		deadWarn = deadWarn || strings.Contains(w, "Post.Names") && strings.Contains(w, "closed permissions")
	}
	if !deadWarn {
		t.Fatalf("the dead store tag on the closed embed must warn, got %q", pkg.Warnings)
	}

	promo := t.TempDir()
	writeGo(t, promo, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Inner struct {
	X string `+"`json:\"x\" store:\"query\"`"+`
}

type Post struct {
	db.Model
	Inner `+"`gorm:\"->:false;<-:false\"`"+`
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	pkg, err = Scan(promo)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range pkg.Warnings {
		if strings.Contains(w, "Inner") {
			t.Fatalf("a fully closed struct embed never expands, so nothing promotes — no warning, got %q", pkg.Warnings)
		}
	}

	readable := t.TempDir()
	writeGo(t, readable, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Names []string

type Post struct {
	db.Model
	Names `+"`gorm:\"<-:false\"`"+`
	Name string `+"`store:\"query\"`"+`
}
`)
	if _, err := Scan(readable); err == nil || !strings.Contains(err.Error(), "Post.Names") {
		t.Fatalf("`<-:false` alone keeps the field READABLE — the fatal shape stays fatal, got %v", err)
	}
}

// round-8 runtime-latch types: each pins one verified GORM v1.31.2
// behavior the round-8 scan changes mirror.
type R8Meta struct{ Tags []string }

type r8PromotedHost struct {
	R8Meta
	Name string
}

type R8MetaAnon struct{ Round6Names }

type r8NestedAnonHost struct {
	R8MetaAnon
	Name string
}

type R8Child struct{ ID uint }

type r8ArrayHost struct{ Children [2]R8Child }

// R8SerCode pairs a serializer tag with an empty GormDataType: the
// overwrite erases the serializer's DataType, so the field is fatal.
type R8SerCode string

// GormDataType implements the GORM data-type hook, destructively.
func (R8SerCode) GormDataType() string { return "" }

type r8SerHost struct {
	Code R8SerCode `gorm:"serializer:json"`
}

// r8TypeHost proves the opposite: gorm:"type:..." runs AFTER the
// GormDataType overwrite and refills the DataType — a legal column.
type r8TypeHost struct {
	Code R8SerCode `gorm:"type:text"`
}

type r8DurSliceHost struct{ Ds []time.Duration }

type r8DisabledBaseHost struct {
	db.Model `gorm:"->:false"`
	Name     string
}

// TestScan_Round8PromotedFatalPropagates is review round-8 finding 1:
// model-fatality travels the embed graph. A chok base carried through a
// local struct embed still makes the embedder a runtime model, and an
// expanding embed's promoted fields re-enter the OUTER relation gate —
// so a fatal shape inside an embedded struct aborts the embedding
// model, named (promoted) and anonymous (sub-parse) alike. Fully
// closed inner fields stay inert.
func TestScan_Round8PromotedFatalPropagates(t *testing.T) {
	quiet := logger.Default
	logger.Default = logger.Discard
	t.Cleanup(func() { logger.Default = quiet })
	if _, err := schema.Parse(&r8PromotedHost{}, &sync.Map{}, schema.NamingStrategy{}); err == nil || !strings.Contains(err.Error(), "unsupported data type") {
		t.Fatalf("latch: promoted named containers must abort the OUTER parse, got %v", err)
	}
	if _, err := schema.Parse(&r8NestedAnonHost{}, &sync.Map{}, schema.NamingStrategy{}); err == nil {
		t.Fatal("latch: a nested anonymous container must abort through the sub-parse")
	}

	for wantErr, src := range map[string]string{
		"Post.Tags":   "type Base struct{ db.Model }\n\ntype Post struct {\n\tBase\n\tTags []string\n\tName string `store:\"query\"`\n}",
		"Meta.Tags":   "type Meta struct{ Tags []string }\n\ntype Post struct {\n\tdb.Model\n\tMeta\n\tName string `store:\"query\"`\n}",
		"Inner.Names": "type Names []string\n\ntype Inner struct{ Names }\n\ntype Post struct {\n\tdb.Model\n\tInner\n\tName string `store:\"query\"`\n}",
		"B.Tags":      "type B struct{ Tags []string }\n\ntype A struct{ B }\n\ntype Post struct {\n\tdb.Model\n\tA\n\tName string `store:\"query\"`\n}",
	} {
		dir := t.TempDir()
		writeGo(t, dir, "m.go", "package m\n\nimport \"github.com/zynthara/chok/v2/db\"\n\n"+src+"\n")
		if _, err := Scan(dir); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: the fatal shape rides the embed graph and must fail the scan, got %v", wantErr, err)
		}
	}

	closed := t.TempDir()
	writeGo(t, closed, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Meta struct {
	Tags []string `+"`gorm:\"->:false;<-:false\"`"+`
}

type Post struct {
	db.Model
	Meta
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	if _, err := Scan(closed); err != nil {
		t.Fatalf("a fully closed inner field is inert — the model is legal, got %v", err)
	}
}

// TestScan_Round8ArrayKinds is review round-8 finding 2: the relation
// switch accepts only the Struct and Slice kinds — a FIXED ARRAY aborts
// whatever its element resolves to (latched), while a slice keeps
// classifying by its terminal.
func TestScan_Round8ArrayKinds(t *testing.T) {
	quiet := logger.Default
	logger.Default = logger.Discard
	t.Cleanup(func() { logger.Default = quiet })
	if _, err := schema.Parse(&r8ArrayHost{}, &sync.Map{}, schema.NamingStrategy{}); err == nil || !strings.Contains(err.Error(), "unsupported data type") {
		t.Fatalf("latch: a fixed array of structs must abort the model, got %v", err)
	}

	tagged := t.TempDir()
	writeGo(t, tagged, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Child struct{ ID uint }

type Post struct {
	db.Model
	Children [2]Child `+"`json:\"children\" store:\"query\"`"+`
}
`)
	if _, err := Scan(tagged); err == nil || !strings.Contains(err.Error(), "Post.Children") {
		t.Fatalf("a tagged struct array must fail loud, got %v", err)
	}

	untagged := t.TempDir()
	writeGo(t, untagged, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Child struct{ ID uint }

type Post struct {
	db.Model
	Children [2]Child
	Name     string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	if _, err := Scan(untagged); err == nil || !strings.Contains(err.Error(), "Post.Children") {
		t.Fatalf("an untagged struct array still aborts the runtime model, got %v", err)
	}

	sliceOf := t.TempDir()
	writeGo(t, sliceOf, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Child struct {
	ID     uint
	PostID uint
}

type Post struct {
	db.Model
	Grid [][2]Child `+"`json:\"grid\" store:\"query\"`"+`
	Raw  [8]byte    `+"`json:\"raw\" store:\"query\"`"+`
	Name string     `+"`json:\"name\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(sliceOf)
	if err != nil {
		t.Fatalf("a SLICE of struct arrays parses as a relation (outermost kind Slice) — legal, got %v", err)
	}
	got := map[string]bool{}
	for _, f := range pkg.Models[0].Fields {
		got[f.GoName] = true
	}
	if got["Grid"] || !got["Raw"] || !got["Name"] {
		t.Errorf("Grid is a relation (skipped), Raw stays a bytes column, got %v", got)
	}
	var warned bool
	for _, w := range pkg.Warnings {
		warned = warned || strings.Contains(w, "Post.Grid")
	}
	if !warned {
		t.Fatalf("the dead tag on the slice relation must warn, got %q", pkg.Warnings)
	}

	opaque := t.TempDir()
	writeGo(t, opaque, "m.go", `package m

import (
	"example.com/other"

	"github.com/zynthara/chok/v2/db"
)

type Post struct {
	db.Model
	Arr [2]other.Thing `+"`json:\"arr\" store:\"query\"`"+`
}
`)
	if _, err := Scan(opaque); err == nil || !strings.Contains(err.Error(), "cannot statically decide") {
		t.Fatalf("an opaque array element could be a cross-package byte alias — must stay unknowable, got %v", err)
	}
}

// TestScan_Round8DataTypePipeline is review round-8 finding 3: the
// DataType pipeline order is serializer → kind → GormDataType
// (unconditional overwrite) → snapshot → TYPE. So an exact GormDataType
// beats serializer proofs AND the Valuer-derived kind, while
// gorm:"type:..." — running last — rescues everything (latched both
// ways).
func TestScan_Round8DataTypePipeline(t *testing.T) {
	quiet := logger.Default
	logger.Default = logger.Discard
	t.Cleanup(func() { logger.Default = quiet })
	if _, err := schema.Parse(&r8SerHost{}, &sync.Map{}, schema.NamingStrategy{}); err == nil || !strings.Contains(err.Error(), "unsupported data type") {
		t.Fatalf("latch: the GormDataType overwrite must erase the serializer's DataType, got %v", err)
	}
	if s, err := schema.Parse(&r8TypeHost{}, &sync.Map{}, schema.NamingStrategy{}); err != nil || s.LookUpField("code") == nil {
		t.Fatalf("latch: gorm:\"type:...\" runs last and refills the DataType, got %v", err)
	}

	serializer := t.TempDir()
	writeGo(t, serializer, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Code string

func (Code) GormDataType() string { return "" }

type Post struct {
	db.Model
	Code Code `+"`json:\"code\" store:\"query\" gorm:\"serializer:json\"`"+`
}
`)
	if _, err := Scan(serializer); err == nil || !strings.Contains(err.Error(), "Post.Code") || !strings.Contains(err.Error(), "empty string") {
		t.Fatalf("the serializer proof does not survive the GormDataType overwrite, got %v", err)
	}

	valuer := t.TempDir()
	writeGo(t, valuer, "m.go", `package m

import (
	"database/sql/driver"

	"github.com/zynthara/chok/v2/db"
)

type V struct{ S string }

func (v V) Value() (driver.Value, error) { return v.S, nil }

func (V) GormDataType() string { return "" }

type M int64

func (m M) Value() (driver.Value, error) { return int64(m), nil }

func (M) GormDataType() string { return "" }

type Post struct {
	db.Model
	V    V      `+"`json:\"v\" store:\"query\"`"+`
	Name string `+"`json:\"name\" store:\"query\"`"+`
}

type Broken struct {
	db.Model
	M M `+"`json:\"m\" store:\"query\"`"+`
}
`)
	if _, err := Scan(valuer); err == nil || !strings.Contains(err.Error(), "Broken.M") {
		t.Fatalf("a Valuer with a scalar shape and an empty GormDataType is fatal, got %v", err)
	}

	valuerStruct := t.TempDir()
	writeGo(t, valuerStruct, "m.go", `package m

import (
	"database/sql/driver"

	"github.com/zynthara/chok/v2/db"
)

type V struct{ S string }

func (v V) Value() (driver.Value, error) { return v.S, nil }

func (V) GormDataType() string { return "" }

type Post struct {
	db.Model
	V    V      `+"`json:\"v\" store:\"query\"`"+`
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(valuerStruct)
	if err != nil {
		t.Fatalf("a Valuer STRUCT with an empty GormDataType downgrades to a relation (DataType-less struct), got %v", err)
	}
	got := map[string]bool{}
	for _, f := range pkg.Models[0].Fields {
		got[f.GoName] = true
	}
	if got["V"] || !got["Name"] {
		t.Errorf("V is a relation at runtime, not a column, got %v", got)
	}

	rescued := t.TempDir()
	writeGo(t, rescued, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Code string

func (Code) GormDataType() string { return "" }

type CodeI int

func (CodeI) GormDataType() string { return "" }

type Dyn string

func (d Dyn) GormDataType() string {
	if d == "" {
		return ""
	}
	return "text"
}

type Post struct {
	db.Model
	Code  Code `+"`json:\"code\" store:\"query\" gorm:\"type:text\"`"+`
	CodeI `+"`json:\"code_i\" store:\"query\" gorm:\"type:bigint\"`"+`
	Dyn   Dyn  `+"`json:\"dyn\" store:\"query\" gorm:\"type:text\"`"+`
}
`)
	pkg, err = Scan(rescued)
	if err != nil {
		t.Fatalf("gorm:\"type:...\" runs after the overwrite and rescues named AND anonymous scalars (latched), got %v", err)
	}
	got = map[string]bool{}
	for _, f := range pkg.Models[0].Fields {
		got[f.GoName] = true
	}
	for _, want := range []string{"Code", "CodeI", "Dyn"} {
		if !got[want] {
			t.Errorf("%s must generate — TYPE refills the DataType deterministically, got %v", want, got)
		}
	}
}

// TestScan_Round8OpaqueFailsLoud is review round-8 finding 4: an
// unresolvable underlying kind must not be guessed. A tagged
// gorm-embedded cross-package type could promote, keep its column or
// abort; a tagged container with an opaque terminal could be a relation
// or an abort ([]time.Duration is one, latched) — both fail loud now.
func TestScan_Round8OpaqueFailsLoud(t *testing.T) {
	quiet := logger.Default
	logger.Default = logger.Discard
	t.Cleanup(func() { logger.Default = quiet })
	if _, err := schema.Parse(&r8DurSliceHost{}, &sync.Map{}, schema.NamingStrategy{}); err == nil || !strings.Contains(err.Error(), "time.Duration") {
		t.Fatalf("latch: a scalar-kind cross-package terminal aborts the model, got %v", err)
	}

	for label, src := range map[string]string{
		"embedded-datatypes": "import (\n\t\"github.com/zynthara/chok/v2/db\"\n\t\"gorm.io/datatypes\"\n)\n\ntype Post struct {\n\tdb.Model\n\tJ datatypes.JSON `store:\"query\" gorm:\"embedded\"`\n}",
		"embedded-duration":  "import (\n\t\"time\"\n\n\t\"github.com/zynthara/chok/v2/db\"\n)\n\ntype Post struct {\n\tdb.Model\n\tD time.Duration `store:\"query\" gorm:\"embedded\"`\n}",
		"slice-duration":     "import (\n\t\"time\"\n\n\t\"github.com/zynthara/chok/v2/db\"\n)\n\ntype Post struct {\n\tdb.Model\n\tDs []time.Duration `store:\"query\"`\n}",
	} {
		dir := t.TempDir()
		writeGo(t, dir, "m.go", "package m\n\n"+src+"\n")
		if _, err := Scan(dir); err == nil || !strings.Contains(err.Error(), "cannot statically decide") {
			t.Errorf("%s: an opaque kind must fail loud under a store tag, got %v", label, err)
		}
	}

	untagged := t.TempDir()
	writeGo(t, untagged, "m.go", `package m

import (
	"time"

	"github.com/zynthara/chok/v2/db"
)

type Post struct {
	db.Model
	Ds   []time.Duration
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	if _, err := Scan(untagged); err != nil {
		t.Fatalf("an UNTAGGED opaque terminal stays silent — it could be a legal cross-package struct relation (documented residual), got %v", err)
	}
}

// TestScan_Round8DisabledBase is review round-8 finding 5: a chok base
// embed whose own gorm tag disables the expansion (`-` family or fully
// closed permissions) leaves the model without rid/id at runtime
// (latched) — store.New fails, so generating the base trio would mint
// dead references. `-:migration` keeps the field usable and stays a
// working base.
func TestScan_Round8DisabledBase(t *testing.T) {
	if s, err := schema.Parse(&r8DisabledBaseHost{}, &sync.Map{}, schema.NamingStrategy{}); err != nil || s.LookUpField("rid") != nil || s.LookUpField("id") != nil {
		t.Fatalf("latch: a perms-closed base embed must not expand (no rid/id), got err=%v", err)
	}

	for label, tag := range map[string]string{
		"closed":  "`gorm:\"->:false\"`",
		"ignored": "`gorm:\"-\"`",
	} {
		dir := t.TempDir()
		writeGo(t, dir, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Post struct {
	db.Model `+tag+`
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
		if _, err := Scan(dir); err == nil || !strings.Contains(err.Error(), "disabled by its gorm tag") {
			t.Errorf("%s: a disabled base on a generated model must fail loud, got %v", label, err)
		}
	}

	migration := t.TempDir()
	writeGo(t, migration, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Post struct {
	db.Model `+"`gorm:\"-:migration\"`"+`
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(migration)
	if err != nil {
		t.Fatalf("-:migration keeps the base usable, got %v", err)
	}
	var id bool
	for _, f := range pkg.Models[0].Fields {
		id = id || f.GoName == "ID"
	}
	if !id {
		t.Fatal("the base trio still generates for a working base")
	}

	tagless := t.TempDir()
	writeGo(t, tagless, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Ghost struct {
	db.Model `+"`gorm:\"->:false\"`"+`
	Name     string
}
`)
	pkg, err = Scan(tagless)
	if err != nil || len(pkg.Models) != 0 {
		t.Fatalf("a tagless struct generates nothing — no dead refs to guard, got err=%v models=%d", err, len(pkg.Models))
	}
}

// round-9 runtime-latch types: each pins one verified GORM v1.31.2
// behavior the round-9 scan changes mirror.
type R9Box[T any] struct{ Data []T }

type r9GenericHost struct {
	R9Box[string]
	Name string
}

type r9EmbBaseHost struct {
	db.Model `gorm:"embedded;->:false"`
	Name     string
}

type R9DisabledBase struct {
	db.Model `gorm:"-"`
}

type r9WrapHost struct {
	R9DisabledBase
	Name string
}

type r9EmptyTypeHost struct {
	Code string `gorm:"type:"`
	Name string
}

// TestScan_Round9GenericEmbedPropagation is review round-9 finding 1:
// an embedded generic INSTANTIATION promotes its fields with the
// ARGUMENTS substituted — Box[string] carries Data []string (fatal,
// latched) while Box[byte] carries Data []byte (a bytes column) — so
// promotedFatal threads the instantiation environment instead of
// walking the bare generic name.
func TestScan_Round9GenericEmbedPropagation(t *testing.T) {
	quiet := logger.Default
	logger.Default = logger.Discard
	t.Cleanup(func() { logger.Default = quiet })
	if _, err := schema.Parse(&r9GenericHost{}, &sync.Map{}, schema.NamingStrategy{}); err == nil || !strings.Contains(err.Error(), "unsupported data type") {
		t.Fatalf("latch: the promoted Data []string must abort the model, got %v", err)
	}

	fatal := t.TempDir()
	writeGo(t, fatal, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Box[T any] struct{ Data []T }

type Post struct {
	db.Model
	Box[string]
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	if _, err := Scan(fatal); err == nil || !strings.Contains(err.Error(), "Box.Data") {
		t.Fatalf("the promoted generic field must classify under the instantiation's arguments, got %v", err)
	}

	nested := t.TempDir()
	writeGo(t, nested, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Inner[T any] struct{ Data []T }

type Outer[T any] struct{ Inner[T] }

type Post struct {
	db.Model
	Outer[string]
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	if _, err := Scan(nested); err == nil || !strings.Contains(err.Error(), "Inner.Data") {
		t.Fatalf("arguments must ride through nested generic embeds, got %v", err)
	}

	wrapper := t.TempDir()
	writeGo(t, wrapper, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Box[T any] struct{ Data []T }

type Post struct {
	db.Model
	W    Box[string] `+"`gorm:\"embedded\"`"+`
	Name string      `+"`json:\"name\" store:\"query\"`"+`
}
`)
	if _, err := Scan(wrapper); err == nil || !strings.Contains(err.Error(), "Box.Data") {
		t.Fatalf("gorm-embedded generic wrappers promote with arguments too, got %v", err)
	}

	positive := t.TempDir()
	writeGo(t, positive, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Box[T any] struct{ Data []T }

type Post struct {
	db.Model
	Box[byte]
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	if _, err := Scan(positive); err != nil {
		t.Fatalf("Box[byte] promotes Data []byte — a bytes column, the model is legal, got %v", err)
	}
}

// TestScan_Round9BaseEnablement is review round-9 finding 2: base
// enablement follows GORM's EMBEDDED priority (the tag arm has no
// permission gate, so `gorm:"embedded;->:false"` still expands the
// base — latched with rid present), and a DISABLED base hidden inside
// a local wrapper must propagate as fatal — the Go type satisfies
// db.Modeler but the schema has no rid (latched), so store.New fails.
func TestScan_Round9BaseEnablement(t *testing.T) {
	if s, err := schema.Parse(&r9EmbBaseHost{}, &sync.Map{}, schema.NamingStrategy{}); err != nil || s.LookUpField("rid") == nil {
		t.Fatalf("latch: the EMBEDDED tag expands the base regardless of permissions, got err=%v", err)
	}
	if s, err := schema.Parse(&r9WrapHost{}, &sync.Map{}, schema.NamingStrategy{}); err != nil || s.LookUpField("rid") != nil {
		t.Fatalf("latch: a wrapper-disabled base never expands (no rid), got err=%v", err)
	}

	embedded := t.TempDir()
	writeGo(t, embedded, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Post struct {
	db.Model `+"`gorm:\"embedded;->:false\"`"+`
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	pkg, err := Scan(embedded)
	if err != nil {
		t.Fatalf("an EMBEDDED-tagged base expands regardless of permissions — a working base, got %v", err)
	}
	var id bool
	for _, f := range pkg.Models[0].Fields {
		id = id || f.GoName == "ID"
	}
	if !id {
		t.Fatal("the base trio still generates for an EMBEDDED-tagged base")
	}

	wrapped := t.TempDir()
	writeGo(t, wrapped, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Base struct {
	db.Model `+"`gorm:\"-\"`"+`
}

type Post struct {
	Base
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	if _, err := Scan(wrapped); err == nil || !strings.Contains(err.Error(), "disabled by a gorm tag") {
		t.Fatalf("a wrapper-disabled base leaves no rid at runtime and must fail loud, got %v", err)
	}

	rescued := t.TempDir()
	writeGo(t, rescued, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Base struct {
	db.Model `+"`gorm:\"-\"`"+`
}

type Post struct {
	db.Model
	Base
	Name string `+"`json:\"name\" store:\"query\"`"+`
}
`)
	if _, err := Scan(rescued); err != nil {
		t.Fatalf("another ENABLED base wins — the model works, got %v", err)
	}
}

// TestScan_Round9SerializerNotTerminal is review round-9 finding 3: on
// a cross-package NAMED type the method set is invisible — an unseen
// GormDataType returning the empty string would erase the serializer's
// DataType after it runs — so serializer alone proves nothing there;
// only the later-running non-empty gorm:"type:..." is terminal.
// Literal shapes have no method set to hide anything in, so serializer
// keeps proving those.
func TestScan_Round9SerializerNotTerminal(t *testing.T) {
	opaque := t.TempDir()
	writeGo(t, opaque, "m.go", `package m

import (
	"example.com/external"

	"github.com/zynthara/chok/v2/db"
)

type Post struct {
	db.Model
	Code external.Code `+"`json:\"code\" store:\"query\" gorm:\"serializer:json\"`"+`
}
`)
	if _, err := Scan(opaque); err == nil || !strings.Contains(err.Error(), "cannot statically decide") {
		t.Fatalf("serializer on an opaque cross-package type is not terminal — an invisible GormDataType could erase it, got %v", err)
	}

	typed := t.TempDir()
	writeGo(t, typed, "m.go", `package m

import (
	"example.com/external"

	"github.com/zynthara/chok/v2/db"
)

type Post struct {
	db.Model
	Code external.Code `+"`json:\"code\" store:\"query\" gorm:\"type:text\"`"+`
}
`)
	pkg, err := Scan(typed)
	if err != nil {
		t.Fatalf("a non-empty gorm:\"type:...\" runs last — terminal on any type, got %v", err)
	}
	var code bool
	for _, f := range pkg.Models[0].Fields {
		code = code || f.GoName == "Code"
	}
	if !code {
		t.Fatalf("Code must generate under the TYPE proof, got %+v", pkg.Models[0].Fields)
	}

	literals := t.TempDir()
	writeGo(t, literals, "m.go", `package m

import (
	"example.com/external"

	"github.com/zynthara/chok/v2/db"
)

type Post struct {
	db.Model
	Tags  []string         `+"`json:\"tags\" store:\"query\" gorm:\"serializer:json\"`"+`
	Items []external.Thing `+"`json:\"items\" store:\"query\" gorm:\"serializer:json\"`"+`
}
`)
	pkg, err = Scan(literals)
	if err != nil {
		t.Fatalf("literal shapes cannot hide a GormDataType — serializer stays a proof, got %v", err)
	}
	got := map[string]bool{}
	for _, f := range pkg.Models[0].Fields {
		got[f.GoName] = true
	}
	if !got["Tags"] || !got["Items"] {
		t.Errorf("serializer proves literal-rooted shapes, cross-package elements included, got %v", got)
	}
}

// TestScan_Round9TypeIsTerminal is review round-9 finding 4: the
// non-empty TYPE proof precedes method resolution, so it wins even
// over methodUnsure — an alias to an opaque cross-package type (or a
// struct with an unverifiable embed) with gorm:"type:..." is a column,
// exactly like the direct spelling.
func TestScan_Round9TypeIsTerminal(t *testing.T) {
	alias := t.TempDir()
	writeGo(t, alias, "m.go", `package m

import (
	"example.com/external"

	"github.com/zynthara/chok/v2/db"
)

type Code = external.Code

type Crate struct{ Code }

type Post struct {
	db.Model
	Code  Code  `+"`json:\"code\" store:\"query\" gorm:\"type:text\"`"+`
	Crate Crate `+"`json:\"crate\" store:\"query\" gorm:\"type:jsonb\"`"+`
}
`)
	pkg, err := Scan(alias)
	if err != nil {
		t.Fatalf("TYPE is terminal ahead of method resolution — alias spelling included, got %v", err)
	}
	got := map[string]bool{}
	for _, f := range pkg.Models[0].Fields {
		got[f.GoName] = true
	}
	if !got["Code"] || !got["Crate"] {
		t.Errorf("both TYPE-proven fields must generate, got %v", got)
	}

	serializer := t.TempDir()
	writeGo(t, serializer, "m.go", `package m

import (
	"example.com/external"

	"github.com/zynthara/chok/v2/db"
)

type Code = external.Code

type Post struct {
	db.Model
	Code Code `+"`json:\"code\" store:\"query\" gorm:\"serializer:json\"`"+`
}
`)
	if _, err := Scan(serializer); err == nil || !strings.Contains(err.Error(), "cannot statically decide") {
		t.Fatalf("the alias spelling classifies like the direct one — serializer stays non-terminal, got %v", err)
	}
}

// TestScan_Round9EmptyTypeTag is review round-9 finding 5: GORM keys
// the TYPE override on the KEY's presence — `gorm:"type:"` with an
// empty value erases the final DataType, so a would-be column loses
// its DBName (latched) and the store tag is dead; shapes that were
// fatal stay fatal (the erased DataType re-arms the relation gate).
func TestScan_Round9EmptyTypeTag(t *testing.T) {
	if s, err := schema.Parse(&r9EmptyTypeHost{}, &sync.Map{}, schema.NamingStrategy{}); err != nil || s.LookUpField("code") != nil {
		t.Fatalf("latch: an empty TYPE value erases the DataType (no DBName), got err=%v", err)
	}

	dead := t.TempDir()
	writeGo(t, dead, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Post struct {
	db.Model
	Code Code   `+"`json:\"code\" store:\"query\" gorm:\"type:\"`"+`
	Name string `+"`json:\"name\" store:\"query\"`"+`
}

type Code string
`)
	pkg, err := Scan(dead)
	if err != nil {
		t.Fatalf("an empty TYPE on a scalar is a dead field, not a fatal one, got %v", err)
	}
	got := map[string]bool{}
	for _, f := range pkg.Models[0].Fields {
		got[f.GoName] = true
	}
	if got["Code"] || !got["Name"] {
		t.Errorf("the erased column must not generate, got %v", got)
	}
	var warned bool
	for _, w := range pkg.Warnings {
		warned = warned || strings.Contains(w, "Post.Code") && strings.Contains(w, "empty gorm type tag")
	}
	if !warned {
		t.Fatalf("the dead store tag must warn, got %q", pkg.Warnings)
	}

	fatal := t.TempDir()
	writeGo(t, fatal, "m.go", `package m

import "github.com/zynthara/chok/v2/db"

type Post struct {
	db.Model
	Tags []string `+"`json:\"tags\" store:\"query\" gorm:\"type:\"`"+`
}
`)
	if _, err := Scan(fatal); err == nil || !strings.Contains(err.Error(), "Post.Tags") {
		t.Fatalf("an empty TYPE cannot rescue a fatal shape, got %v", err)
	}
}
