package account

import (
	"embed"
	"io/fs"
	"strings"

	"github.com/zynthara/chok/v2/db"
)

// Baseline fingerprints under migrations/baseline/ are generated from a
// fresh AutoMigrate database. Regeneration is a local maintainer flow and
// two-pass, because fingerprints embed at build time:
//
//	CHOK_UPDATE_BASELINES=1 go test ./account ./audit ./authz -run TestMigrationSequence
//	CHOK_UPDATE_BASELINES=1 CHOK_TEST_DRIVER=postgres CHOK_TEST_PG_DSN=<dsn> go test ./account ./audit ./authz -run TestMigrationSequence
//	CHOK_UPDATE_BASELINES=1 CHOK_TEST_MYSQL_DSN=<dsn> go test ./account ./audit ./authz -run TestMigrationSequence
//
// then rerun without the variable so the equivalence gates verify the result.
// Discipline by cause: a model/schema change ships with a matching migration
// file and an EquivalentVersion bump; a pure gorm-introspection or
// normalization change (schema unchanged) regenerates all three dialect
// files together with the reason recorded in the commit — the version does
// not move.
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
	}, db.SequenceOwner("github.com/zynthara/chok/v2/account"))
	if err != nil {
		panic(err)
	}
	return seq
}
