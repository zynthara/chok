package store

// The fieldgen semantic latch. `chok gen fields` re-derives public field
// names from source (internal/fieldgen), while store.New derives them
// from the parsed GORM schema (tagDeclaredFields) — two implementations
// of one naming contract. This file is the only thing pinning them
// together: it scans the compiled fixture package with the real
// generator, builds real stores over the same models, and asserts the
// generated value set IS the whitelist key set, face by face. If either
// side drifts (a new fallback rule, a NamingStrategy change, a base-trio
// edit), this fails before any user sees a generated constant the
// runtime rejects.

import (
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/internal/fieldgen"
	"github.com/zynthara/chok/v2/internal/fieldgen/fixture"
	"github.com/zynthara/chok/v2/internal/fieldgen/fixture/edge"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

const (
	fixtureDir     = "../internal/fieldgen/fixture"
	edgeFixtureDir = "../internal/fieldgen/fixture/edge"
)

// scanFixtureDir returns the generator's view of one model in dir.
func scanFixtureDir(t *testing.T, dir, model string) fieldgen.Model {
	t.Helper()
	pkg, err := fieldgen.Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range pkg.Models {
		if m.Name == model {
			return m
		}
	}
	t.Fatalf("fixture model %s not found in scan (got %+v)", model, pkg.Models)
	return fieldgen.Model{}
}

// scanFixture returns the generator's view of a main-fixture model.
func scanFixture(t *testing.T, model string) fieldgen.Model {
	t.Helper()
	return scanFixtureDir(t, fixtureDir, model)
}

// assertLatch checks both directions between the generated surface and a
// live store's maps: every generated value resolves on its declared
// face(s) and only there, and every whitelist key is covered by a
// generated value — the generator neither invents nor misses fields.
func assertLatch(t *testing.T, m fieldgen.Model, queryMap, updateMap map[string]string) {
	t.Helper()
	genQuery := map[string]bool{}
	genUpdate := map[string]bool{}
	for _, f := range m.Fields {
		if f.Query {
			genQuery[f.Value] = true
		}
		if f.Update {
			genUpdate[f.Value] = true
		}
	}
	for _, f := range m.Fields {
		if f.Query {
			if _, err := where.ResolveField(queryMap, f.Value); err != nil {
				t.Errorf("%s.%s: generated value %q must resolve on the query face: %v", m.Name, f.GoName, f.Value, err)
			}
		} else if !genQuery[f.Value] { // the key may ride another field's query face
			if _, err := where.ResolveField(queryMap, f.Value); err == nil {
				t.Errorf("%s.%s: %q is not query-declared yet resolves on the query face", m.Name, f.GoName, f.Value)
			}
		}
		if f.Update {
			if _, err := where.ResolveField(updateMap, f.Value); err != nil {
				t.Errorf("%s.%s: generated value %q must resolve on the update face: %v", m.Name, f.GoName, f.Value, err)
			}
		} else if !genUpdate[f.Value] {
			if _, err := where.ResolveField(updateMap, f.Value); err == nil {
				t.Errorf("%s.%s: %q is not update-declared yet resolves on the update face", m.Name, f.GoName, f.Value)
			}
		}
	}
	for key := range queryMap {
		if !genQuery[key] {
			t.Errorf("%s: runtime query key %q has no generated reference — the generator missed a field", m.Name, key)
		}
	}
	for key := range updateMap {
		if !genUpdate[key] {
			t.Errorf("%s: runtime update key %q has no generated reference — the generator missed a field", m.Name, key)
		}
	}
}

func TestFieldGen_SemanticLatch_TagDeclaredSurfaces(t *testing.T) {
	h := dbtest.Open(t)

	arts := New[fixture.Article](h, log.Empty())
	assertLatch(t, scanFixture(t, "Article"), arts.queryFieldMap, arts.updateFieldMap)

	shadow := New[fixture.ShadowID](h, log.Empty())
	m := scanFixture(t, "ShadowID")
	assertLatch(t, m, shadow.queryFieldMap, shadow.updateFieldMap)

	// Method-proven columns (exact driver.Valuer, GormDataType) —
	// review round-2: both are real runtime columns, including the
	// anonymous Valuer embed.
	wallets := New[fixture.Wallet](h, log.Empty())
	assertLatch(t, scanFixture(t, "Wallet"), wallets.queryFieldMap, wallets.updateFieldMap)

	// The base trio yields to a declared field owning the public name:
	// exactly one generated symbol carries "id", and it is the user's.
	for _, f := range m.Fields {
		if f.GoName == "ID" {
			t.Errorf("ShadowID: base ID symbol must be skipped when a declared field owns the \"id\" key")
		}
		if f.GoName == "PublicID" && (f.Value != "id" || f.Base) {
			t.Errorf("ShadowID.PublicID = %+v, want the declared \"id\" key", f)
		}
	}
	if col, err := where.ResolveField(shadow.queryFieldMap, "id"); err != nil || col != "public_id" {
		t.Errorf("ShadowID query \"id\" resolves to (%q, %v), want the user column public_id", col, err)
	}
}

// TestFieldGen_SemanticLatch_ProtectedAndVersion pins the red lines: no
// generated symbol on any face for version, and the update face carries
// no base or otherwise protected column — matching tagDeclaredFields
// (query excludes version) and protectedUpdateColumns (update excludes
// the whole lifecycle set).
func TestFieldGen_SemanticLatch_ProtectedAndVersion(t *testing.T) {
	h := dbtest.Open(t)
	arts := New[fixture.Article](h, log.Empty())

	m := scanFixture(t, "Article")
	for _, f := range m.Fields {
		if f.Value == "version" {
			t.Errorf("Article.%s: version must not be generated on any face", f.GoName)
		}
		if f.Base && f.Update {
			t.Errorf("Article.%s: base symbols are query-only", f.GoName)
		}
		if f.Update {
			col, err := where.ResolveField(arts.updateFieldMap, f.Value)
			if err != nil {
				t.Errorf("Article.%s: %v", f.GoName, err)
				continue
			}
			if isProtectedUpdateColumn(arts.modelSchema, col) {
				t.Errorf("Article.%s: generated update reference %q resolves to protected column %q", f.GoName, f.Value, col)
			}
		}
	}
	if _, err := where.ResolveField(arts.queryFieldMap, "version"); err == nil {
		t.Error("version must stay outside the query whitelist")
	}
}

// TestFieldGen_SemanticLatch_AliasStability pins decision 2 of the
// design: generated values are whitelist KEYS, so a WithColumnAlias
// assembly re-points columns without invalidating a single reference.
func TestFieldGen_SemanticLatch_AliasStability(t *testing.T) {
	h := dbtest.Open(t)
	aliased := New[fixture.Article](h, log.Empty(),
		WithColumnAlias("aliased", "internal_note"))

	m := scanFixture(t, "Article")
	for _, f := range m.Fields {
		if !f.Query {
			continue
		}
		if _, err := where.ResolveField(aliased.queryFieldMap, f.Value); err != nil {
			t.Errorf("Article.%s: %q must survive WithColumnAlias assembly: %v", f.GoName, f.Value, err)
		}
	}
	if col, _ := where.ResolveField(aliased.queryFieldMap, "aliased"); col != "internal_note" {
		t.Errorf("alias must re-point the column (got %q), while the key stays stable", col)
	}
	// The base "id" key rides the automatic rid alias — still the same key.
	if col, _ := where.ResolveField(aliased.queryFieldMap, "id"); col != "rid" {
		t.Errorf("base id key must auto-alias to the rid column, got %q", col)
	}
}

// TestFieldGen_SemanticLatch_EdgeShapes pins the review round-1 shapes
// against real stores, both directions:
//
//   - a store-tagged relation field is a column on NEITHER side —
//     runtime (DBName empty) and generator (classified, skipped, warned)
//     agree, so the exact-set latch holds for Contact;
//   - promoted embeds exist at runtime but not in the generated surface
//     — Event (whole surface promoted, scan absent + warned) and Entry
//     (generated ⊂ runtime with the missing set exactly {"by"}) pin the
//     documented residual instead of letting it drift silently.
func TestFieldGen_SemanticLatch_EdgeShapes(t *testing.T) {
	h := dbtest.Open(t)

	pkg, err := fieldgen.Scan(edgeFixtureDir)
	if err != nil {
		t.Fatal(err)
	}

	// Contact: relations excluded on both sides — full latch holds.
	// Badge doubly so: its wrong-signature Value method must not count
	// as driver.Valuer (review round-2); Sticker triply — a defined
	// type over a real Valuer loses the method set (review round-3).
	contacts := New[edge.Contact](h, log.Empty())
	assertLatch(t, scanFixtureDir(t, edgeFixtureDir, "Contact"), contacts.queryFieldMap, contacts.updateFieldMap)
	// profile: plain struct; badge: wrong-sig Value; sticker: defined
	// type sheds the method set; purse: same-depth ambiguity; chest:
	// shallow wrong-sig shadows the promoted exact one (round-4);
	// satchel: two same-depth PATHS to one Valuer are just as ambiguous;
	// parcel: an alias to *driver.Value keeps the pointer (round-5).
	for _, relation := range []string{"profile", "badge", "sticker", "purse", "chest", "satchel", "parcel"} {
		if _, err := where.ResolveField(contacts.queryFieldMap, relation); err == nil {
			t.Errorf("relation key %q must not exist in the runtime query map either", relation)
		}
	}
	if col, err := where.ResolveField(contacts.queryFieldMap, edge.ContactFields.ProfileID); err != nil || col != "profile_id" {
		t.Errorf("FK column stays a normal reference, got (%q, %v)", col, err)
	}

	// Parent: a defined slice resolves to a has-many relation through
	// the type chain (review round-2) — excluded on both sides.
	parents := New[edge.Parent](h, log.Empty())
	assertLatch(t, scanFixtureDir(t, edgeFixtureDir, "Parent"), parents.queryFieldMap, parents.updateFieldMap)
	if _, err := where.ResolveField(parents.queryFieldMap, "children"); err == nil {
		t.Error("defined-slice relation must not be a runtime query key")
	}

	// Player: an anonymous GormDataType struct expands as an embed at
	// runtime (review round-3) — the tag on the embed line is dead on
	// both sides, so the exact-set latch holds without a "level" key.
	players := New[edge.Player](h, log.Empty())
	assertLatch(t, scanFixtureDir(t, edgeFixtureDir, "Player"), players.queryFieldMap, players.updateFieldMap)
	if _, err := where.ResolveField(players.queryFieldMap, "level"); err == nil {
		t.Error("anonymous GormDataType embed must not be a runtime query key")
	}

	// Event: runtime model via promotion, generator has nothing — the
	// gap must be exactly the promoted key, and loudly diagnosed.
	events := New[edge.Event](h, log.Empty())
	if _, err := where.ResolveField(events.queryFieldMap, "actor"); err != nil {
		t.Fatalf("runtime must promote the embedded tag (fixture premise): %v", err)
	}
	for _, m := range pkg.Models {
		if m.Name == "Event" {
			t.Fatal("Event must not scan as a model — its surface is promoted")
		}
	}
	var eventWarned bool
	for _, w := range pkg.Warnings {
		if strings.Contains(w, "Event") && strings.Contains(w, "AuditBase") {
			eventWarned = true
		}
	}
	if !eventWarned {
		t.Fatalf("the promoted-only model must be diagnosed, got %q", pkg.Warnings)
	}

	// Entry: generated ⊂ runtime, missing exactly the promoted "by".
	entries := New[edge.Entry](h, log.Empty())
	m := scanFixtureDir(t, edgeFixtureDir, "Entry")
	genQuery := map[string]bool{}
	for _, f := range m.Fields {
		if f.Query {
			genQuery[f.Value] = true
			if _, err := where.ResolveField(entries.queryFieldMap, f.Value); err != nil {
				t.Errorf("Entry.%s: generated value %q must resolve: %v", f.GoName, f.Value, err)
			}
		}
	}
	var missing []string
	for key := range entries.queryFieldMap {
		if !genQuery[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) != 1 || missing[0] != "by" {
		t.Errorf("the generated/runtime gap must be exactly the promoted key [by], got %v", missing)
	}
	if len(entries.updateFieldMap) != 0 {
		t.Errorf("Entry declares no update face, runtime has %v", entries.updateFieldMap)
	}
	if _, err := where.ResolveField(entries.queryFieldMap, edge.EntryFields.Title); err != nil {
		t.Errorf("compiled edge constant must resolve: %v", err)
	}

	// Ticket: identical promotion contract with an UNEXPORTED target
	// type — GORM promotes by field name (review round-2), so the
	// runtime has "ref", the scan has only Subject, and the warning
	// must fire exactly like Entry's.
	tickets := New[edge.Ticket](h, log.Empty())
	if _, err := where.ResolveField(tickets.queryFieldMap, "ref"); err != nil {
		t.Fatalf("runtime must promote the unexported-target embed (fixture premise): %v", err)
	}
	tm := scanFixtureDir(t, edgeFixtureDir, "Ticket")
	genTicket := map[string]bool{}
	for _, f := range tm.Fields {
		if f.Query {
			genTicket[f.Value] = true
		}
	}
	var ticketMissing []string
	for key := range tickets.queryFieldMap {
		if !genTicket[key] {
			ticketMissing = append(ticketMissing, key)
		}
	}
	if len(ticketMissing) != 1 || ticketMissing[0] != "ref" {
		t.Errorf("Ticket's generated/runtime gap must be exactly [ref], got %v", ticketMissing)
	}
	var ticketWarned bool
	for _, w := range pkg.Warnings {
		if strings.Contains(w, "Ticket") && strings.Contains(w, "Extra") {
			ticketWarned = true
		}
	}
	if !ticketWarned {
		t.Fatalf("the unexported-target promotion must be diagnosed, got %q", pkg.Warnings)
	}
}

// TestFieldGen_SemanticLatch_CompiledSymbols closes the loop through the
// checked-in generated file: the constants a user's code would actually
// reference (compiled from fixture/chok_fields_gen.go, byte-pinned by
// the fieldgen golden test) resolve against live stores.
func TestFieldGen_SemanticLatch_CompiledSymbols(t *testing.T) {
	h := dbtest.Open(t)
	arts := New[fixture.Article](h, log.Empty())
	shadow := New[fixture.ShadowID](h, log.Empty())
	wallets := New[fixture.Wallet](h, log.Empty())

	for _, tc := range []struct {
		name  string
		fm    map[string]string
		value string
	}{
		{"Article anonymous code/query", arts.queryFieldMap, fixture.ArticleFields.Code},
		{"Article published_at/query", arts.queryFieldMap, fixture.ArticleFields.PublishedAt},
		{"Article title/query", arts.queryFieldMap, fixture.ArticleFields.Title},
		{"Article title/update", arts.updateFieldMap, fixture.ArticleFields.Title},
		{"Article body/update", arts.updateFieldMap, fixture.ArticleFields.Body},
		{"Article secret/query", arts.queryFieldMap, fixture.ArticleFields.Secret},
		{"Article internal note/query", arts.queryFieldMap, fixture.ArticleFields.InternalNote},
		{"Article http status/query", arts.queryFieldMap, fixture.ArticleFields.HTTPStatus},
		{"Article legacy body/update", arts.updateFieldMap, fixture.ArticleFields.LegacyBody},
		{"Article base id/query", arts.queryFieldMap, fixture.ArticleFields.ID},
		{"Article base created_at/query", arts.queryFieldMap, fixture.ArticleFields.CreatedAt},
		{"Article base updated_at/query", arts.queryFieldMap, fixture.ArticleFields.UpdatedAt},
		{"ShadowID declared id/query", shadow.queryFieldMap, fixture.ShadowIDFields.PublicID},
		{"ShadowID name/update", shadow.updateFieldMap, fixture.ShadowIDFields.Name},
		{"ShadowID defined-scalar kind/query", shadow.queryFieldMap, fixture.ShadowIDFields.Kind},
		{"Wallet valuer-embed money/query", wallets.queryFieldMap, fixture.WalletFields.Money},
		{"Wallet gorm-data-type flags/update", wallets.updateFieldMap, fixture.WalletFields.Flags},
		{"Wallet promoted-valuer box/query", wallets.queryFieldMap, fixture.WalletFields.Box},
		{"Wallet defined-time seal/query", wallets.queryFieldMap, fixture.WalletFields.Seal},
		{"Wallet byte-array token/query", wallets.queryFieldMap, fixture.WalletFields.Token},
		{"Wallet json-serializer meta/query", wallets.queryFieldMap, fixture.WalletFields.Meta},
		{"Wallet gdt-time clock/query", wallets.queryFieldMap, fixture.WalletFields.Clock},
		{"Wallet alias-sig locker/query", wallets.queryFieldMap, fixture.WalletFields.Locker},
		{"Wallet generic payload/query", wallets.queryFieldMap, fixture.WalletFields.Payload},
		{"Wallet anonymous generic chunk/query", wallets.queryFieldMap, fixture.WalletFields.Bytes},
		{"Wallet alias-embedded valuer vault/query", wallets.queryFieldMap, fixture.WalletFields.Vault},
		{"Wallet defined alias-elem slice strip/query", wallets.queryFieldMap, fixture.WalletFields.Strip},
		{"Wallet alias-elem slice slab/query", wallets.queryFieldMap, fixture.WalletFields.Slab},
		{"Wallet alias-instantiated generic packed/query", wallets.queryFieldMap, fixture.WalletFields.Packed},
	} {
		if _, err := where.ResolveField(tc.fm, tc.value); err != nil {
			t.Errorf("%s: compiled constant %q rejected by the runtime map: %v", tc.name, tc.value, err)
		}
	}
}
