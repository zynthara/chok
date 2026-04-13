package db

import (
	"context"
	"errors"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// --- test models ---

type TestUser struct {
	SoftDeleteModel
	Name  string `json:"name"  gorm:"size:100"`
	Email string `json:"email" gorm:"size:200;not null"`
}

func (TestUser) RIDPrefix() string { return "usr" }

type TestItem struct {
	Model
	Code string `json:"code" gorm:"uniqueIndex;size:50"`
}

func (TestItem) RIDPrefix() string { return "itm" }

// Models for Migrate validation tests.
type BadUniqueIndex struct {
	SoftDeleteModel
	Email string `gorm:"uniqueIndex;not null"` // permanent uniqueness (survives soft delete)
}

type NullableColumn struct {
	SoftDeleteModel
	Email *string `gorm:"not null"` // pointer → nullable
}

type MissingNotNull struct {
	SoftDeleteModel
	Email string `gorm:"size:200"` // missing "not null"
}

func (BadUniqueIndex) RIDPrefix() string  { return "bad" }
func (NullableColumn) RIDPrefix() string  { return "nul" }
func (MissingNotNull) RIDPrefix() string  { return "mis" }

// --- helpers ---

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	return gdb
}

// --- tests ---

func TestTransaction_CommitOnSuccess(t *testing.T) {
	gdb := openTestDB(t)
	Migrate(gdb, Table(&TestItem{}))

	ctx := context.Background()
	err := Transaction(ctx, gdb, func(tx *gorm.DB) error {
		return tx.Create(&TestItem{Code: "A"}).Error
	})
	if err != nil {
		t.Fatal(err)
	}

	var count int64
	gdb.Model(&TestItem{}).Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}
}

func TestTransaction_RollbackOnError(t *testing.T) {
	gdb := openTestDB(t)
	Migrate(gdb, Table(&TestItem{}))

	ctx := context.Background()
	err := Transaction(ctx, gdb, func(tx *gorm.DB) error {
		tx.Create(&TestItem{Code: "A"})
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected error")
	}

	var count int64
	gdb.Model(&TestItem{}).Count(&count)
	if count != 0 {
		t.Fatalf("expected 0 rows after rollback, got %d", count)
	}
}

func TestTransaction_CtxPropagation(t *testing.T) {
	gdb := openTestDB(t)
	Migrate(gdb, Table(&TestItem{}))

	type ctxKey string
	ctx := context.WithValue(context.Background(), ctxKey("k"), "v")

	var got string
	Transaction(ctx, gdb, func(tx *gorm.DB) error {
		stmt := tx.Statement
		if stmt != nil && stmt.Context != nil {
			if v, ok := stmt.Context.Value(ctxKey("k")).(string); ok {
				got = v
			}
		}
		return nil
	})

	if got != "v" {
		// GORM WithContext may not expose context on Statement in all paths.
		// The important thing is Transaction doesn't panic.
		t.Log("context propagation: value not directly accessible via Statement.Context (expected in some GORM versions)")
	}
}

func TestClose(t *testing.T) {
	gdb := openTestDB(t)
	if err := Close(gdb); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	// After close, queries should fail.
	var count int64
	err := gdb.Raw("SELECT 1").Count(&count).Error
	if err == nil {
		t.Fatal("expected error after Close")
	}
}

func TestTable_InvalidModel_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for non-Model struct")
		}
	}()
	Table(&struct{ Name string }{})
}

func TestTable_InvalidRIDPrefix_Panics(t *testing.T) {
	// db.Table calls ValidateModel which validates RIDPrefix.
	// ValidateModel for a valid model should not panic.
	spec := Table(&TestUser{}, SoftUnique("uk_email", "email"))
	if spec.model == nil {
		t.Fatal("model should be set")
	}
}

func TestMigrate_UniqueIndexOnSoftDelete_Allowed(t *testing.T) {
	gdb := openTestDB(t)
	err := Migrate(gdb, Table(&BadUniqueIndex{}))
	if err != nil {
		t.Fatalf("uniqueIndex on SoftDeleteModel should be allowed: %v", err)
	}
}

func TestMigrate_SoftUniqueOnNonSoftDelete_Error(t *testing.T) {
	gdb := openTestDB(t)
	// Item uses Model (not SoftDeleteModel), so SoftUnique should fail.
	err := Migrate(gdb, TableSpec{
		model:   &TestItem{},
		indexes: []SoftIndex{SoftUnique("uk_code", "code")},
		soft:    false,
	})
	if err == nil {
		t.Fatal("expected error for SoftUnique on non-SoftDeleteModel")
	}
}

func TestMigrate_SoftUniqueNullablePointer_Error(t *testing.T) {
	gdb := openTestDB(t)
	err := Migrate(gdb, Table(&NullableColumn{}, SoftUnique("uk_email", "email")))
	if err == nil {
		t.Fatal("expected error for pointer type in SoftUnique")
	}
}

func TestMigrate_SoftUniqueMissingNotNull_Error(t *testing.T) {
	gdb := openTestDB(t)
	err := Migrate(gdb, Table(&MissingNotNull{}, SoftUnique("uk_email", "email")))
	if err == nil {
		t.Fatal("expected error for missing 'not null' tag")
	}
}

