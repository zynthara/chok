package audit_test

import (
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/zynthara/chok/v2/audit"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/internal/testseq"
)

func TestMigrationBehavior_AuditUpgradeMatrix(t *testing.T) {
	runAuditUpgradeMatrix(t, dbtest.Open)
}

func TestMigrationBehavior_MySQLAuditUpgradeMatrix(t *testing.T) {
	runAuditUpgradeMatrix(t, dbtest.OpenMySQL)
}

func runAuditUpgradeMatrix(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()
	testseq.RunUpgradeMatrix(t, open, audit.MigrationSequence(), db.Sequence{}, testseq.UpgradeSpec{
		Trace: auditJSONTrace,
		PrepareAdoptable: func(t testing.TB, h *db.DB) {
			t.Helper()
			if err := audit.MigrateSchema(t.Context(), h); err != nil {
				t.Fatalf("audit auto schema: %v", err)
			}
		},
	})
}

func auditJSONTrace(t testing.TB, h *db.DB) testseq.Trace {
	t.Helper()
	gdb := h.Unsafe(t.Context())
	when := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	row := &audit.Log{
		ID: "audit_matrixPayload", OccurredAt: when, Action: "matrix.write", Result: audit.ResultSuccess,
		Before:   datatypes.JSON(`{"中文":"旧值","emoji":"🔋","nested":{"nil":null,"list":[1,true]}}`),
		After:    datatypes.JSON(`{"中文":"新值","nested":{"count":2}}`),
		Metadata: datatypes.JSON(`{"source":"迁移矩阵","nullable":null}`),
	}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("audit behavior create payload row: %v", err)
	}
	if err := gdb.Create(&audit.Log{
		ID: "audit_matrixNull", OccurredAt: when.Add(time.Second), Action: "matrix.null", Result: audit.ResultSuccess,
	}).Error; err != nil {
		t.Fatalf("audit behavior create NULL row: %v", err)
	}
	if err := gdb.Create(&audit.Log{
		ID: "audit_matrixEmpty", OccurredAt: when.Add(2 * time.Second), Action: "matrix.empty", Result: audit.ResultSuccess,
		Before: datatypes.JSON(`{}`), After: datatypes.JSON(`{}`), Metadata: datatypes.JSON(`{}`),
	}).Error; err != nil {
		t.Fatalf("audit behavior create empty-object row: %v", err)
	}

	var got, nullRow, emptyRow audit.Log
	if err := gdb.First(&got, "id = ?", row.ID).Error; err != nil {
		t.Fatalf("audit behavior read payload row: %v", err)
	}
	if err := gdb.First(&nullRow, "id = ?", "audit_matrixNull").Error; err != nil {
		t.Fatalf("audit behavior read NULL row: %v", err)
	}
	if err := gdb.First(&emptyRow, "id = ?", "audit_matrixEmpty").Error; err != nil {
		t.Fatalf("audit behavior read empty-object row: %v", err)
	}
	separated := len(nullRow.Before) == 0 && len(nullRow.After) == 0 && len(nullRow.Metadata) == 0 &&
		testseq.CanonicalJSON(t, emptyRow.Before) == "{}" &&
		testseq.CanonicalJSON(t, emptyRow.After) == "{}" &&
		testseq.CanonicalJSON(t, emptyRow.Metadata) == "{}"
	if !separated {
		t.Fatalf("audit behavior SQL NULL and empty objects collapsed: null=%q/%q/%q empty=%q/%q/%q",
			nullRow.Before, nullRow.After, nullRow.Metadata, emptyRow.Before, emptyRow.After, emptyRow.Metadata)
	}

	return testseq.Trace{
		{Step: "audit_before_json", OK: true, JSON: testseq.CanonicalJSON(t, got.Before)},
		{Step: "audit_after_json", OK: true, JSON: testseq.CanonicalJSON(t, got.After)},
		{Step: "audit_metadata_json", OK: true, JSON: testseq.CanonicalJSON(t, got.Metadata)},
		{Step: "audit_json_null_vs_empty", OK: separated, Business: "null|{}"},
	}
}
