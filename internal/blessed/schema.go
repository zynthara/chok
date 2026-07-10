package blessed

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/zynthara/chok/v2/kernel"
)

var schemaTableNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SchemaTables returns the sorted catalog of tables owned by chok's built-in
// modules. Conflicting ownership and unsafe names are errors rather than
// silently-deduplicated declarations.
func SchemaTables() ([]string, error) {
	return schemaTables(Modules())
}

func schemaTables(modules []Module) ([]string, error) {
	owners := make(map[string]kernel.Key)
	for _, module := range modules {
		descriptor := module.New().Describe()
		owner := kernel.KeyOf(descriptor)
		for _, table := range descriptor.Schema.Tables {
			if !schemaTableNameRE.MatchString(table) {
				return nil, fmt.Errorf("blessed: component %s declares invalid schema table %q", owner, table)
			}
			if previous, exists := owners[table]; exists {
				return nil, fmt.Errorf("blessed: schema table %q is declared by both %s and %s", table, previous, owner)
			}
			owners[table] = owner
		}
	}

	tables := make([]string, 0, len(owners))
	for table := range owners {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	return tables, nil
}
