package identity

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
)

var identityTables = []string{"users", "identities", "audit_logs"}

// describeSchema renders tables as the database reports them: each
// column's type, length, nullability, default and key, and each index's
// columns (sorted) and uniqueness.
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
			lines = append(lines, fmt.Sprintf("  %s %s(%d) null=%v default=%q pk=%v", c.Name(), strings.ToLower(c.DatabaseTypeName()), length, nullable, def, pk))
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

// TestMigrations_VersionedMatchesAuto: the SQL files build, on this lane,
// the schema migrate: auto builds from the models.
func TestMigrations_VersionedMatchesAuto(t *testing.T) {
	auto := dbtest.Open(t)
	newIdentityOn(t, auto, Config{})
	versioned := dbtest.OpenConfig(t, versionedConfig(t))
	newIdentityOn(t, versioned, Config{})
	want, got := describeSchema(t, auto, identityTables...), describeSchema(t, versioned, identityTables...)
	if want != got {
		t.Fatalf("versioned schema differs from auto on %s\nauto:\n%s\nversioned:\n%s", dbtest.Driver(), want, got)
	}
	if !strings.Contains(want, "uk_user_email [delete_token email] unique=true") || !strings.Contains(want, "uk_identity_provider [delete_token provider provider_account_id] unique=true") {
		t.Fatalf("the SoftUnique indexes:\n%s", want)
	}
	// Auto migrating over the versioned schema finds nothing to change.
	if err := versioned.Unsafe(t.Context()).AutoMigrate(&User{}, &linkedIdentity{}, &auditRow{}); err != nil {
		t.Fatal(err)
	}
	if again := describeSchema(t, versioned, identityTables...); again != got {
		t.Fatalf("auto migrate changed the versioned schema:\n%s", again)
	}
}

func TestMigrations_VersionedUsesTheLedger(t *testing.T) {
	d := dbtest.OpenConfig(t, versionedConfig(t))
	idn := newIdentityOn(t, d, Config{Audit: AuditConfig{Enabled: true}})
	sts, err := d.MigrationStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, st := range sts {
		got = append(got, st.Owner+" "+st.Version+"_"+st.Name+" "+st.State)
	}
	want := []string{
		"identity 20261024000001_create_users_and_identities applied",
		"identity 20261024000002_create_audit_logs applied",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ledger:\n%s", strings.Join(got, "\n"))
	}
	// The tables work: an account and an audit record.
	c := serve(t, idn)
	register(t, c, "alice@test.com", "password123")
	if err := idn.Audit().LogSync(t.Context(), Event{Action: "x", Metadata: map[string]any{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrations_OffTouchesNothing(t *testing.T) {
	cfg := dbtest.Config(t)
	cfg.Migrate = "off"
	d := dbtest.OpenConfig(t, cfg)
	_, err := New(Config{SigningKey: testKey}, d)
	if !errors.Is(err, db.ErrNotMigrated) {
		t.Fatalf("off on a database without identity's tables: %v", err)
	}
	m := d.Unsafe(t.Context()).Migrator()
	for _, table := range identityTables {
		if m.HasTable(table) {
			t.Errorf("migrate: off created %s", table)
		}
	}
}

func TestMigrations_OffRunsOnAMigratedDatabase(t *testing.T) {
	cfg := versionedConfig(t)
	d := dbtest.OpenConfig(t, cfg)
	if _, err := New(Config{SigningKey: testKey}, d); err != nil {
		t.Fatal(err)
	}
	cfg.Migrate = "off"
	off := dbtest.OpenConfig(t, cfg)
	idn := newIdentityOn(t, off, Config{})
	register(t, serve(t, idn), "alice@test.com", "password123")
}

func TestMigrations_AutoCreatesTheTables(t *testing.T) {
	d := dbtest.Open(t)
	newIdentityOn(t, d, Config{})
	m := d.Unsafe(t.Context()).Migrator()
	for _, table := range identityTables {
		if !m.HasTable(table) {
			t.Errorf("no table %s", table)
		}
	}
	if !m.HasIndex("users", "uk_user_email") || !m.HasIndex("identities", "uk_identity_provider") {
		t.Fatal("the SoftUnique indexes")
	}
	// auto mode records nothing in the ledger: the tables follow the models.
	if sts, err := d.MigrationStatus(t.Context()); err != nil || len(sts) != 2 || sts[0].State != "pending" {
		t.Fatalf("status in auto mode: %+v %v", sts, err)
	}
	// Registering twice on one database is refused.
	if _, err := New(Config{SigningKey: testKey}, d); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("a second identity on the database: %v", err)
	}
}
