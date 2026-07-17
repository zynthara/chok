package db

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func ownedManifestTestFS(files map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		for name, statement := range files {
			out[dialect+"/"+name] = &fstest.MapFile{Data: []byte(statement)}
		}
	}
	return out
}

func manifestTestSequence(t *testing.T, kind, owner string, files map[string]string, options ...SequenceOption) Sequence {
	t.Helper()
	opts := []SequenceOption{SequenceOwner(owner)}
	opts = append(opts, options...)
	seq, err := OwnedSequence(kind, ownedManifestTestFS(files), Baseline{}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestOwnedSequence_RequiresStableValidatedOwnerAndReservedMapping(t *testing.T) {
	fsys := ownedManifestTestFS(map[string]string{"0001_init.sql": "SELECT 1;"})
	if _, err := OwnedSequence("widget", fsys, Baseline{}); err == nil || !strings.Contains(err.Error(), "SequenceOwner") {
		t.Fatalf("missing owner must fail, got %v", err)
	}
	if _, err := OwnedSequence("manifest", fsys, Baseline{}, SequenceOwner(ownedTestSequenceOwner)); err == nil {
		t.Fatal("manifest kind must be forbidden for every owner")
	}
	if _, err := OwnedSequence("account", fsys, Baseline{}, SequenceOwner(ownedTestSequenceOwner)); err == nil || !strings.Contains(err.Error(), chokAccountSequenceOwner) {
		t.Fatalf("reserved account owner must fail with expected identity, got %v", err)
	}
	if _, err := OwnedSequence("widget", fsys, Baseline{}, SequenceOwner(" example.com/widget")); err == nil {
		t.Fatal("owner with surrounding whitespace must fail")
	}
	if _, err := OwnedSequence("widget", fsys, Baseline{}, SequenceOwner(strings.Repeat("a", maxSequenceOwnerBytes+1))); err == nil {
		t.Fatal("overlong owner must fail before database access")
	}
	if _, err := OwnedSequence("widget", fsys, Baseline{},
		SequenceOwner("example.com/widget"), SequenceVersion(strings.Repeat("v", maxSequenceVersionBytes+1))); err == nil {
		t.Fatal("overlong component version must fail before database access")
	}
	seq, err := OwnedSequence("widget", fsys, Baseline{},
		SequenceOwner("example.com/widget/migrations"), SequenceVersion("v1.4.0"))
	if err != nil {
		t.Fatal(err)
	}
	if seq.Owner() != "example.com/widget/migrations" || seq.ComponentVersion() != "v1.4.0" {
		t.Fatalf("sequence metadata = owner=%q version=%q", seq.Owner(), seq.ComponentVersion())
	}
}

func TestRegisterOwnedSequence_OwnerAndVersionAreDescriptorIdentity(t *testing.T) {
	h := openTestHandle(t)
	component := &Component{}
	files := map[string]string{"0001_init.sql": "SELECT 1;"}
	first := manifestTestSequence(t, "registry_identity", "example.com/one/registry", files, SequenceVersion("v1"))
	if err := component.registerOwnedSequence(h, first); err != nil {
		t.Fatal(err)
	}
	otherOwner := manifestTestSequence(t, "registry_identity", "example.com/two/registry", files, SequenceVersion("v1"))
	if err := component.registerOwnedSequence(h, otherOwner); err == nil || !strings.Contains(err.Error(), "conflicting metadata") {
		t.Fatalf("same bytes under another owner must fail in-process, got %v", err)
	}
	otherVersion := manifestTestSequence(t, "registry_identity", first.Owner(), files, SequenceVersion("v2"))
	if err := component.registerOwnedSequence(h, otherVersion); err == nil || !strings.Contains(err.Error(), "conflicting metadata") {
		t.Fatalf("same owner with conflicting declared version must fail in-process, got %v", err)
	}
}

func TestSequenceManifest_FirstClaimReentryAndConflict(t *testing.T) {
	h := openTestHandle(t)
	files := map[string]string{"0001_init.sql": "CREATE TABLE manifest_claim_item (id BIGINT PRIMARY KEY);"}
	first := manifestTestSequence(t, "claim_test", "example.com/one/claim", files, SequenceVersion("v1.0.0"))
	if _, err := ApplySequence(t.Context(), h, first); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySequence(t.Context(), h, first); err != nil {
		t.Fatalf("same owner reentry must be idempotent: %v", err)
	}
	entries, err := ManifestEntries(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != "claim_test" || entries[0].Owner != first.Owner() || entries[0].Provenance != "claimed" || entries[0].ComponentVersion != "v1.0.0" {
		t.Fatalf("manifest entries = %+v", entries)
	}
	snapshot, err := LedgerSnapshot(t.Context(), h, "claim_test")
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Exists || snapshot.Frontier != 1 || snapshot.Rows != 1 || snapshot.Dirty != 0 {
		t.Fatalf("ledger snapshot = %+v", snapshot)
	}

	other := manifestTestSequence(t, "claim_test", "example.com/other/claim", files)
	if _, err := ApplySequence(t.Context(), h, other); !errors.Is(err, ErrSequenceClaimConflict) {
		t.Fatalf("different owner must fail with claim conflict, got %v", err)
	}
}

func TestSequenceManifest_ConcurrentFirstClaimConverges(t *testing.T) {
	h := openTestHandle(t)
	seq := manifestTestSequence(t, "claim_concurrent", "example.com/concurrent/claim", map[string]string{
		"0001_init.sql": "CREATE TABLE manifest_concurrent_item (id BIGINT PRIMARY KEY);",
	})
	// Four workers instead of two: the historical failure mode was the
	// pre-lock CREATE TABLE IF NOT EXISTS colliding on PostgreSQL's catalog
	// uniques, and more concurrent first claims widen the window enough to
	// reproduce on slower CI runners too.
	const workers = 4
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ApplySequence(t.Context(), h, seq)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent same-owner first claim must converge: %v", err)
		}
	}
	entries, err := ManifestEntries(t.Context(), h)
	if err != nil || len(entries) != 1 || entries[0].Kind != seq.Kind() || entries[0].Owner != seq.Owner() {
		t.Fatalf("concurrent first claim entries=%+v err=%v", entries, err)
	}
}

// TestPreflightSequenceClaim_StateMatrix pins the pre-lock probe behind
// apply and repair on every reachable static state. The converged state
// (claim and ledger both present) is the footprint a concurrent first
// claim leaves mid-probe; the probe's claim-first-ledger-second read order
// must report it as existing, never as the claim-without-ledger corruption.
func TestPreflightSequenceClaim_StateMatrix(t *testing.T) {
	h := openTestHandle(t)
	files := map[string]string{"0001_init.sql": "CREATE TABLE preflight_claim_item (id BIGINT PRIMARY KEY);"}
	seq := manifestTestSequence(t, "preflight_claim", "example.com/preflight/claim", files)
	e, err := resolveOwnedSequence(h, seq)
	if err != nil {
		t.Fatal(err)
	}
	gdb := h.gdb.WithContext(t.Context())

	existed, err := e.preflightSequenceClaim(gdb)
	if err != nil || existed {
		t.Fatalf("fresh database preflight = (%v, %v), want (false, nil)", existed, err)
	}

	if _, err := ApplySequence(t.Context(), h, seq); err != nil {
		t.Fatal(err)
	}
	existed, err = e.preflightSequenceClaim(gdb)
	if err != nil || !existed {
		t.Fatalf("converged claim preflight = (%v, %v), want (true, nil)", existed, err)
	}

	if err := h.gdb.Migrator().DropTable(seq.Ledger()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.preflightSequenceClaim(gdb); !errors.Is(err, ErrSequenceManifestCorrupt) {
		t.Fatalf("claim without ledger preflight = %v, want ErrSequenceManifestCorrupt", err)
	}
}

func TestSequenceManifest_IsScopedPerDatabase(t *testing.T) {
	firstDB := openTestHandle(t)
	secondDB := openTestHandle(t)
	files := map[string]string{"0001_init.sql": "CREATE TABLE manifest_scoped_item (id BIGINT PRIMARY KEY);"}
	first := manifestTestSequence(t, "scoped_claim", "example.com/one/scoped", files)
	second := manifestTestSequence(t, "scoped_claim", "example.com/two/scoped", files)
	if _, err := ApplySequence(t.Context(), firstDB, first); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySequence(t.Context(), secondDB, second); err != nil {
		t.Fatalf("the same kind in another database must have an independent claim: %v", err)
	}
	firstEntries, err := ManifestEntries(t.Context(), firstDB)
	if err != nil {
		t.Fatal(err)
	}
	secondEntries, err := ManifestEntries(t.Context(), secondDB)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstEntries) != 1 || firstEntries[0].Owner != first.Owner() ||
		len(secondEntries) != 1 || secondEntries[0].Owner != second.Owner() {
		t.Fatalf("database-scoped claims: first=%+v second=%+v", firstEntries, secondEntries)
	}
}

func TestSequenceManifest_ReadAPIsWorkOnReadOnlyHandle(t *testing.T) {
	path := t.TempDir() + "/manifest-readonly.db"
	writable, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	seq := manifestTestSequence(t, "readonly_claim", "example.com/readonly/claim", map[string]string{
		"0001_init.sql": "CREATE TABLE manifest_readonly_item (id BIGINT PRIMARY KEY);",
	})
	if _, err := ApplySequence(t.Context(), writable, seq); err != nil {
		_ = writable.Close()
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := Open(Options{Driver: "sqlite", ReadOnly: true, SQLite: SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	entries, err := ManifestEntries(t.Context(), readOnly)
	if err != nil || len(entries) != 1 || entries[0].Owner != seq.Owner() {
		t.Fatalf("read-only manifest entries=%+v err=%v", entries, err)
	}
	snapshot, err := LedgerSnapshot(t.Context(), readOnly, seq.Kind())
	if err != nil || !snapshot.Exists || snapshot.Frontier != 1 || snapshot.Dirty != 0 {
		t.Fatalf("read-only ledger snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSequenceManifest_AdoptionPersistsBeforePendingMigration(t *testing.T) {
	h := openTestHandle(t)
	owner := "example.com/legacy/component"
	initial := manifestTestSequence(t, "legacy_adopt", owner, map[string]string{
		"0001_init.sql": "CREATE TABLE manifest_legacy_item (id BIGINT PRIMARY KEY);",
	})
	if _, err := ApplySequence(t.Context(), h, initial); err != nil {
		t.Fatal(err)
	}
	if err := h.gdb.Migrator().DropTable(sequenceManifestTable); err != nil {
		t.Fatal(err)
	}

	newer := manifestTestSequence(t, "legacy_adopt", owner, map[string]string{
		"0001_init.sql":  "CREATE TABLE manifest_legacy_item (id BIGINT PRIMARY KEY);",
		"0002_break.sql": "INVALID SQL;",
	})
	if _, err := ApplySequence(t.Context(), h, newer); err == nil {
		t.Fatal("pending invalid migration must fail")
	}
	entries, err := ManifestEntries(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Owner != owner || entries[0].Provenance != "adopted" {
		t.Fatalf("owner must persist before pending SQL starts: %+v", entries)
	}
	other := manifestTestSequence(t, "legacy_adopt", "example.com/other/component", map[string]string{
		"0001_init.sql":  "CREATE TABLE manifest_legacy_item (id BIGINT PRIMARY KEY);",
		"0002_break.sql": "INVALID SQL;",
	})
	if _, err := ApplySequence(t.Context(), h, other); !errors.Is(err, ErrSequenceClaimConflict) {
		t.Fatalf("failed first upgrade must remain claimed against another owner, got %v", err)
	}
}

func TestSequenceManifest_RepairAndFloorShareAuthorization(t *testing.T) {
	h := openTestHandle(t)
	files := map[string]string{"0001_break.sql": "INVALID SQL;"}
	owner := manifestTestSequence(t, "repair_guard", "example.com/one/repair", files)
	if _, err := ApplySequence(t.Context(), h, owner); err == nil {
		t.Fatal("broken migration must create a dirty row")
	}
	status, err := SequenceStatus(t.Context(), h, owner)
	if err != nil {
		t.Fatal(err)
	}
	other := manifestTestSequence(t, "repair_guard", "example.com/two/repair", files)
	_, err = RepairSequence(t.Context(), h, other, RepairOptions{
		Action: RepairRetry, Version: 1, ExpectedChecksum: status.Dirty[0].Checksum, Reason: "must respect owner",
	})
	if !errors.Is(err, ErrSequenceClaimConflict) {
		t.Fatalf("repair by another owner must fail, got %v", err)
	}
	if err := h.gdb.Exec("UPDATE "+sequenceManifestTable+" SET engine_floor = ? WHERE kind = ?", MigrationEngineGeneration+1, owner.Kind()).Error; err != nil {
		t.Fatal(err)
	}
	entries, err := ManifestEntries(t.Context(), h)
	if err != nil || len(entries) != 1 || entries[0].EngineCompatible() {
		t.Fatalf("status APIs must expose, not reject, a higher engine floor: entries=%+v err=%v", entries, err)
	}
	if _, err := ApplySequence(t.Context(), h, owner); !errors.Is(err, ErrMigrationEngineTooOld) {
		t.Fatalf("apply must reject higher engine floor, got %v", err)
	}
	if _, err := RepairSequence(t.Context(), h, owner, RepairOptions{
		Action: RepairRetry, Version: 1, ExpectedChecksum: status.Dirty[0].Checksum, Reason: "must respect floor",
	}); !errors.Is(err, ErrMigrationEngineTooOld) {
		t.Fatalf("repair must reject higher engine floor, got %v", err)
	}
	if _, err := RepairSequenceClaim(t.Context(), h, owner.Kind(), RepairClaimOptions{
		ExpectedOwner: owner.Owner(), NewOwner: "example.com/new/repair", Reason: "test transfer",
	}); !errors.Is(err, ErrMigrationEngineTooOld) {
		t.Fatalf("claim transfer must reject higher engine floor, got %v", err)
	}
}

func TestRepairSequenceClaim_CASAndMissingLedgerFailClosed(t *testing.T) {
	h := openTestHandle(t)
	files := map[string]string{"0001_init.sql": "CREATE TABLE manifest_transfer_item (id BIGINT PRIMARY KEY);"}
	oldOwner := "example.com/old/transfer"
	newOwner := "example.com/new/transfer"
	seq := manifestTestSequence(t, "claim_transfer", oldOwner, files)
	if _, err := ApplySequence(t.Context(), h, seq); err != nil {
		t.Fatal(err)
	}
	if _, err := RepairSequenceClaim(t.Context(), h, seq.Kind(), RepairClaimOptions{
		ExpectedOwner: "example.com/wrong/transfer", NewOwner: newOwner, Reason: "test transfer",
	}); err == nil || !strings.Contains(err.Error(), "expected owner") {
		t.Fatalf("claim transfer must enforce the expected-owner CAS, got %v", err)
	}
	report, err := RepairSequenceClaim(t.Context(), h, seq.Kind(), RepairClaimOptions{
		ExpectedOwner: oldOwner, NewOwner: newOwner, Reason: "test transfer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.PreviousOwner != oldOwner || report.NewOwner != newOwner {
		t.Fatalf("claim repair report = %+v", report)
	}
	if _, err := ApplySequence(t.Context(), h, seq); !errors.Is(err, ErrSequenceClaimConflict) {
		t.Fatalf("previous owner must fail after transfer, got %v", err)
	}
	newSeq := manifestTestSequence(t, seq.Kind(), newOwner, files)
	if _, err := ApplySequence(t.Context(), h, newSeq); err != nil {
		t.Fatalf("new owner must apply after transfer: %v", err)
	}
	if err := h.gdb.Migrator().DropTable(seq.Ledger()); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySequence(t.Context(), h, newSeq); !errors.Is(err, ErrSequenceManifestCorrupt) {
		t.Fatalf("apply with a claim but no ledger must fail closed, got %v", err)
	}
	if h.gdb.Migrator().HasTable(seq.Ledger()) {
		t.Fatal("apply must not recreate a missing owned ledger")
	}
	if _, err := RepairSequence(t.Context(), h, newSeq, RepairOptions{
		Action: RepairRetry, Version: 1, ExpectedChecksum: strings.Repeat("0", 64), Reason: "missing ledger guard",
	}); !errors.Is(err, ErrSequenceManifestCorrupt) {
		t.Fatalf("ledger repair with a claim but no ledger must fail closed, got %v", err)
	}
	if h.gdb.Migrator().HasTable(seq.Ledger()) {
		t.Fatal("ledger repair must not recreate a missing owned ledger")
	}
	if _, err := RepairSequenceClaim(t.Context(), h, seq.Kind(), RepairClaimOptions{
		ExpectedOwner: newOwner, NewOwner: oldOwner, Reason: "test transfer",
	}); !errors.Is(err, ErrSequenceManifestCorrupt) {
		t.Fatalf("claim without ledger must fail closed, got %v", err)
	}
	if h.gdb.Migrator().HasTable(seq.Ledger()) {
		t.Fatal("claim repair must not recreate a missing SQLite ledger")
	}
}

func TestRepairSequenceClaim_CanRestoreReservedOwner(t *testing.T) {
	h := openTestHandle(t)
	e := migrationEngine{seq: migrationSequence{
		kind:    "account",
		ledger:  ledgerForSequenceKind("account"),
		dialect: h.gdb.Dialector.Name(),
	}}
	if err := e.ensureLedgerBase(h.gdb); err != nil {
		t.Fatal(err)
	}
	if err := ensureManifestBase(h.gdb); err != nil {
		t.Fatal(err)
	}
	wrongOwner := "example.com/fork/account"
	if err := h.gdb.Exec(
		"INSERT INTO "+sequenceManifestTable+" (kind, ledger, owner, engine_floor, provenance, claimed_at) VALUES (?, ?, ?, ?, ?, ?)",
		e.seq.kind, e.seq.ledger, wrongOwner, MigrationEngineGeneration, "claimed", time.Now().UTC(),
	).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ManifestEntries(t.Context(), h); !errors.Is(err, ErrSequenceManifestCorrupt) {
		t.Fatalf("ordinary manifest reads must reject a corrupted reserved owner, got %v", err)
	}
	report, err := RepairSequenceClaim(t.Context(), h, e.seq.kind, RepairClaimOptions{
		ExpectedOwner: wrongOwner,
		NewOwner:      chokAccountSequenceOwner, Reason: "test transfer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.PreviousOwner != wrongOwner || report.NewOwner != chokAccountSequenceOwner {
		t.Fatalf("reserved claim repair report = %+v", report)
	}
	entries, err := ManifestEntries(t.Context(), h)
	if err != nil || len(entries) != 1 || entries[0].Owner != chokAccountSequenceOwner {
		t.Fatalf("reserved owner was not restored: entries=%+v err=%v", entries, err)
	}
}

func TestRepairSequenceClaim_WaitsForTheMigrationLock(t *testing.T) {
	h := openTestHandle(t)
	seq := manifestTestSequence(t, "claim_lock", "example.com/old/lock", map[string]string{
		"0001_init.sql": "CREATE TABLE manifest_claim_lock_item (id BIGINT PRIMARY KEY);",
	})
	if _, err := ApplySequence(t.Context(), h, seq); err != nil {
		t.Fatal(err)
	}
	e, err := resolveOwnedSequence(h, seq)
	if err != nil {
		t.Fatal(err)
	}
	held, err := e.acquireMigrationLock(t.Context(), h.gdb.WithContext(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Millisecond)
	defer cancel()
	_, repairErr := RepairSequenceClaim(ctx, h, seq.Kind(), RepairClaimOptions{
		ExpectedOwner: seq.Owner(), NewOwner: "example.com/new/lock", Reason: "test transfer",
	})
	held.release()
	if repairErr == nil || (!errors.Is(repairErr, context.DeadlineExceeded) && !strings.Contains(repairErr.Error(), "deadline") && !strings.Contains(repairErr.Error(), "lease")) {
		t.Fatalf("claim transfer must wait for the active migration lock, got %v", repairErr)
	}
	entries, err := ManifestEntries(t.Context(), h)
	if err != nil || len(entries) != 1 || entries[0].Owner != seq.Owner() {
		t.Fatalf("timed-out claim transfer changed owner: entries=%+v err=%v", entries, err)
	}
}

func TestManifestTrustBoundaryAndAdditiveUpgrade(t *testing.T) {
	h := openTestHandle(t)
	if _, err := RepairSequenceClaim(t.Context(), h, "unclaimed_kind", RepairClaimOptions{
		ExpectedOwner: "example.com/old/unclaimed", NewOwner: "example.com/new/unclaimed", Reason: "test transfer",
	}); !errors.Is(err, ErrSequenceUnclaimed) {
		t.Fatalf("claim repair must not create a first claim, got %v", err)
	}
	if _, err := LedgerSnapshot(t.Context(), h, `bad";drop_table`); err == nil {
		t.Fatal("unsafe kind must be rejected before SQL derivation")
	}
	if _, err := LedgerSnapshot(t.Context(), h, sequenceManifestKind); err == nil {
		t.Fatal("manifest kind must be rejected")
	}
	if _, err := RepairSequenceClaim(t.Context(), h, sequenceManifestKind, RepairClaimOptions{
		ExpectedOwner: "example.com/old/manifest", NewOwner: "example.com/new/manifest", Reason: "test transfer",
	}); err == nil {
		t.Fatal("claim repair must reject the manifest kind")
	}
	if err := h.gdb.Exec(
		"CREATE TABLE " + sequenceManifestTable + " (kind VARCHAR(31) PRIMARY KEY, ledger VARCHAR(64) NOT NULL, owner VARCHAR(190) NOT NULL)",
	).Error; err != nil {
		t.Fatal(err)
	}
	first := manifestTestSequence(t, "upgrade_one", "example.com/upgrade/one", map[string]string{
		"0001_init.sql": "CREATE TABLE manifest_upgrade_one (id BIGINT PRIMARY KEY);",
	})
	second := manifestTestSequence(t, "upgrade_two", "example.com/upgrade/two", map[string]string{
		"0001_init.sql": "CREATE TABLE manifest_upgrade_two (id BIGINT PRIMARY KEY);",
	})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, seq := range []Sequence{first, second} {
		seq := seq
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ApplySequence(t.Context(), h, seq)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent different-kind manifest upgrade: %v", err)
		}
	}
	entries, err := ManifestEntries(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("upgraded manifest entries = %+v", entries)
	}
	if err := h.gdb.Exec("UPDATE "+sequenceManifestTable+" SET ledger = 'schema_migrations_chok_other' WHERE kind = ?", first.Kind()).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ManifestEntries(t.Context(), h); !errors.Is(err, ErrSequenceManifestCorrupt) {
		t.Fatalf("tampered manifest ledger must fail closed, got %v", err)
	}
	if _, err := LedgerSnapshot(t.Context(), h, first.Kind()); !errors.Is(err, ErrSequenceManifestCorrupt) {
		t.Fatalf("ledger snapshot must also reject a tampered manifest ledger, got %v", err)
	}
}

func TestSanitizeSequenceVersion_NeverPersistsWhatReadsReject(t *testing.T) {
	// A multibyte rune straddling the byte limit must not be split into
	// invalid UTF-8 that turns every later manifest read into corruption.
	overlong := strings.Repeat("v", maxSequenceVersionBytes-1) + "界"
	if len(overlong) <= maxSequenceVersionBytes {
		t.Fatalf("fixture must exceed %d bytes, got %d", maxSequenceVersionBytes, len(overlong))
	}
	cases := []string{
		overlong,
		strings.Repeat("v", maxSequenceVersionBytes+7),
		"v1.2.3\x00broken",
		" padded ",
		"(devel)",
		"",
	}
	for _, input := range cases {
		got := sanitizeSequenceVersion(input)
		if err := validateSequenceVersion(got); err != nil {
			t.Fatalf("sanitizeSequenceVersion(%q) = %q, still rejected by the read path: %v", input, got, err)
		}
	}
	if got := sanitizeSequenceVersion("(devel)"); got != "(devel)" {
		t.Fatalf("valid version must pass through unchanged, got %q", got)
	}
}

func TestValidateSequenceKind_ExportedGate(t *testing.T) {
	if err := ValidateSequenceKind("billing"); err != nil {
		t.Fatalf("legal kind rejected: %v", err)
	}
	for _, kind := range []string{"manifest", `bad";drop`, "", "Upper", strings.Repeat("k", 32)} {
		if ValidateSequenceKind(kind) == nil {
			t.Fatalf("kind %q must be rejected", kind)
		}
	}
}

func TestManifestProbes_DeadContextIsNotAVerdict(t *testing.T) {
	// CI regression (2026-07-17, PG lane): gorm's Migrator().HasTable
	// swallows query errors into false, so a table-existence probe running
	// under an expired context read as absence — and RepairSequenceClaim
	// escalated it into a fabricated ErrSequenceManifestCorrupt (claim
	// without ledger) instead of the deadline error the caller's context
	// carried. For a repair tool, inventing corruption verdicts out of
	// transport failures is the worst failure class. Deterministic pin: a
	// pre-cancelled context must surface the context error on every probe
	// path — never a verdict, never a result-shaped lie (empty manifest,
	// absent ledger, empty history, sequence not present).
	h := openTestHandle(t)
	seq := manifestTestSequence(t, "dead_ctx", "example.com/dead/ctx", map[string]string{
		"0001_init.sql": "CREATE TABLE manifest_dead_ctx_item (id BIGINT PRIMARY KEY);",
	})
	if _, err := ApplySequence(t.Context(), h, seq); err != nil {
		t.Fatal(err)
	}

	dead, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := RepairSequenceClaim(dead, h, seq.Kind(), RepairClaimOptions{
		ExpectedOwner: seq.Owner(), NewOwner: "example.com/new/ctx", Reason: "test transfer",
	})
	if errors.Is(err, ErrSequenceManifestCorrupt) || errors.Is(err, ErrSequenceUnclaimed) {
		t.Fatalf("a dead-context probe must not fabricate a manifest verdict, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("claim repair under a dead context must surface the context error, got %v", err)
	}

	if _, err := ManifestEntries(dead, h); !errors.Is(err, context.Canceled) {
		t.Fatalf("ManifestEntries must not report an empty manifest under a dead context, got %v", err)
	}
	if _, err := LedgerSnapshot(dead, h, seq.Kind()); !errors.Is(err, context.Canceled) {
		t.Fatalf("LedgerSnapshot must not report an absent ledger under a dead context, got %v", err)
	}
	if _, err := RepairHistory(dead, h, RepairHistoryFilter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("RepairHistory must not report empty history under a dead context, got %v", err)
	}
	if _, err := SequencePresent(dead, h, seq); !errors.Is(err, context.Canceled) {
		t.Fatalf("SequencePresent must not report absence under a dead context, got %v", err)
	}

	// The live-context paths still work after the dead-context calls.
	entries, err := ManifestEntries(t.Context(), h)
	if err != nil || len(entries) != 1 || entries[0].Owner != seq.Owner() {
		t.Fatalf("dead-context probes must leave the manifest untouched: entries=%+v err=%v", entries, err)
	}
}
