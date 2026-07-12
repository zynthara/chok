package account

import (
	"embed"
	"io/fs"
	"strings"

	"github.com/zynthara/chok/v2/db"
)

// Baseline fingerprints under migrations/baseline/ are generated from a
// fresh AutoMigrate database. Regenerate them only for a deliberate model or
// gorm-introspection change (two-pass — fingerprints embed at build time):
//
//	CHOK_UPDATE_BASELINES=1 go test ./account ./audit ./authz -run TestMigrationSequence
//
// repeated under the Postgres/MySQL lanes (CHOK_TEST_DRIVER / DSN envs) for
// the other dialect files, then rerun without the variable: the equivalence
// gates verify the result, and the baseline change must ship with a matching
// migration + EquivalentVersion bump.
//
//go:embed migrations/*/*.sql migrations/baseline/*.json
var migrationAssets embed.FS

var migrationSequence = mustMigrationSequence()

// MigrationSequence returns account's immutable, dialect-resolved migration
// history. Runtime modules and the chok CLI consume this same descriptor.
func MigrationSequence() db.Sequence { return migrationSequence }

func mustMigrationSequence() db.Sequence {
	root, err := fs.Sub(migrationAssets, "migrations")
	if err != nil {
		panic(err)
	}
	fingerprints := make(map[string]string, 3)
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		raw, err := fs.ReadFile(root, "baseline/"+dialect+".json")
		if err != nil {
			panic(err)
		}
		fingerprints[dialect] = strings.TrimSpace(string(raw))
	}
	seq, err := db.OwnedSequence("account", root, db.Baseline{
		EquivalentVersion: 2,
		Tables:            []string{"users", "identities"},
		Fingerprints:      fingerprints,
	})
	if err != nil {
		panic(err)
	}
	return seq
}
