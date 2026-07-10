package blessed

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/kernel"
)

type schemaComponent struct {
	descriptor kernel.Descriptor
}

func (c *schemaComponent) Describe() kernel.Descriptor               { return c.descriptor }
func (c *schemaComponent) Init(context.Context, kernel.Kernel) error { return nil }
func (c *schemaComponent) Close(context.Context) error               { return nil }

func schemaModule(kind string, tables ...string) Module {
	return Module{New: func() kernel.Component {
		return &schemaComponent{descriptor: kernel.Descriptor{
			Kind: kind, Schema: kernel.SchemaOwner{Tables: tables},
		}}
	}}
}

func TestSchemaTables_MatchesGeneratedDBCatalog(t *testing.T) {
	tables, err := SchemaTables()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tables, db.FrameworkTables()) {
		t.Fatalf("descriptor catalog and generated db catalog differ: descriptors=%v generated=%v", tables, db.FrameworkTables())
	}
}

func TestSchemaTables_RejectsConflictingOwnership(t *testing.T) {
	_, err := schemaTables([]Module{
		schemaModule("one", "shared"),
		schemaModule("two", "shared"),
	})
	if err == nil || !strings.Contains(err.Error(), `"shared"`) || !strings.Contains(err.Error(), "one") || !strings.Contains(err.Error(), "two") {
		t.Fatalf("duplicate ownership must name the table and both owners: %v", err)
	}
}

func TestSchemaTables_RejectsDuplicateWithinOwnerAndInvalidNames(t *testing.T) {
	if _, err := schemaTables([]Module{schemaModule("one", "same", "same")}); err == nil {
		t.Fatal("a component must not declare the same table twice")
	}
	for _, table := range []string{"", " leading", "schema.table", "semi;colon"} {
		if _, err := schemaTables([]Module{schemaModule("one", table)}); err == nil {
			t.Errorf("invalid table name %q must be rejected", table)
		}
	}
}
