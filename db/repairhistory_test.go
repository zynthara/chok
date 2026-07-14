package db

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zynthara/chok/v2/db/internal/testlane"
)

// dirtyRepairFixture applies a deliberately broken migration so the ledger
// holds one dirty row, and returns the sequence plus the dirty checksum.
func dirtyRepairFixture(t *testing.T, h *DB, kind, table string) (Sequence, string) {
	t.Helper()
	seq := manifestTestSequence(t, kind, "example.com/history/"+kind, map[string]string{
		"0001_break.sql": "CREATE TABLE " + table + " (id BIGINT PRIMARY KEY); INVALID SQL;",
	})
	if _, err := ApplySequence(t.Context(), h, seq); err == nil {
		t.Fatal("broken migration must fail and leave a dirty row")
	}
	status, err := SequenceStatus(t.Context(), h, seq)
	if err != nil || len(status.Dirty) != 1 {
		t.Fatalf("dirty fixture status = %+v err=%v", status, err)
	}
	return seq, status.Dirty[0].Checksum
}

func repairHistoryRows(t *testing.T, h *DB, filter RepairHistoryFilter) []RepairRecord {
	t.Helper()
	records, err := RepairHistory(t.Context(), h, filter)
	if err != nil {
		t.Fatalf("read repair history: %v", err)
	}
	return records
}

