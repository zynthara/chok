package jobs

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
)

var jobsTables = []string{"chok_jobs_messages", "chok_jobs_watermarks"}

// describeSchema renders tables as the database reports them: each
// column's type, length, full type, nullability, default and key, and
// each index's columns (sorted) and uniqueness.
func describeSchema(t *testing.T, d *db.DB, tables ...string) string {
	t.Helper()
	m := d.Unsafe(t.Context()).Migrator()
	var b strings.Builder
	for _, table := range tables {
		cols, err := m.ColumnTypes(table)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		var lines []string
		for _, c := range cols {
			length, _ := c.Length()
			nullable, _ := c.Nullable()
			def, _ := c.DefaultValue()
			pk, _ := c.PrimaryKey()
			// The full type where the dialect reports one: MySQL's
			// datetime(3) and bigint unsigned matter to the relay.
			full, _ := c.ColumnType()
			lines = append(lines, fmt.Sprintf("  %s %s(%d) %q null=%v default=%q pk=%v", c.Name(), strings.ToLower(c.DatabaseTypeName()), length, strings.ToLower(full), nullable, def, pk))
		}
		idx, err := m.GetIndexes(table)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		for _, i := range idx {
			unique, _ := i.Unique()
			pk, _ := i.PrimaryKey()
			// Postgres reports an index's columns in no fixed order.
			cols := slices.Sorted(slices.Values(i.Columns()))
			lines = append(lines, fmt.Sprintf("  index %s %v unique=%v pk=%v", i.Name(), cols, unique, pk))
		}
		slices.Sort(lines)
		fmt.Fprintf(&b, "%s\n%s\n", table, strings.Join(lines, "\n"))
	}
	return b.String()
}

func versionedConfig(t *testing.T) db.Config {
	t.Helper()
	cfg := dbtest.Config(t)
	cfg.Migrate = "versioned"
	return cfg
}

func registerJobs(t *testing.T, d *db.DB) {
	t.Helper()
	if err := d.RegisterSchema("jobs", tables(), migrations()); err != nil {
		t.Fatal(err)
	}
}

// TestMigrations_VersionedMatchesAuto: the SQL files build, on this lane,
// the schema migrate: auto builds from the row types. It is v2's three
// TestMigrationSequence_*SchemaEquivalent tests in one, run by every lane;
// v2's baseline adoption is gone with the per-owner ledgers (one ledger,
// plan D13).
func TestMigrations_VersionedMatchesAuto(t *testing.T) {
	auto := dbtest.Open(t)
	registerJobs(t, auto)
	versioned := dbtest.OpenConfig(t, versionedConfig(t))
	registerJobs(t, versioned)
	want, got := describeSchema(t, auto, jobsTables...), describeSchema(t, versioned, jobsTables...)
	if want != got {
		t.Fatalf("versioned schema differs from auto on %s\nauto:\n%s\nversioned:\n%s", dbtest.Driver(), want, got)
	}
	for _, index := range []string{
		"idx_chok_jobs_messages_scan [created_at id] unique=false",
		"idx_chok_jobs_messages_topic [created_at id topic] unique=false",
	} {
		if !strings.Contains(want, index) {
			t.Fatalf("the scan indexes:\n%s", want)
		}
	}
	// Auto migrating over the versioned schema finds nothing to change.
	if err := versioned.Unsafe(t.Context()).AutoMigrate(&message{}, &watermarkRow{}); err != nil {
		t.Fatal(err)
	}
	if again := describeSchema(t, versioned, jobsTables...); again != got {
		t.Fatalf("auto migrate changed the versioned schema:\n%s", again)
	}
}
