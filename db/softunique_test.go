package db

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/rid"
)

// SoftUnique's observable contract, identical across dialects even
// though the index shape differs (Postgres: partial unique index over
// live rows; MySQL/SQLite: composite UNIQUE(cols..., delete_token)):
//
//	1. two live rows with the same value conflict
//	2. soft-deleting a row releases the value for a new live row
//	3. soft-deleted rows never conflict with each other
//
// This runs on both lanes of the M3 dual-run and is the behavioural
// half of the "partial unique index generation rule" acceptance item;
// the Postgres shape assertion below is the structural half.

type SoftUniqueDoc struct {
	SoftDeleteModel
	Code string `json:"code" gorm:"size:50;not null"`
}

func (SoftUniqueDoc) RIDPrefix() string { return "sud" }

// softDelete mirrors store.Delete's soft path (deleted_at + fresh
// delete_token) without importing store (which imports db).
func softDelete(t *testing.T, h *DB, id uint) {
	t.Helper()
	err := h.Unsafe(context.Background()).Model(&SoftUniqueDoc{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"deleted_at":   gorm.Expr("CURRENT_TIMESTAMP"), // portable across all three dialects
			"delete_token": rid.NewRaw(),
		}).Error
	if err != nil {
		t.Fatalf("soft delete: %v", err)
	}
}

func TestSoftUnique_Behaviour(t *testing.T) {
	gdb := dbtest.Open(t)
	h := Wrap(gdb)
	ctx := context.Background()

	if err := h.Migrate(ctx, Table(&SoftUniqueDoc{}, SoftUnique("uk_sud_code", "code"))); err != nil {
		t.Fatal(err)
	}

	first := &SoftUniqueDoc{Code: "X"}
	if err := h.Unsafe(ctx).Create(first).Error; err != nil {
		t.Fatal(err)
	}

	// (1) live duplicate conflicts.
	if err := h.Unsafe(ctx).Create(&SoftUniqueDoc{Code: "X"}).Error; err == nil {
		t.Fatal("two live rows with the same code must conflict")
	}

	// (2) soft delete releases the value.
	softDelete(t, h, first.ID)
	second := &SoftUniqueDoc{Code: "X"}
	if err := h.Unsafe(ctx).Create(second).Error; err != nil {
		t.Fatalf("soft-deleted value must be reusable by a live row: %v", err)
	}

	// (3) soft-deleted rows never conflict with each other.
	softDelete(t, h, second.ID)
	third := &SoftUniqueDoc{Code: "X"}
	if err := h.Unsafe(ctx).Create(third).Error; err != nil {
		t.Fatalf("second soft delete + relive must work: %v", err)
	}

	var live, total int64
	h.Unsafe(ctx).Model(&SoftUniqueDoc{}).Count(&live)
	h.Unsafe(ctx).Unscoped().Model(&SoftUniqueDoc{}).Count(&total)
	if live != 1 || total != 3 {
		t.Fatalf("want 1 live / 3 total, got %d/%d", live, total)
	}
}

// TestSoftUnique_PostgresPartialIndexShape pins the SPEC §5.3 M3 rule:
// on Postgres the index is a partial unique index over the declared
// columns with WHERE deleted_at IS NULL, and delete_token is NOT part
// of the key.
func TestSoftUnique_PostgresPartialIndexShape(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("postgres-lane only (CHOK_TEST_DRIVER=postgres)")
	}
	gdb := dbtest.Open(t)
	h := Wrap(gdb)
	ctx := context.Background()

	if err := h.Migrate(ctx, Table(&SoftUniqueDoc{}, SoftUnique("uk_sud_code", "code"))); err != nil {
		t.Fatal(err)
	}

	var indexdef string
	err := h.Unsafe(ctx).Raw(
		"SELECT indexdef FROM pg_indexes WHERE indexname = 'uk_sud_code'",
	).Scan(&indexdef).Error
	if err != nil {
		t.Fatal(err)
	}
	if indexdef == "" {
		t.Fatal("uk_sud_code not found in pg_indexes")
	}
	lower := strings.ToLower(indexdef)
	if !strings.Contains(lower, "unique index") {
		t.Fatalf("index must be UNIQUE: %s", indexdef)
	}
	if !strings.Contains(lower, "where (deleted_at is null)") && !strings.Contains(lower, "where deleted_at is null") {
		t.Fatalf("index must be partial on live rows: %s", indexdef)
	}
	if strings.Contains(lower, "delete_token") {
		t.Fatalf("postgres partial index must not include delete_token: %s", indexdef)
	}
}