func TestRepairHistory_LedgerActionsRecordRows(t *testing.T) {
	h := openTestHandle(t)
	ctx := t.Context()

	// retry: delete the dirty row so the next up reruns it.
	retrySeq, retryChecksum := dirtyRepairFixture(t, h, "hist_retry", "hist_retry_item")
	retryReport, err := RepairSequence(ctx, h, retrySeq, RepairOptions{
		Action: RepairRetry, Version: 1, ExpectedChecksum: retryChecksum,
		Reason: "operator restored pre-migration state", Operator: "ops@jumphost",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retryReport.Operator != "ops@jumphost" || retryReport.ChokVersion != currentChokVersion() {
		t.Fatalf("report must echo persisted identity: %+v", retryReport)
	}
	retryStatus, err := SequenceStatus(ctx, h, retrySeq)
	if err != nil || retryStatus.Fence != nil {
		t.Fatalf("retry must leave no fence after release: %+v err=%v", retryStatus, err)
	}

	// mark-applied: keep the row, clear dirty.
	markSeq, markChecksum := dirtyRepairFixture(t, h, "hist_mark", "hist_mark_item")
	if _, err := RepairSequence(ctx, h, markSeq, RepairOptions{
		Action: RepairMarkApplied, Version: 1, ExpectedChecksum: markChecksum,
		Reason: "DDL completed manually",
	}); err != nil {
		t.Fatal(err)
	}

	// accept-drift: bless an edited file's checksum.
	driftSeq := manifestTestSequence(t, "hist_drift", "example.com/history/hist_drift", map[string]string{
		"0001_init.sql": "CREATE TABLE hist_drift_item (id BIGINT PRIMARY KEY);",
	})
	if _, err := ApplySequence(ctx, h, driftSeq); err != nil {
		t.Fatal(err)
	}
	drifted := manifestTestSequence(t, "hist_drift", "example.com/history/hist_drift", map[string]string{
		"0001_init.sql": "CREATE TABLE hist_drift_item (id BIGINT PRIMARY KEY); -- reformatted",
	})
	driftStatus, err := SequenceStatus(ctx, h, drifted)
	if err != nil || len(driftStatus.Drift) != 1 {
		t.Fatalf("drift fixture status = %+v err=%v", driftStatus, err)
	}
	if _, err := RepairSequence(ctx, h, drifted, RepairOptions{
		Action: RepairAcceptDrift, Version: 1, ExpectedChecksum: driftStatus.Drift[0].Ledger,
		Reason: "reviewed reformatting, content equivalent",
	}); err != nil {
		t.Fatal(err)
	}

	records := repairHistoryRows(t, h, RepairHistoryFilter{})
	if len(records) != 3 {
		t.Fatalf("got %d history rows, want 3: %+v", len(records), records)
	}
	if records[0].Action != string(RepairAcceptDrift) || records[2].Action != string(RepairRetry) {
		t.Fatalf("history must be most recent first: %+v", records)
	}
	retryRows := repairHistoryRows(t, h, RepairHistoryFilter{Kind: "hist_retry"})
	if len(retryRows) != 1 {
		t.Fatalf("kind filter rows = %+v", retryRows)
	}
	row := retryRows[0]
	if row.Ledger != retrySeq.Ledger() || row.Version != 1 || row.File != "0001_break.sql" ||
		row.LedgerChecksum != retryChecksum || row.CurrentChecksum != retryChecksum ||
		row.Reason != "operator restored pre-migration state" ||
		row.Operator != "ops@jumphost" || row.ChokVersion != retryReport.ChokVersion ||
		row.PreviousOwner != "" || row.NewOwner != "" || row.RepairedAt.IsZero() {
		t.Fatalf("retry history row mismatch: %+v", row)
	}
}

func TestRepairHistory_ClaimTransferRecordsRowAndRequiresReason(t *testing.T) {
	h := openTestHandle(t)
	ctx := t.Context()
	seq := manifestTestSequence(t, "hist_claim", "example.com/history/old", map[string]string{
		"0001_init.sql": "CREATE TABLE hist_claim_item (id BIGINT PRIMARY KEY);",
	})
	if _, err := ApplySequence(ctx, h, seq); err != nil {
		t.Fatal(err)
	}
	if _, err := RepairSequenceClaim(ctx, h, seq.Kind(), RepairClaimOptions{
		ExpectedOwner: seq.Owner(), NewOwner: "example.com/history/new",
	}); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("claim transfer without reason must fail, got %v", err)
	}
	if len(repairHistoryRows(t, h, RepairHistoryFilter{})) != 0 {
		t.Fatal("rejected transfer must not write history")
	}
	report, err := RepairSequenceClaim(ctx, h, seq.Kind(), RepairClaimOptions{
		ExpectedOwner: seq.Owner(), NewOwner: "example.com/history/new",
		Reason: "package moved", Operator: "release-bot@ci",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Reason != "package moved" || report.Operator != "release-bot@ci" || report.ChokVersion != currentChokVersion() {
		t.Fatalf("claim report must echo persisted evidence: %+v", report)
	}
	records := repairHistoryRows(t, h, RepairHistoryFilter{Kind: seq.Kind()})
	if len(records) != 1 {
		t.Fatalf("claim history rows = %+v", records)
	}
	row := records[0]
	if row.Action != repairActionClaimTransfer || row.Version != 0 ||
		row.PreviousOwner != seq.Owner() || row.NewOwner != "example.com/history/new" ||
		row.File != "" || row.LedgerChecksum != "" || row.CurrentChecksum != "" ||
		row.Reason != "package moved" || row.Operator != "release-bot@ci" {
		t.Fatalf("claim history row mismatch: %+v", row)
	}
}

func TestRepairHistory_FailedRepairWritesNothing(t *testing.T) {
	h := openTestHandle(t)
	seq, checksum := dirtyRepairFixture(t, h, "hist_fail", "hist_fail_item")
	wrong := strings.Repeat("ab", 32)
	if wrong == checksum {
		t.Fatal("fixture checksum collision")
	}
	if _, err := RepairSequence(t.Context(), h, seq, RepairOptions{
		Action: RepairRetry, Version: 1, ExpectedChecksum: wrong, Reason: "stale inspection",
	}); err == nil {
		t.Fatal("checksum mismatch must fail the repair")
	}
	if rows := repairHistoryRows(t, h, RepairHistoryFilter{}); len(rows) != 0 {
		t.Fatalf("failed repair wrote history: %+v", rows)
	}
	status, err := SequenceStatus(t.Context(), h, seq)
	if err != nil || len(status.Dirty) != 1 {
		t.Fatalf("failed repair must keep the dirty row: %+v err=%v", status, err)
	}
}

func TestRepairHistory_PostgresAtomicFenceAndRollback(t *testing.T) {
	if testlane.Driver() != "postgres" {
		t.Skip("postgres lane only")
	}
	t.Run("fence cleanup joins the repair transaction", func(t *testing.T) {
		h := openTestHandle(t)
		seq, checksum := dirtyRepairFixture(t, h, "hist_pg_fence", "hist_pg_fence_item")
		if _, err := RepairSequence(t.Context(), h, seq, RepairOptions{
			Action: RepairRetry, Version: 1, ExpectedChecksum: checksum, Reason: "pg atomic check",
		}); err != nil {
			t.Fatal(err)
		}
		status, err := SequenceStatus(t.Context(), h, seq)
		if err != nil || status.Fence != nil {
			t.Fatalf("fence must clear in the repair commit: %+v err=%v", status, err)
		}
		if rows := repairHistoryRows(t, h, RepairHistoryFilter{}); len(rows) != 1 {
			t.Fatalf("want exactly one history row, got %+v", rows)
		}
	})
	t.Run("history insert failure rolls the repair back", func(t *testing.T) {
		h := openTestHandle(t)
		// A hand-made table whose reason column cannot hold a real reason:
		// core columns all present so the additive upgrade passes, but the
		// history INSERT fails and must take the ledger CAS down with it.
		if err := h.Unsafe(t.Context()).Exec(
			"CREATE TABLE " + sequenceRepairHistoryTable + " (" +
				"id BIGSERIAL PRIMARY KEY, kind VARCHAR(31) NOT NULL, ledger VARCHAR(64) NOT NULL, " +
				"dialect VARCHAR(32) NOT NULL, action VARCHAR(32) NOT NULL, version BIGINT NOT NULL DEFAULT 0, " +
				"file VARCHAR(255) NOT NULL DEFAULT '', ledger_checksum VARCHAR(64) NOT NULL DEFAULT '', " +
				"current_checksum VARCHAR(64) NOT NULL DEFAULT '', previous_owner VARCHAR(190) NOT NULL DEFAULT '', " +
				"new_owner VARCHAR(190) NOT NULL DEFAULT '', reason VARCHAR(3) NOT NULL, " +
				"operator VARCHAR(190) NOT NULL DEFAULT '', chok_version VARCHAR(64) NOT NULL DEFAULT '', " +
				"repaired_at TIMESTAMP NOT NULL)",
		).Error; err != nil {
			t.Fatal(err)
		}
		seq, checksum := dirtyRepairFixture(t, h, "hist_pg_atomic", "hist_pg_atomic_item")
		if _, err := RepairSequence(t.Context(), h, seq, RepairOptions{
			Action: RepairRetry, Version: 1, ExpectedChecksum: checksum, Reason: "far too long for varchar three",
		}); err == nil {
			t.Fatal("unrecordable repair must fail")
		}
		status, err := SequenceStatus(t.Context(), h, seq)
		if err != nil || len(status.Dirty) != 1 {
			t.Fatalf("rolled-back repair must keep the dirty row: %+v err=%v", status, err)
		}
		if status.Fence == nil {
			t.Fatal("rolled-back repair must keep the compatibility fence")
		}
		var rows int64
		if err := h.Unsafe(t.Context()).Raw("SELECT COUNT(*) FROM " + sequenceRepairHistoryTable).Scan(&rows).Error; err != nil || rows != 0 {
			t.Fatalf("rolled-back repair must leave zero history rows: rows=%d err=%v", rows, err)
		}
	})
	t.Run("claim transfer rolls back with its history", func(t *testing.T) {
		h := openTestHandle(t)
		seq := manifestTestSequence(t, "hist_pg_claim", "example.com/pg/claim", map[string]string{
			"0001_init.sql": "CREATE TABLE hist_pg_claim_item (id BIGINT PRIMARY KEY);",
		})
		if _, err := ApplySequence(t.Context(), h, seq); err != nil {
			t.Fatal(err)
		}
		if err := h.Unsafe(t.Context()).Exec(
			"CREATE TABLE " + sequenceRepairHistoryTable + " (" +
				"id BIGSERIAL PRIMARY KEY, kind VARCHAR(31) NOT NULL, ledger VARCHAR(64) NOT NULL, " +
				"dialect VARCHAR(32) NOT NULL, action VARCHAR(32) NOT NULL, version BIGINT NOT NULL DEFAULT 0, " +
				"file VARCHAR(255) NOT NULL DEFAULT '', ledger_checksum VARCHAR(64) NOT NULL DEFAULT '', " +
				"current_checksum VARCHAR(64) NOT NULL DEFAULT '', previous_owner VARCHAR(190) NOT NULL DEFAULT '', " +
				"new_owner VARCHAR(190) NOT NULL DEFAULT '', reason VARCHAR(3) NOT NULL, " +
				"operator VARCHAR(190) NOT NULL DEFAULT '', chok_version VARCHAR(64) NOT NULL DEFAULT '', " +
				"repaired_at TIMESTAMP NOT NULL)",
		).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := RepairSequenceClaim(t.Context(), h, seq.Kind(), RepairClaimOptions{
			ExpectedOwner: seq.Owner(), NewOwner: "example.com/pg/next",
			Reason: "far too long for varchar three",
		}); err == nil {
			t.Fatal("unrecordable claim transfer must fail")
		}
		entries, err := ManifestEntries(t.Context(), h)
		if err != nil || len(entries) != 1 || entries[0].Owner != seq.Owner() {
			t.Fatalf("rolled-back transfer must keep the owner: %+v err=%v", entries, err)
		}
		var rows int64
		if err := h.Unsafe(t.Context()).Raw("SELECT COUNT(*) FROM " + sequenceRepairHistoryTable).Scan(&rows).Error; err != nil || rows != 0 {
			t.Fatalf("rolled-back transfer must leave zero history rows: rows=%d err=%v", rows, err)
		}
	})
}

func TestRepairHistory_ReadContract(t *testing.T) {
	h := openTestHandle(t)
	gdb := h.Unsafe(t.Context())
	if _, err := RepairHistory(t.Context(), h, RepairHistoryFilter{Limit: -1}); err == nil {
		t.Fatal("negative limit must be rejected")
	}
	if rows := repairHistoryRows(t, h, RepairHistoryFilter{}); len(rows) != 0 {
		t.Fatalf("missing table must read empty, got %+v", rows)
	}
	if _, err := RepairHistory(t.Context(), h, RepairHistoryFilter{Kind: sequenceManifestKind}); err == nil {
		t.Fatal("manifest kind filter must be rejected")
	}
	if _, err := RepairHistory(t.Context(), h, RepairHistoryFilter{Kind: `bad";drop`}); err == nil {
		t.Fatal("unsafe kind filter must be rejected")
	}

	if err := ensureRepairHistoryBase(gdb); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	checksum := strings.Repeat("ab", 32)
	for i, kind := range []string{"hist_read_a", "hist_read_b", repairHistoryKindApp} {
		ledger := ledgerForSequenceKind(kind)
		if kind == repairHistoryKindApp {
			ledger = ledgerTable
		}
		if err := insertRepairHistory(gdb, RepairRecord{
			Kind: kind, Ledger: ledger, Dialect: gdb.Dialector.Name(),
			Action: string(RepairRetry), Version: 1, File: "0001_x.sql",
			LedgerChecksum: checksum, CurrentChecksum: checksum,
			Reason: "row " + kind, ChokVersion: currentChokVersion(),
			RepairedAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if rows := repairHistoryRows(t, h, RepairHistoryFilter{Limit: 2}); len(rows) != 2 ||
		rows[0].Kind != repairHistoryKindApp || rows[1].Kind != "hist_read_b" {
		t.Fatalf("limit must keep the most recent rows: %+v", rows)
	}
	appRows := repairHistoryRows(t, h, RepairHistoryFilter{Kind: repairHistoryKindApp})
	if len(appRows) != 1 || appRows[0].Ledger != ledgerTable {
		t.Fatalf("app filter rows = %+v", appRows)
	}
}

func TestRepairHistory_ValidationMatrix(t *testing.T) {
	checksum := strings.Repeat("cd", 32)
	validLedger := RepairRecord{
		Kind: "hist_valid", Ledger: ledgerForSequenceKind("hist_valid"), Dialect: "sqlite",
		Action: string(RepairMarkApplied), Version: 3, File: "0003_x.sql",
		LedgerChecksum: checksum, CurrentChecksum: checksum,
		Reason: "valid", RepairedAt: time.Now().UTC(),
	}
	validDrift := RepairRecord{
		Kind: "hist_valid", Ledger: ledgerForSequenceKind("hist_valid"), Dialect: "sqlite",
		Action: string(RepairAcceptDrift), Version: 2, File: "0002_y.sql",
		LedgerChecksum: strings.Repeat("aa", 32), CurrentChecksum: strings.Repeat("bb", 32),
		Reason: "valid", RepairedAt: time.Now().UTC(),
	}
	validClaim := RepairRecord{
		Kind: "hist_valid", Ledger: ledgerForSequenceKind("hist_valid"), Dialect: "sqlite",
		Action: repairActionClaimTransfer, Version: 0,
		PreviousOwner: "example.com/a", NewOwner: "example.com/b",
		Reason: "valid", RepairedAt: time.Now().UTC(),
	}
	cases := []struct {
		name   string
		tamper string
		args   []any
	}{
		{"invalid action", "UPDATE " + sequenceRepairHistoryTable + " SET action = 'sneak'", nil},
		{"ledger mismatch", "UPDATE " + sequenceRepairHistoryTable + " SET ledger = 'schema_migrations_chok_other' WHERE action != 'claim-transfer'", nil},
		{"claim with version", "UPDATE " + sequenceRepairHistoryTable + " SET version = 9 WHERE action = 'claim-transfer'", nil},
		{"ledger action with owner", "UPDATE " + sequenceRepairHistoryTable + " SET new_owner = 'example.com/x' WHERE action != 'claim-transfer'", nil},
		{"present checksum emptied", "UPDATE " + sequenceRepairHistoryTable + " SET ledger_checksum = '' WHERE action != 'claim-transfer'", nil},
		{"present file emptied", "UPDATE " + sequenceRepairHistoryTable + " SET file = '' WHERE action != 'claim-transfer'", nil},
		{"file impersonates another version", "UPDATE " + sequenceRepairHistoryTable + " SET file = '0009_other.sql' WHERE version = 3", nil},
		{"file with path separator", "UPDATE " + sequenceRepairHistoryTable + " SET file = ? WHERE version = 3", []any{"0003_../x.sql"}},
		{"file with terminal escape", "UPDATE " + sequenceRepairHistoryTable + " SET file = ? WHERE version = 3", []any{"0003_\x1b[31mx.sql"}},
		{"accept-drift without checksum change", "UPDATE " + sequenceRepairHistoryTable + " SET current_checksum = ledger_checksum WHERE action = 'accept-drift'", nil},
		{"reserved claim target", "UPDATE " + sequenceRepairHistoryTable + " SET kind = 'account', ledger = 'schema_migrations_chok_account' WHERE action = 'claim-transfer'", nil},
		{"reason emptied", "UPDATE " + sequenceRepairHistoryTable + " SET reason = ' '", nil},
		{"reason untrimmed", "UPDATE " + sequenceRepairHistoryTable + " SET reason = ' padded'", nil},
		{"app with sequence ledger", "UPDATE " + sequenceRepairHistoryTable + " SET kind = 'app' WHERE action != 'claim-transfer'", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := openTestHandle(t)
			gdb := h.Unsafe(t.Context())
			if err := ensureRepairHistoryBase(gdb); err != nil {
				t.Fatal(err)
			}
			for _, record := range []RepairRecord{validLedger, validDrift, validClaim} {
				if err := insertRepairHistory(gdb, record); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := RepairHistory(t.Context(), h, RepairHistoryFilter{}); err != nil {
				t.Fatalf("untampered rows must read cleanly: %v", err)
			}
			if err := gdb.Exec(tc.tamper, tc.args...).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := RepairHistory(t.Context(), h, RepairHistoryFilter{}); !errors.Is(err, ErrRepairHistoryCorrupt) {
				t.Fatalf("tampered history must fail closed, got %v", err)
			}
		})
	}
}

func TestRepairHistory_ReservedKindsAndOperatorContract(t *testing.T) {
	for _, kind := range []string{repairHistoryKindApp, "repairs", sequenceManifestKind} {
		if err := ValidateSequenceKind(kind); err == nil {
			t.Fatalf("kind %q must be reserved", kind)
		}
		if _, err := OwnedSequence(kind, ownedManifestTestFS(map[string]string{"0001_x.sql": "SELECT 1;"}), Baseline{},
			SequenceOwner("example.com/reserved/probe")); err == nil {
			t.Fatalf("OwnedSequence(%q) must be rejected", kind)
		}
	}

	h := openTestHandle(t)
	seq, checksum := dirtyRepairFixture(t, h, "hist_operator", "hist_operator_item")
	if _, err := RepairSequence(t.Context(), h, seq, RepairOptions{
		Action: RepairRetry, Version: 1, ExpectedChecksum: checksum,
		Reason: "explicit operator must validate", Operator: "bad\x00operator",
	}); err == nil {
		t.Fatal("control characters in an explicit operator must be rejected")
	}
	status, err := SequenceStatus(t.Context(), h, seq)
	if err != nil || len(status.Dirty) != 1 {
		t.Fatalf("rejected operator must not repair anything: %+v err=%v", status, err)
	}
	report, err := RepairSequence(t.Context(), h, seq, RepairOptions{
		Action: RepairRetry, Version: 1, ExpectedChecksum: checksum, Reason: "derived operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := repairHistoryRows(t, h, RepairHistoryFilter{Kind: seq.Kind()})
	if len(rows) != 1 || rows[0].Operator != report.Operator {
		t.Fatalf("report operator %q must match persisted %+v", report.Operator, rows)
	}
}

func TestRepairHistory_LegacyShapeFallbackAndConcurrentUpgrade(t *testing.T) {
	t.Run("missing additive columns fall back and survive the upgrade", func(t *testing.T) {
		h := openTestHandle(t)
		gdb := h.Unsafe(t.Context())
		// Build the legacy shape from the dialect-correct base — a hand-typed
		// CREATE would need per-dialect auto-increment spellings — then drop
		// the additive tail, same as the concurrent-upgrade fixture below.
		if err := ensureRepairHistoryBase(gdb); err != nil {
			t.Fatal(err)
		}
		for _, column := range []string{"operator", "chok_version"} {
			if err := gdb.Exec("ALTER TABLE " + sequenceRepairHistoryTable + " DROP COLUMN " + column).Error; err != nil {
				t.Skipf("cannot drop %s to build legacy shape on this lane: %v", column, err)
			}
		}
		checksum := strings.Repeat("ef", 32)
		if err := gdb.Exec(
			"INSERT INTO "+sequenceRepairHistoryTable+
				" (kind, ledger, dialect, action, version, file, ledger_checksum, current_checksum, reason, repaired_at)"+
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			"hist_legacy", ledgerForSequenceKind("hist_legacy"), gdb.Dialector.Name(),
			string(RepairRetry), 1, "0001_x.sql", checksum, checksum,
			"written before additive columns", time.Now().UTC(),
		).Error; err != nil {
			t.Fatal(err)
		}
		rows := repairHistoryRows(t, h, RepairHistoryFilter{})
		if len(rows) != 1 || rows[0].Operator != "" || rows[0].ChokVersion != "" {
			t.Fatalf("absent additive columns must fall back to empty: %+v", rows)
		}

		// A real repair runs ensureRepairHistoryColumns, whose ALTER backfills
		// the legacy row with empty defaults. The legacy row must stay valid
		// evidence afterwards — the exact regression an action-dependent
		// column in the additive set would reintroduce.
		seq, dirtyChecksum := dirtyRepairFixture(t, h, "hist_upgraded", "hist_upgraded_item")
		if _, err := RepairSequence(t.Context(), h, seq, RepairOptions{
			Action: RepairRetry, Version: 1, ExpectedChecksum: dirtyChecksum, Reason: "post-upgrade write",
		}); err != nil {
			t.Fatal(err)
		}
		rows = repairHistoryRows(t, h, RepairHistoryFilter{})
		if len(rows) != 2 {
			t.Fatalf("legacy row must survive the additive upgrade: %+v", rows)
		}
		if rows[1].Kind != "hist_legacy" || rows[1].Operator != "" || rows[1].ChokVersion != "" {
			t.Fatalf("backfilled legacy row drifted: %+v", rows[1])
		}
	})
	t.Run("missing core column fails closed", func(t *testing.T) {
		h := openTestHandle(t)
		gdb := h.Unsafe(t.Context())
		if err := gdb.Exec(
			"CREATE TABLE " + sequenceRepairHistoryTable + " (" +
				"id INTEGER PRIMARY KEY, kind VARCHAR(31) NOT NULL, ledger VARCHAR(64) NOT NULL, " +
				"dialect VARCHAR(32) NOT NULL, action VARCHAR(32) NOT NULL, version BIGINT NOT NULL DEFAULT 0, " +
				"reason TEXT NOT NULL, repaired_at TIMESTAMP NOT NULL)",
		).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := RepairHistory(t.Context(), h, RepairHistoryFilter{}); !errors.Is(err, ErrRepairHistoryCorrupt) {
			t.Fatalf("a table missing evidence columns must fail closed, got %v", err)
		}
	})
	t.Run("two kinds race the additive upgrade", func(t *testing.T) {
		h := openTestHandle(t)
		gdb := h.Unsafe(t.Context())
		if err := ensureRepairHistoryBase(gdb); err != nil {
			t.Fatal(err)
		}
		for _, column := range []string{"chok_version", "operator"} {
			if err := gdb.Exec("ALTER TABLE " + sequenceRepairHistoryTable + " DROP COLUMN " + column).Error; err != nil {
				t.Skipf("cannot drop %s to build legacy shape on this lane: %v", column, err)
			}
		}
		seqA, checksumA := dirtyRepairFixture(t, h, "hist_race_a", "hist_race_a_item")
		seqB, checksumB := dirtyRepairFixture(t, h, "hist_race_b", "hist_race_b_item")
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		repairOne := func(seq Sequence, checksum string) {
			defer wg.Done()
			_, err := RepairSequence(t.Context(), h, seq, RepairOptions{
				Action: RepairRetry, Version: 1, ExpectedChecksum: checksum, Reason: "concurrent upgrade",
			})
			errs <- err
		}
		wg.Add(2)
		go repairOne(seqA, checksumA)
		go repairOne(seqB, checksumB)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent repair during additive upgrade: %v", err)
			}
		}
		if rows := repairHistoryRows(t, h, RepairHistoryFilter{}); len(rows) != 2 {
			t.Fatalf("both repairs must be recorded: %+v", rows)
		}
	})
}

func TestRepairHistory_FilenameRuleSharedWithLoader(t *testing.T) {
	// The loader and the history reader share one filename rule, so a
	// migration that can be loaded (and therefore repaired) can never
	// persist a history row the reader rejects.
	for _, name := range []string{"0001_\x1b[31mx.sql", "0001_a\\b.sql", "0001_\xffx.sql"} {
		fsys := fstest.MapFS{name: &fstest.MapFile{Data: []byte("SELECT 1;")}}
		if _, err := LoadMigrations(fsys); err == nil {
			t.Fatalf("loader must reject %q", name)
		}
	}
	fsys := fstest.MapFS{"0001_ok.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}}
	if _, err := LoadMigrations(fsys); err != nil {
		t.Fatalf("clean filename rejected: %v", err)
	}
	if err := validateRepairHistoryFile("0001_ok.sql", 1); err != nil {
		t.Fatalf("reader must accept what the loader accepts: %v", err)
	}
}

func TestRepairHistory_MySQLLedgerAndClaim(t *testing.T) {
	h := openMySQLMigrationTestHandle(t, "repairhistory")
	ctx := t.Context()
	seq, checksum := dirtyRepairFixture(t, h, "hist_mysql", "hist_mysql_item")
	if _, err := RepairSequence(ctx, h, seq, RepairOptions{
		Action: RepairRetry, Version: 1, ExpectedChecksum: checksum, Reason: "mysql lane retry",
	}); err != nil {
		t.Fatal(err)
	}
	claimSeq := manifestTestSequence(t, "hist_mysql_claim", "example.com/mysql/history", map[string]string{
		"0001_init.sql": "CREATE TABLE hist_mysql_claim_item (id BIGINT PRIMARY KEY);",
	})
	if _, err := ApplySequence(ctx, h, claimSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := RepairSequenceClaim(ctx, h, claimSeq.Kind(), RepairClaimOptions{
		ExpectedOwner: claimSeq.Owner(), NewOwner: "example.com/mysql/next",
		Reason: "mysql lane transfer",
	}); err != nil {
		t.Fatal(err)
	}
	records := repairHistoryRows(t, h, RepairHistoryFilter{})
	if len(records) != 2 || records[0].Action != repairActionClaimTransfer || records[1].Action != string(RepairRetry) {
		t.Fatalf("mysql history rows = %+v", records)
	}
}
