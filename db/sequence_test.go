package db

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
)

const ownedTestSequenceOwner = "example.com/choktest/sequence"

func ownedTestOptions() []SequenceOption {
	return []SequenceOption{SequenceOwner(ownedTestSequenceOwner)}
}

func ownedTestFS(sql string) fstest.MapFS {
	return fstest.MapFS{
		"sqlite/0001_init.sql":   &fstest.MapFile{Data: []byte(sql)},
		"mysql/0001_init.sql":    &fstest.MapFile{Data: []byte(sql)},
		"postgres/0001_init.sql": &fstest.MapFile{Data: []byte(sql)},
	}
}

func TestSequence_ApplicationAndOwnedLedgersAreIndependent(t *testing.T) {
	ctx := context.Background()
	h := openTestHandle(t)
	appFS := fstest.MapFS{
		"0001_app.sql": &fstest.MapFile{Data: []byte("CREATE TABLE app_sequence_item (id INTEGER PRIMARY KEY);")},
	}
	owned, err := OwnedSequence("widget", ownedTestFS("CREATE TABLE owned_sequence_item (id INTEGER PRIMARY KEY);"), Baseline{}, ownedTestOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyMigrations(ctx, h, appFS); err != nil {
		t.Fatal(err)
	}
	report, err := ApplySequence(ctx, h, owned)
	if err != nil {
		t.Fatal(err)
	}
	if report.Sequence != "widget" || report.Ledger != "schema_migrations_chok_widget" || report.Dialect != h.gdb.Dialector.Name() {
		t.Fatalf("owned identity = %+v", report)
	}
	appStatus, err := MigrationsStatus(ctx, h, appFS)
	if err != nil {
		t.Fatal(err)
	}
	ownedStatus, err := SequenceStatus(ctx, h, owned)
	if err != nil {
		t.Fatal(err)
	}
	if len(appStatus.Applied) != 1 || appStatus.Applied[0].Name != "app" {
		t.Fatalf("app status crossed ledgers: %+v", appStatus)
	}
	if len(ownedStatus.Applied) != 1 || ownedStatus.Applied[0].Name != "init" {
		t.Fatalf("owned status crossed ledgers: %+v", ownedStatus)
	}
	if !h.gdb.Migrator().HasTable(ledgerTable) || !h.gdb.Migrator().HasTable(owned.Ledger()) {
		t.Fatal("both ledgers must exist")
	}
}

func TestSequence_DirtyAndRepairStayInOwnedLedger(t *testing.T) {
	ctx := context.Background()
	h := openTestHandle(t)
	seq, err := OwnedSequence("broken", ownedTestFS("CREATE TABLE broken_owned (id INTEGER PRIMARY KEY); INVALID SQL;"), Baseline{}, ownedTestOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySequence(ctx, h, seq); err == nil {
		t.Fatal("broken owned migration must fail")
	}
	st, err := SequenceStatus(ctx, h, seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Dirty) != 1 || st.Dirty[0].Dialect != h.gdb.Dialector.Name() {
		t.Fatalf("owned dirty status = %+v", st)
	}
	if h.gdb.Migrator().HasTable(ledgerTable) {
		t.Fatal("owned sequence must not create the application ledger")
	}
	_, err = RepairSequence(ctx, h, seq, RepairOptions{
		Action: RepairRetry, Version: 1,
		ExpectedChecksum: st.Dirty[0].Checksum, Reason: "test restored partial DDL",
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err = SequenceStatus(ctx, h, seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Dirty) != 0 || len(st.Pending) != 1 {
		t.Fatalf("owned repair result = %+v", st)
	}
}

func TestOwnedSequence_RejectsDialectSetDrift(t *testing.T) {
	fsys := ownedTestFS("SELECT 1;")
	delete(fsys, "mysql/0001_init.sql")
	fsys["mysql/0002_other.sql"] = &fstest.MapFile{Data: []byte("SELECT 1;")}
	_, err := OwnedSequence("drift", fsys, Baseline{}, ownedTestOptions()...)
	if err == nil || !strings.Contains(err.Error(), "dialect set mismatch") {
		t.Fatalf("want dialect set mismatch, got %v", err)
	}
}

func TestSameBaseline_RequiresEquivalentVersionAndFingerprints(t *testing.T) {
	base := Baseline{
		EquivalentVersion: 1,
		Tables:            []string{"one", "two"},
		Fingerprints:      map[string]string{"sqlite": "a", "mysql": "b", "postgres": "c"},
	}
	if !sameBaseline(base, cloneBaseline(base)) {
		t.Fatal("cloned baseline must match")
	}
	changedVersion := cloneBaseline(base)
	changedVersion.EquivalentVersion = 2
	if sameBaseline(base, changedVersion) {
		t.Fatal("equivalent version is registration identity")
	}
	changedFingerprint := cloneBaseline(base)
	changedFingerprint.Fingerprints["postgres"] = "different"
	if sameBaseline(base, changedFingerprint) {
		t.Fatal("fingerprints are registration identity")
	}
}

func TestSequence_DialectMismatchFailsStatusAndRepair(t *testing.T) {
	h := openTestHandle(t)
	seq, err := OwnedSequence("dialect_guard", ownedTestFS("CREATE TABLE dialect_guard_item (id INTEGER PRIMARY KEY);"), Baseline{}, ownedTestOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySequence(t.Context(), h, seq); err != nil {
		t.Fatal(err)
	}
	if err := h.gdb.Exec("UPDATE " + seq.Ledger() + " SET dialect = 'mysql' WHERE version = 1").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := SequenceStatus(t.Context(), h, seq); err == nil || !strings.Contains(err.Error(), "dialect mismatch") {
		t.Fatalf("status must reject cross-dialect ledger, got %v", err)
	}
	files, err := LoadMigrations(fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte("CREATE TABLE dialect_guard_item (id INTEGER PRIMARY KEY);")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RepairSequence(t.Context(), h, seq, RepairOptions{
		Action: RepairAcceptDrift, Version: 1, ExpectedChecksum: files[0].Checksum,
		Reason: "must not cross dialects",
	}); err == nil || !strings.Contains(err.Error(), "dialect mismatch") {
		t.Fatalf("repair must reject cross-dialect ledger, got %v", err)
	}
}

func TestSequence_LedgerAwareOlderBinaryRejectsNewerLedger(t *testing.T) {
	h := openTestHandle(t)
	newerFS := fstest.MapFS{}
	olderFS := fstest.MapFS{}
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		first := &fstest.MapFile{Data: []byte("CREATE TABLE ledger_aware_item (id INTEGER PRIMARY KEY);")}
		newerFS[dialect+"/0001_init.sql"] = first
		olderFS[dialect+"/0001_init.sql"] = first
		newerFS[dialect+"/0002_new_shape.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE ledger_aware_new_shape (id INTEGER PRIMARY KEY);")}
	}
	newer, err := OwnedSequence("ledger_aware", newerFS, Baseline{}, ownedTestOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	older, err := OwnedSequence("ledger_aware", olderFS, Baseline{}, ownedTestOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySequence(t.Context(), h, newer); err != nil {
		t.Fatal(err)
	}
	status, err := SequenceStatus(t.Context(), h, older)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Missing) != 1 || status.Missing[0].Version != 2 {
		t.Fatalf("older sequence must expose newer ledger row as missing: %+v", status)
	}
	if _, err := ApplySequence(t.Context(), h, older); err == nil || !strings.Contains(err.Error(), "no matching migration file") {
		t.Fatalf("older sequence must fail closed against newer ledger, got %v", err)
	}
}
