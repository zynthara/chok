package account_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/account"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/internal/testschema"
)

func openAccountMigrationDB(t *testing.T) *db.DB {
	t.Helper()
	h, err := db.Open(db.Options{Driver: "sqlite", SQLite: db.SQLiteOptions{Path: ":memory:"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestMigrationSequence_AutoBaselineAndFreshSchemaEquivalent(t *testing.T) {
	ctx := context.Background()
	autoDB := openAccountMigrationDB(t)
	if err := account.MigrateSchema(ctx, autoDB); err != nil {
		t.Fatal(err)
	}
	autoFingerprint, err := db.SchemaFingerprint(ctx, autoDB, []string{"users", "identities"})
	if err != nil {
		t.Fatal(err)
	}
	testschema.UpdateBaselineIfRequested(t, autoFingerprint)
	report, err := db.ApplySequence(ctx, autoDB, account.MigrationSequence())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Adopted) != 2 || report.Adopted[0].Provenance != "baseline" || len(report.Applied) != 0 {
		t.Fatalf("baseline report = %+v", report)
	}

	freshDB := openAccountMigrationDB(t)
	report, err = db.ApplySequence(ctx, freshDB, account.MigrationSequence())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Applied) != 2 || len(report.Adopted) != 0 {
		t.Fatalf("fresh report = %+v", report)
	}
	freshFingerprint, err := db.SchemaFingerprint(ctx, freshDB, []string{"users", "identities"})
	if err != nil {
		t.Fatal(err)
	}
	if freshFingerprint != autoFingerprint {
		t.Fatal("versioned account schema differs from AutoMigrate schema")
	}
}

func TestMigrationSequence_PartialBaselineFailsClosed(t *testing.T) {
	h := openAccountMigrationDB(t)
	if err := h.Migrate(t.Context(), account.Table()); err != nil {
		t.Fatal(err)
	}
	_, err := db.ApplySequence(t.Context(), h, account.MigrationSequence())
	if err == nil || !strings.Contains(err.Error(), "partially present") || !strings.Contains(err.Error(), "identities") {
		t.Fatalf("want structured partial-baseline failure, got %v", err)
	}
}

func TestMigrationSequence_FingerprintMismatchWritesNoAdoptionRows(t *testing.T) {
	h := openAccountMigrationDB(t)
	if err := account.MigrateSchema(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if err := h.Unsafe(t.Context()).Exec("ALTER TABLE users ADD COLUMN foreign_shape TEXT").Error; err != nil {
		t.Fatal(err)
	}
	_, err := db.ApplySequence(t.Context(), h, account.MigrationSequence())
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("want fingerprint mismatch, got %v", err)
	}
	var rows int64
	if err := h.Unsafe(t.Context()).Raw("SELECT COUNT(*) FROM schema_migrations_chok_account WHERE version > 0").Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("failed baseline adoption wrote %d rows", rows)
	}
}

func TestMigrationSequence_TransitionReleaseCoexistsWithLegacyAutoMigrate(t *testing.T) {
	h := openAccountMigrationDB(t)
	if _, err := db.ApplySequence(t.Context(), h, account.MigrationSequence()); err != nil {
		t.Fatal(err)
	}
	before, err := db.SchemaFingerprint(t.Context(), h, []string{"users", "identities"})
	if err != nil {
		t.Fatal(err)
	}
	if err := account.MigrateSchema(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	after, err := db.SchemaFingerprint(t.Context(), h, []string{"users", "identities"})
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("legacy AutoMigrate drifted the transition release schema")
	}
	st, err := db.SequenceStatus(t.Context(), h, account.MigrationSequence())
	if err != nil || !st.Clean() {
		t.Fatalf("transition ledger after legacy AutoMigrate = %+v err=%v", st, err)
	}
}

func TestMigrationSequence_PostgresSchemaEquivalent(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("postgres lane only")
	}
	assertAccountSchemaEquivalent(t, dbtest.Open)
}

func TestMigrationSequence_MySQLSchemaEquivalent(t *testing.T) {
	assertAccountSchemaEquivalent(t, dbtest.OpenMySQL)
}

func assertAccountSchemaEquivalent(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()
	ctx := context.Background()
	autoDB := open(t)
	if err := account.MigrateSchema(ctx, autoDB); err != nil {
		t.Fatal(err)
	}
	want, err := db.SchemaFingerprint(ctx, autoDB, []string{"users", "identities"})
	if err != nil {
		t.Fatal(err)
	}
	testschema.UpdateBaselineIfRequested(t, want)
	report, err := db.ApplySequence(ctx, autoDB, account.MigrationSequence())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Adopted) != 2 {
		t.Fatalf("baseline adoption on the real dialect = %+v", report)
	}
	freshDB := open(t)
	if _, err := db.ApplySequence(ctx, freshDB, account.MigrationSequence()); err != nil {
		t.Fatal(err)
	}
	got, err := db.SchemaFingerprint(ctx, freshDB, []string{"users", "identities"})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("versioned account schema differs from AutoMigrate\nwant=%s\ngot=%s", want, got)
	}
}
