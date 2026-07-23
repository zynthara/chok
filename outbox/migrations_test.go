package outbox_test

import (
	"context"
	"testing"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/internal/testschema"
	"github.com/zynthara/chok/v2/outbox"
)

var outboxTables = []string{"outbox_messages", "outbox_relay_state"}

func TestMigrationSequence_AutoBaselineAndFreshSchemaEquivalent(t *testing.T) {
	ctx := context.Background()
	open := func() *db.DB {
		h, err := db.Open(db.Options{Driver: "sqlite", SQLite: db.SQLiteOptions{Path: ":memory:"}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
		return h
	}
	autoDB := open()
	if err := outbox.MigrateSchema(ctx, autoDB); err != nil {
		t.Fatal(err)
	}
	want, err := db.SchemaFingerprint(ctx, autoDB, outboxTables)
	if err != nil {
		t.Fatal(err)
	}
	testschema.UpdateBaselineIfRequested(t, want)
	report, err := db.ApplySequence(ctx, autoDB, outbox.MigrationSequence())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Adopted) != 1 || report.Adopted[0].Provenance != "baseline" {
		t.Fatalf("baseline report = %+v", report)
	}
	freshDB := open()
	if _, err := db.ApplySequence(ctx, freshDB, outbox.MigrationSequence()); err != nil {
		t.Fatal(err)
	}
	got, err := db.SchemaFingerprint(ctx, freshDB, outboxTables)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("versioned outbox schema differs from MigrateSchema\nwant=%s\ngot=%s", want, got)
	}
}

func TestMigrationSequence_PostgresSchemaEquivalent(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("postgres lane only")
	}
	assertOutboxSchemaEquivalent(t, dbtest.Open, "postgres")
}

func TestMigrationSequence_MySQLSchemaEquivalent(t *testing.T) {
	assertOutboxSchemaEquivalent(t, dbtest.OpenMySQL, "mysql")
}

func assertOutboxSchemaEquivalent(t *testing.T, open func(testing.TB) *db.DB, dialect string) {
	t.Helper()
	ctx := context.Background()
	autoDB := open(t)
	if err := outbox.MigrateSchema(ctx, autoDB); err != nil {
		t.Fatal(err)
	}
	want, err := db.SchemaFingerprint(ctx, autoDB, outboxTables)
	if err != nil {
		t.Fatal(err)
	}
	testschema.UpdateBaselineIfRequested(t, want)
	report, err := db.ApplySequence(ctx, autoDB, outbox.MigrationSequence())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Adopted) != 1 || len(report.Applied) != 0 || report.Dialect != dialect {
		t.Fatalf("baseline adoption on %s = %+v", dialect, report)
	}
	if adopted := report.Adopted[0]; adopted.Provenance != "baseline" || adopted.Dialect != dialect {
		t.Fatalf("adopted row on %s = %+v", dialect, adopted)
	}
	freshDB := open(t)
	if _, err := db.ApplySequence(ctx, freshDB, outbox.MigrationSequence()); err != nil {
		t.Fatal(err)
	}
	got, err := db.SchemaFingerprint(ctx, freshDB, outboxTables)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("versioned outbox schema differs from MigrateSchema\nwant=%s\ngot=%s", want, got)
	}
}
