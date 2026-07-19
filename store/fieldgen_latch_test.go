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
	"testing"

	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/internal/fieldgen"
	"github.com/zynthara/chok/v2/internal/fieldgen/fixture"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

const fixtureDir = "../internal/fieldgen/fixture"

// scanFixture returns the generator's view of a fixture model.
func scanFixture(t *testing.T, model string) fieldgen.Model {
	t.Helper()
	pkg, err := fieldgen.Scan(fixtureDir)
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

// TestFieldGen_SemanticLatch_CompiledSymbols closes the loop through the
// checked-in generated file: the constants a user's code would actually
// reference (compiled from fixture/chok_fields_gen.go, byte-pinned by
// the fieldgen golden test) resolve against live stores.
func TestFieldGen_SemanticLatch_CompiledSymbols(t *testing.T) {
	h := dbtest.Open(t)
	arts := New[fixture.Article](h, log.Empty())
	shadow := New[fixture.ShadowID](h, log.Empty())

	for _, tc := range []struct {
		name  string
		fm    map[string]string
		value string
	}{
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
	} {
		if _, err := where.ResolveField(tc.fm, tc.value); err != nil {
			t.Errorf("%s: compiled constant %q rejected by the runtime map: %v", tc.name, tc.value, err)
		}
	}
}