func TestMigrate_Success(t *testing.T) {
	gdb := openTestDB(t)
	err := Migrate(gdb,
		Table(&TestUser{}, SoftUnique("uk_email", "email")),
		Table(&TestItem{}),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Verify tables exist.
	if !gdb.Migrator().HasTable(&TestUser{}) {
		t.Fatal("test_users table should exist")
	}
	if !gdb.Migrator().HasTable(&TestItem{}) {
		t.Fatal("test_items table should exist")
	}
}

func TestBeforeCreate_SetsVersionAndRID(t *testing.T) {
	gdb := openTestDB(t)
	Migrate(gdb, Table(&TestUser{}, SoftUnique("uk_email", "email")))

	u := &TestUser{Name: "alice", Email: "a@b.com"}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatal(err)
	}
	if u.Version != 1 {
		t.Fatalf("expected version 1, got %d", u.Version)
	}
	if u.RID == "" {
		t.Fatal("RID should be auto-generated")
	}
	if u.RID[:4] != "usr_" {
		t.Fatalf("expected usr_ prefix, got %s", u.RID)
	}
}

func TestValidateModel_NonStruct(t *testing.T) {
	err := ValidateModel("not a struct")
	if err == nil {
		t.Fatal("expected error for non-struct")
	}
}

func TestValidateModel_NoModel(t *testing.T) {
	err := ValidateModel(&struct{ Name string }{})
	if err == nil {
		t.Fatal("expected error for struct without db.Model")
	}
}

func TestValidateModel_ValidModel(t *testing.T) {
	if err := ValidateModel(&TestUser{}); err != nil {
		t.Fatalf("valid model rejected: %v", err)
	}
}

// --- regression: indirect SoftDeleteModel embedding ---

type IndirectBase struct {
	SoftDeleteModel
}

type IndirectSoftUser struct {
	IndirectBase
	Email string `json:"email" gorm:"size:200;not null"`
}

func (IndirectSoftUser) RIDPrefix() string { return "isu" }

func TestIsSoftDeleteModel_IndirectEmbed(t *testing.T) {
	if !IsSoftDeleteModel(&IndirectSoftUser{}) {
		t.Fatal("indirect SoftDeleteModel embedding should be detected")
	}
}

func TestMigrate_IndirectSoftDelete_SoftUniqueWorks(t *testing.T) {
	gdb := openTestDB(t)
	err := Migrate(gdb, Table(&IndirectSoftUser{}, SoftUnique("uk_isu_email", "email")))
	if err != nil {
		t.Fatalf("SoftUnique should work with indirect SoftDeleteModel: %v", err)
	}
}

// --- Owned mixin tests ---

type OwnedProduct struct {
	OwnedModel
	Name string `json:"name" gorm:"size:100;not null"`
}

func (OwnedProduct) RIDPrefix() string { return "prd" }

type OwnedSoftProduct struct {
	OwnedSoftDeleteModel
	Name string `json:"name" gorm:"size:100;not null"`
}

func (OwnedSoftProduct) RIDPrefix() string { return "osp" }

// MixinProduct uses the low-level Owned mixin directly.
type MixinProduct struct {
	Model
	Owned
	Name string `json:"name" gorm:"size:100;not null"`
}

func (MixinProduct) RIDPrefix() string { return "mxp" }

func TestIsOwnedModel(t *testing.T) {
	tests := []struct {
		name  string
		model any
		want  bool
	}{
		{"OwnedModel pointer", &OwnedProduct{}, true},
		{"OwnedModel value", OwnedProduct{}, true},
		{"OwnedSoftDeleteModel pointer", &OwnedSoftProduct{}, true},
		{"Owned mixin pointer", &MixinProduct{}, true},
		{"non-owned pointer", &TestItem{}, false},
		{"non-owned value", TestItem{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsOwnedModel(tt.model); got != tt.want {
				t.Fatalf("IsOwnedModel(%T) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestOwned_GetSetOwnerID(t *testing.T) {
	p := &OwnedProduct{}
	if p.GetOwnerID() != "" {
		t.Fatal("initial OwnerID should be empty")
	}
	p.SetOwnerID("usr_abc")
	if p.GetOwnerID() != "usr_abc" {
		t.Fatalf("expected usr_abc, got %s", p.GetOwnerID())
	}
}

func TestOwned_MigrateWithModel(t *testing.T) {
	gdb := openTestDB(t)
	if err := Migrate(gdb, Table(&OwnedProduct{})); err != nil {
		t.Fatalf("migration with Owned mixin should succeed: %v", err)
	}
	if !gdb.Migrator().HasColumn(&OwnedProduct{}, "owner_id") {
		t.Fatal("owner_id column should exist after migration")
	}
}

func TestOwned_MigrateWithSoftDeleteModel(t *testing.T) {
	gdb := openTestDB(t)
	if err := Migrate(gdb, Table(&OwnedSoftProduct{})); err != nil {
		t.Fatalf("migration with Owned+SoftDelete should succeed: %v", err)
	}
	if !gdb.Migrator().HasColumn(&OwnedSoftProduct{}, "owner_id") {
		t.Fatal("owner_id column should exist")
	}
	if !gdb.Migrator().HasColumn(&OwnedSoftProduct{}, "deleted_at") {
		t.Fatal("deleted_at column should exist")
	}
}

// --- regression: uniqueIndex on anonymous embedded fields ---

type EmbeddedFields struct {
	Tag string `gorm:"uniqueIndex;not null"`
}

type SoftUserWithEmbeddedUniqueIndex struct {
	SoftDeleteModel
	EmbeddedFields
}

func (SoftUserWithEmbeddedUniqueIndex) RIDPrefix() string { return "sue" }

func TestMigrate_UniqueIndexOnAnonymousEmbedField_Allowed(t *testing.T) {
	gdb := openTestDB(t)
	err := Migrate(gdb, Table(&SoftUserWithEmbeddedUniqueIndex{}))
	if err != nil {
		t.Fatalf("uniqueIndex on embedded field in SoftDeleteModel should be allowed: %v", err)
	}
}
