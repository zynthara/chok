package docgen

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/zynthara/chok/v2/internal/blessed"
	"github.com/zynthara/chok/v2/kernel"
)

func TestFrameworkTablesSource_IsFormattedGo(t *testing.T) {
	source, err := FrameworkTablesSource()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "framework_tables_gen.go", source, parser.AllErrors); err != nil {
		t.Fatalf("generated framework table source must parse: %v\n%s", err, source)
	}
	for _, table := range []string{
		"audit_logs",
		"casbin_rule",
		"identities",
		"schema_migrations",
		"schema_migrations_chok_account",
		"schema_migrations_chok_audit",
		"schema_migrations_chok_authz",
		"users",
	} {
		if !strings.Contains(string(source), `"`+table+`"`) {
			t.Errorf("generated framework table source missing %q", table)
		}
	}
}

func TestComponentsTable_CoversEveryBlessedModule(t *testing.T) {
	en := ComponentsTable("en")
	zh := ComponentsTable("zh")
	for _, m := range blessed.Modules() {
		section := kernel.SectionKeyOf(m.New().Describe())
		for lang, table := range map[string]string{"en": en, "zh": zh} {
			if !strings.Contains(table, "`"+section+"`") {
				t.Errorf("%s table must list section %q", lang, section)
			}
		}
	}
	// Spot-check semantics the table is supposed to carry.
	if !strings.Contains(en, "| `web.Module()` | `http` |") {
		t.Error("web must be listed under its http section key")
	}
	for _, want := range []string{"| `audit.Module()`", "false"} {
		if !strings.Contains(en, want) {
			t.Errorf("components table missing %q", want)
		}
	}
}

func TestConfigReference_MarksSensitiveAndEnums(t *testing.T) {
	ref := ConfigReference()
	for _, want := range []string{
		"## `db`",
		"| `postgres.password` | string | — | restart | **sensitive** |",
		"one of: sqlite \\| mysql \\| postgres",
		"| `signing_key` | string | — | restart | **sensitive** |",
		"| `probe_timeout` | duration | `3s` | hot |",
		"*Multi-instance.*",
	} {
		if !strings.Contains(ref, want) {
			t.Errorf("config reference missing %q", want)
		}
	}
}

func TestJSONSchema_ValidatesExampleYAMLs(t *testing.T) {
	schema, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var js map[string]any
	if err := json.Unmarshal(schema, &js); err != nil {
		t.Fatalf("schema must be valid JSON: %v", err)
	}

	repoRoot := filepath.Join("..", "..")
	for _, rel := range []string{
		"examples/blog/chok.yaml",
		"internal/fixture/m4/chok.yaml",
		"internal/fixture/m3/chok.yaml",
		"internal/fixture/m2/chok.yaml",
	} {
		path := filepath.Join(repoRoot, rel)
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // fixture without yaml — nothing to validate
			}
			t.Fatal(err)
		}
		var tree map[string]any
		if err := yaml.Unmarshal(raw, &tree); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if errs := ValidateYAMLTree(schema, tree); len(errs) > 0 {
			t.Errorf("%s must validate against the generated schema:", rel)
			for _, e := range errs {
				t.Errorf("  %v", e)
			}
		}
	}
}

func TestJSONSchema_CatchesRealMistakes(t *testing.T) {
	schema, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string]any{
		"db": map[string]any{
			"driver":  "oracle",                        // enum violation
			"migrate": true,                            // type violation (enum)
			"sqlite":  map[string]any{"pathh": "x.db"}, // unknown key (typo)
		},
		"http": map[string]any{
			"addr": 8080, // must be string
		},
		"myapp": map[string]any{"anything": "goes"}, // business section: allowed
	}
	errs := ValidateYAMLTree(schema, bad)
	if len(errs) < 4 {
		t.Fatalf("expected ≥4 violations (driver enum, migrate enum, sqlite typo, addr type), got %d: %v", len(errs), errs)
	}
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	for _, want := range []string{"$.db.driver", "$.db.migrate", "pathh", "$.http.addr"} {
		if !strings.Contains(joined, want) {
			t.Errorf("violations must mention %s, got:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "myapp") {
		t.Errorf("business sections must stay unconstrained, got:\n%s", joined)
	}
}

func TestJSONSchema_SensitiveFieldsCarryNoSecretsAndAreWriteOnly(t *testing.T) {
	schema, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var js map[string]any
	if err := json.Unmarshal(schema, &js); err != nil {
		t.Fatal(err)
	}
	// account.signing_key: writeOnly, no default (M5 prompt: the schema
	// must never embed a real-looking default secret sample).
	acct := js["properties"].(map[string]any)["account"].(map[string]any)
	sk := acct["properties"].(map[string]any)["signing_key"].(map[string]any)
	if sk["writeOnly"] != true {
		t.Error("signing_key must be writeOnly")
	}
	if _, has := sk["default"]; has {
		t.Error("sensitive fields must not carry defaults in the schema")
	}
	// providers.google.client_secret marked writeOnly through the
	// heuristic mirror.
	prov := acct["properties"].(map[string]any)["providers"].(map[string]any)
	google := prov["properties"].(map[string]any)["google"].(map[string]any)
	cs := google["properties"].(map[string]any)["client_secret"].(map[string]any)
	if cs["writeOnly"] != true {
		t.Error("provider client_secret must be writeOnly")
	}
}

func TestInjectBlock_ReplacesOnlyTheMarkedRegion(t *testing.T) {
	doc := "# Title\n\nintro\n\n<!-- gen:components -->\nOLD\n<!-- /gen:components -->\n\noutro\n"
	out, err := InjectBlock(doc, "components", "NEW TABLE\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "NEW TABLE") || strings.Contains(out, "OLD") {
		t.Fatalf("block not replaced:\n%s", out)
	}
	if !strings.HasPrefix(out, "# Title") || !strings.Contains(out, "outro") {
		t.Fatalf("content outside markers must be untouched:\n%s", out)
	}
	// Idempotency: injecting the same content is a fixed point.
	again, err := InjectBlock(out, "components", "NEW TABLE\n")
	if err != nil {
		t.Fatal(err)
	}
	if again != out {
		t.Fatal("InjectBlock must be idempotent for identical content")
	}
	if _, err := InjectBlock("no markers here", "components", "x"); err == nil {
		t.Fatal("missing markers must error")
	}
}
