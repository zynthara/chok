package authz_test

import (
	"context"
	"testing"

	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/authz/casbin"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
)

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
	if err := autoDB.Unsafe(ctx).AutoMigrate(&casbin.CasbinRule{}); err != nil {
		t.Fatal(err)
	}
	want, err := db.SchemaFingerprint(ctx, autoDB, []string{"casbin_rule"})
	if err != nil {
		t.Fatal(err)
	}
	report, err := db.ApplySequence(ctx, autoDB, authz.MigrationSequence())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Adopted) != 1 || report.Adopted[0].Provenance != "baseline" {
		t.Fatalf("baseline report = %+v", report)
	}
	freshDB := open()
	if _, err := db.ApplySequence(ctx, freshDB, authz.MigrationSequence()); err != nil {
		t.Fatal(err)
	}
	got, err := db.SchemaFingerprint(ctx, freshDB, []string{"casbin_rule"})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("versioned authz schema differs from AutoMigrate schema")
	}
}

func TestMigrationSequence_PostgresSchemaEquivalent(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("postgres lane only")
	}
	assertAuthzSchemaEquivalent(t, dbtest.Open)
}

func TestMigrationSequence_MySQLSchemaEquivalent(t *testing.T) {
	assertAuthzSchemaEquivalent(t, dbtest.OpenMySQL)
}

func assertAuthzSchemaEquivalent(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()
	ctx := context.Background()
	autoDB := open(t)
	if err := autoDB.Unsafe(ctx).AutoMigrate(&casbin.CasbinRule{}); err != nil {
		t.Fatal(err)
	}
	want, err := db.SchemaFingerprint(ctx, autoDB, []string{"casbin_rule"})
	if err != nil {
		t.Fatal(err)
	}
	freshDB := open(t)
	if _, err := db.ApplySequence(ctx, freshDB, authz.MigrationSequence()); err != nil {
		t.Fatal(err)
	}
	got, err := db.SchemaFingerprint(ctx, freshDB, []string{"casbin_rule"})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("versioned authz schema differs from AutoMigrate\nwant=%s\ngot=%s", want, got)
	}
}
