package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// --- test models (foreign-table / append-only gate, arch-backlog #13) ---

// UserRoleJoin is the canonical foreign shape: a pure join table with a
// composite primary key and no chok base model.
type UserRoleJoin struct {
	UserID uint `gorm:"primaryKey"`
	RoleID uint `gorm:"primaryKey"`
}

// NoPKForeign has no primary key at all — ForeignTable must reject it.
type NoPKForeign struct {
	Label string `gorm:"size:40"`
}

// AuditSample is the canonical append-only shape.
type AuditSample struct {
	AppendOnlyModel
	Actor string `json:"actor" gorm:"size:100"`
}

// BothBases carries both markers — ambiguous identity. Implemented by
// embedding Model and hand-implementing the append marker (in-package
// test privilege) so the duplicated ID/CreatedAt fields of a double
// embed don't trip go vet; ValidateAppendModel only consults the
// marker interfaces, so the check exercised is identical.
type BothBases struct {
	Model
}

func (BothBases) chokAppendModel() {}

// PrefixedAppend implements RIDPrefixer on an append-only model —
// there is no RID column for the prefix to apply to.
type PrefixedAppend struct {
	AppendOnlyModel
	Kind string `json:"kind" gorm:"size:40"`
}

func (PrefixedAppend) RIDPrefix() string { return "pap" }

// --- helpers ---

func mustPanicContaining(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q, got none", want)
		}
		if !strings.Contains(fmt.Sprint(r), want) {
			t.Fatalf("panic message %v does not contain %q", r, want)
		}
	}()
	fn()
}

// --- ForeignTable ---

func TestForeignTable_CompositePKMigrateAndRoundtrip(t *testing.T) {
	gdb := openTestDB(t)
	if err := Migrate(context.Background(), gdb, ForeignTable(&UserRoleJoin{})); err != nil {
		t.Fatalf("ForeignTable migrate: %v", err)
	}
	if !gdb.Migrator().HasTable(&UserRoleJoin{}) {
		t.Fatal("join table not created")
	}

	// Join-row DML is gorm-native through the escape hatch — chok
	// deliberately ships no store for foreign tables.
	rows := []UserRoleJoin{{UserID: 1, RoleID: 10}, {UserID: 1, RoleID: 11}, {UserID: 2, RoleID: 10}}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatalf("insert join rows: %v", err)
	}
	var count int64
	if err := gdb.Model(&UserRoleJoin{}).Where("user_id = ?", 1).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("want 2 rows for user 1, got %d", count)
	}

	// The composite key must actually constrain: same (user_id, role_id)
	// pair again must violate the PK.
	if err := gdb.Create(&UserRoleJoin{UserID: 1, RoleID: 10}).Error; err == nil {
		t.Fatal("duplicate composite key must be rejected by the primary key")
	}

	if err := gdb.Delete(&UserRoleJoin{}, "user_id = ? AND role_id = ?", 1, 10).Error; err != nil {
		t.Fatalf("delete join row: %v", err)
	}
	if err := gdb.Model(&UserRoleJoin{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("want 2 rows after delete, got %d", count)
	}
}

func TestForeignTable_MigratesAlongsideChokSpecs(t *testing.T) {
	gdb := openTestDB(t)
	err := Migrate(context.Background(), gdb,
		Table(&TestItem{}),
		ForeignTable(&UserRoleJoin{}),
		Table(&AuditSample{}),
	)
	if err != nil {
		t.Fatalf("mixed spec migrate: %v", err)
	}
	for _, m := range []any{&TestItem{}, &UserRoleJoin{}, &AuditSample{}} {
		if !gdb.Migrator().HasTable(m) {
			t.Fatalf("table for %T not created", m)
		}
	}
}

func TestForeignTable_NoPrimaryKey_Panics(t *testing.T) {
	mustPanicContaining(t, "no primary key", func() { ForeignTable(&NoPKForeign{}) })
}

func TestForeignTable_NonStruct_Panics(t *testing.T) {
	mustPanicContaining(t, "must be a struct", func() { ForeignTable(42) })
	mustPanicContaining(t, "must not be nil", func() { ForeignTable(nil) })
}

func TestForeignTable_ChokModel_PanicsPointingToTable(t *testing.T) {
	// Full model: has its own door with full validation.
	mustPanicContaining(t, "db.Table", func() { ForeignTable(&TestItem{}) })
	// Append-only model: same.
	mustPanicContaining(t, "db.Table", func() { ForeignTable(&AuditSample{}) })
}

func TestTable_ForeignShape_PanicsPointingToForeignTable(t *testing.T) {
	mustPanicContaining(t, "db.ForeignTable", func() { Table(&UserRoleJoin{}) })
}

// --- append-only models through db.Table ---

func TestTable_AppendOnlyModel_MigratesWithoutManagedColumns(t *testing.T) {
	gdb := openTestDB(t)
	if err := Migrate(context.Background(), gdb, Table(&AuditSample{})); err != nil {
		t.Fatalf("append-only migrate: %v", err)
	}
	m := gdb.Migrator()
	for _, col := range []string{"id", "created_at", "actor"} {
		if !m.HasColumn(&AuditSample{}, col) {
			t.Fatalf("append-only table must have column %q", col)
		}
	}
	// The full model's managed columns must NOT appear — their absence
	// is the point of the lightweight base.
	for _, col := range []string{"rid", "version", "updated_at", "deleted_at", "delete_token"} {
		if m.HasColumn(&AuditSample{}, col) {
			t.Fatalf("append-only table must not have column %q", col)
		}
	}
}

func TestMigrate_SoftUniqueOnAppendOnly_Error(t *testing.T) {
	gdb := openTestDB(t)
	err := Migrate(context.Background(), gdb, Table(&AuditSample{}, SoftUnique("uk_actor", "actor")))
	if err == nil {
		t.Fatal("SoftUnique on an append-only model must be rejected (no soft delete)")
	}
}

// --- ValidateAppendModel ---

func TestValidateAppendModel_Valid(t *testing.T) {
	if err := ValidateAppendModel(&AuditSample{}); err != nil {
		t.Fatalf("valid append model rejected: %v", err)
	}
}

func TestValidateAppendModel_Rejections(t *testing.T) {
	cases := []struct {
		name  string
		model any
		want  string
	}{
		{"non-struct", 42, "must be a struct"},
		{"missing embed", &struct{ Name string }{}, "must embed db.AppendOnlyModel"},
		{"full model", &TestItem{}, "must embed db.AppendOnlyModel"},
		{"both bases", &BothBases{}, "pick one base"},
		{"rid prefixer", &PrefixedAppend{}, "RIDPrefixer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAppendModel(tc.model)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestTable_AppendOnlyRIDPrefixer_Panics(t *testing.T) {
	mustPanicContaining(t, "RIDPrefixer", func() { Table(&PrefixedAppend{}) })
}
