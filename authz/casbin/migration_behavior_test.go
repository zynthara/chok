package casbin_test

import (
	"testing"

	"github.com/zynthara/chok/v2/authz"
	chokcasbin "github.com/zynthara/chok/v2/authz/casbin"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/internal/testseq"
)

func TestMigrationBehavior_AuthzUpgradeMatrix(t *testing.T) {
	runAuthzUpgradeMatrix(t, dbtest.Open)
}

func TestMigrationBehavior_MySQLAuthzUpgradeMatrix(t *testing.T) {
	runAuthzUpgradeMatrix(t, dbtest.OpenMySQL)
}

func runAuthzUpgradeMatrix(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()
	testseq.RunUpgradeMatrix(t, open, authz.MigrationSequence(), db.Sequence{}, testseq.UpgradeSpec{
		Trace: authzAdapterTrace,
		PrepareAdoptable: func(t testing.TB, h *db.DB) {
			t.Helper()
			if err := authz.MigrateSchema(t.Context(), h); err != nil {
				t.Fatalf("authz auto schema: %v", err)
			}
		},
	})
}

func authzAdapterTrace(t testing.TB, h *db.DB) testseq.Trace {
	t.Helper()
	gdb := h.Unsafe(t.Context())
	adapter, err := chokcasbin.NewAdapterForTest(gdb)
	if err != nil {
		t.Fatalf("authz behavior construct adapter: %v", err)
	}
	rule := []string{"matrix-role", "*", "matrix-object", "read"}
	if err := adapter.AddPolicy("p", "p", rule); err != nil {
		t.Fatalf("authz behavior first AddPolicy: %v", err)
	}
	if err := adapter.AddPolicy("p", "p", rule); err != nil {
		t.Fatalf("authz behavior duplicate AddPolicy: %v", err)
	}
	var rows int64
	if err := gdb.Model(&chokcasbin.CasbinRule{}).
		Where("ptype = ? AND v0 = ? AND v1 = ? AND v2 = ? AND v3 = ?", "p", rule[0], rule[1], rule[2], rule[3]).
		Count(&rows).Error; err != nil {
		t.Fatalf("authz behavior count policy rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("authz adapter duplicate row count = %d, want 1", rows)
	}
	return testseq.Trace{{Step: "casbin_adapter_duplicate", OK: true, Rows: rows}}
}
