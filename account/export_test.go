package account

import (
	"io/fs"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/internal/testseq"
)

// MigrationPrefixForTest constructs an exact versioned-schema prefix without
// exposing account's embedded migration filesystem in production builds.
func MigrationPrefixForTest(t testing.TB, upTo int64) db.Sequence {
	t.Helper()
	root, err := fs.Sub(migrationAssets, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	seq, err := db.OwnedSequence(
		"account",
		testseq.PrefixFS(t, root, upTo),
		db.Baseline{},
		db.SequenceOwner("github.com/zynthara/chok/v2/account"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// MigrationSQLForTest returns one dialect-resolved account migration body.
func MigrationSQLForTest(t testing.TB, dialect string, version int64) string {
	t.Helper()
	root, err := fs.Sub(migrationAssets, "migrations/"+dialect)
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := db.LoadMigrations(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.Version == version {
			return migration.SQL
		}
	}
	t.Fatalf("account: migration version %d not found for %s", version, dialect)
	return ""
}
