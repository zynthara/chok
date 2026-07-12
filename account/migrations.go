package account

import (
	"embed"
	"io/fs"
	"strings"

	"github.com/zynthara/chok/v2/db"
)

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
