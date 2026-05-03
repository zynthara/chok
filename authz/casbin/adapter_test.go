package casbin

import (
	"testing"

	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/persist"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// internal_test.go: package-internal tests for the Adapter
// implementation. Public-facing test paths (Service / Bootstrap /
// Authorize) live in casbin_test.go (external _test package); this
// file exercises adapter-specific edge cases the external tests
// would have to reach through Casbin's enforcer abstractions to
// observe.

func newAdapterDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestNewGormAdapter_AutoMigratesTable verifies the adapter creates
// casbin_rule on first construction. Subsequent Init runs against the
// same DB must be idempotent (gorm.AutoMigrate handles that, but
// we pin the contract here so a future refactor doesn't accidentally
// move the migrate call out of newGormAdapter).
func TestNewGormAdapter_AutoMigratesTable(t *testing.T) {
	db := newAdapterDB(t)
	if _, err := newGormAdapter(db); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable("casbin_rule") {
		t.Fatal("newGormAdapter must AutoMigrate casbin_rule")
	}
	// Idempotent re-construction.
	if _, err := newGormAdapter(db); err != nil {
		t.Fatalf("re-construction should be idempotent, got %v", err)
	}
}

func TestNewGormAdapter_RejectsNilDB(t *testing.T) {
	if _, err := newGormAdapter(nil); err == nil {
		t.Fatal("expected error on nil *gorm.DB")
	}
}

// TestAddPolicy_RoundTrip drives AddPolicy + LoadPolicy through the
// adapter directly (no enforcer in the loop) so a bug in the rule
// serializer or the LoadPolicyLine call surfaces here. The other
// tests touch this through Casbin abstractions where a faulty
// adapter could be hidden by the enforcer's in-memory model.
func TestAddPolicy_RoundTrip(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}

	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	if err := a.AddPolicy("p", "p", []string{"bob", "ws_abc", "task", "write"}); err != nil {
		t.Fatal(err)
	}
	if err := a.AddPolicy("g", "g", []string{"carol", "admin", "*"}); err != nil {
		t.Fatal(err)
	}

	m := model.NewModel()
	if err := m.LoadModelFromText(rbacWithDomainsModel); err != nil {
		t.Fatal(err)
	}
	if err := a.LoadPolicy(m); err != nil {
		t.Fatal(err)
	}
	pPol := m["p"]["p"].Policy
	gPol := m["g"]["g"].Policy
	if len(pPol) != 2 {
		t.Fatalf("expected 2 p-rules, got %d (%v)", len(pPol), pPol)
	}
	if len(gPol) != 1 {
		t.Fatalf("expected 1 g-rule, got %d (%v)", len(gPol), gPol)
	}
	if pPol[0][0] != "alice" || pPol[0][1] != "*" {
		t.Fatalf("p-rule round-trip wrong: %v", pPol[0])
	}
	if gPol[0][2] != "*" {
		t.Fatalf("g-rule domain round-trip wrong: %v", gPol[0])
	}
}

// TestRemovePolicy_ExactMatch covers the "delete by full prefix"
// path. Adapter doesn't dedupe, so adding the same rule twice and
// removing once must leave one copy behind.
func TestRemovePolicy_ExactMatch(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	rule := []string{"alice", "*", "task", "read"}
	for range 2 {
		if err := a.AddPolicy("p", "p", rule); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.RemovePolicy("p", "p", rule); err != nil {
		t.Fatal(err)
	}
	// We don't assert exact remaining count via LoadPolicy; just
	// confirm an unrelated rule survives.
	if err := a.AddPolicy("p", "p", []string{"bob", "*", "task", "write"}); err != nil {
		t.Fatal(err)
	}

	m := model.NewModel()
	_ = m.LoadModelFromText(rbacWithDomainsModel)
	if err := a.LoadPolicy(m); err != nil {
		t.Fatal(err)
	}
	gotBob := false
	for _, r := range m["p"]["p"].Policy {
		if r[0] == "bob" {
			gotBob = true
		}
	}
	if !gotBob {
		t.Fatal("RemovePolicy should not have touched bob's rule")
	}
}

// TestRemoveFilteredPolicy_WildcardSlots covers the
// fieldValues[i]=="" wildcard semantics: an empty slot doesn't
// constrain the column. Casbin uses this to clear "all rules for
// this role regardless of object/action".
func TestRemoveFilteredPolicy_WildcardSlots(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range [][]string{
		{"admin", "*", "task", "read"},
		{"admin", "*", "task", "write"},
		{"admin", "ws_abc", "task", "read"},
		{"viewer", "*", "task", "read"},
	} {
		if err := a.AddPolicy("p", "p", r); err != nil {
			t.Fatal(err)
		}
	}
	// Remove every "admin" rule regardless of domain/obj/act.
	if err := a.RemoveFilteredPolicy("p", "p", 0, "admin"); err != nil {
		t.Fatal(err)
	}

	m := model.NewModel()
	_ = m.LoadModelFromText(rbacWithDomainsModel)
	if err := a.LoadPolicy(m); err != nil {
		t.Fatal(err)
	}
	for _, r := range m["p"]["p"].Policy {
		if r[0] == "admin" {
			t.Fatalf("admin rule survived RemoveFilteredPolicy: %v", r)
		}
	}
	if len(m["p"]["p"].Policy) != 1 || m["p"]["p"].Policy[0][0] != "viewer" {
		t.Fatalf("expected only viewer rule to remain, got %v", m["p"]["p"].Policy)
	}
}

// TestAddPolicies_Batch covers the persist.BatchAdapter optimisation
// path: SyncedEnforcer's Bootstrap-style setup uses AddPolicies to
// batch many rules in one transaction.
func TestAddPolicies_Batch(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ba, ok := a.(persist.BatchAdapter)
	if !ok {
		t.Fatal("adapter should implement persist.BatchAdapter")
	}
	rules := [][]string{
		{"admin", "*", "task", "*"},
		{"admin", "*", "audit", "read"},
		{"viewer", "*", "task", "read"},
	}
	if err := ba.AddPolicies("p", "p", rules); err != nil {
		t.Fatal(err)
	}

	m := model.NewModel()
	_ = m.LoadModelFromText(rbacWithDomainsModel)
	if err := a.LoadPolicy(m); err != nil {
		t.Fatal(err)
	}
	if got := len(m["p"]["p"].Policy); got != 3 {
		t.Fatalf("AddPolicies should have inserted 3 rows, got %d", got)
	}
}

// TestAddPolicies_Empty covers the edge case where Casbin invokes
// the batch entry with zero rules — must be a no-op, not an error.
func TestAddPolicies_Empty(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ba := a.(persist.BatchAdapter)
	if err := ba.AddPolicies("p", "p", nil); err != nil {
		t.Fatalf("empty batch should not error: %v", err)
	}
}

// TestSavePolicy_TruncateAndReplace mirrors gorm-adapter v3's
// SavePolicy semantics: existing rows are wiped, the in-memory model
// is serialised back. Tests this by adding rules through AddPolicy,
// loading into a fresh model, then SavePolicy with a different model.
func TestSavePolicy_TruncateAndReplace(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	// Seed.
	if err := a.AddPolicy("p", "p", []string{"old", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	// Build a fresh model carrying a single new rule and SavePolicy it.
	m := model.NewModel()
	if err := m.LoadModelFromText(rbacWithDomainsModel); err != nil {
		t.Fatal(err)
	}
	m["p"]["p"].Policy = [][]string{{"new", "*", "task", "*"}}
	if err := a.SavePolicy(m); err != nil {
		t.Fatal(err)
	}
	// Reload from disk and assert we only see the new rule.
	m2 := model.NewModel()
	_ = m2.LoadModelFromText(rbacWithDomainsModel)
	if err := a.LoadPolicy(m2); err != nil {
		t.Fatal(err)
	}
	if len(m2["p"]["p"].Policy) != 1 || m2["p"]["p"].Policy[0][0] != "new" {
		t.Fatalf("SavePolicy didn't truncate-and-replace, got %v", m2["p"]["p"].Policy)
	}
}

// TestFormatPolicyLine_TrimsTrailingEmpty covers the serialiser:
// internal empty Vn must stay (Casbin treats them as explicit
// fields), but trailing empty Vn must be elided so LoadPolicyLine
// reads "p, alice, *, task, read" rather than
// "p, alice, *, task, read, , ".
func TestFormatPolicyLine_TrimsTrailingEmpty(t *testing.T) {
	tests := []struct {
		name string
		row  CasbinRule
		want string
	}{
		{"all populated", CasbinRule{Ptype: "p", V0: "alice", V1: "*", V2: "task", V3: "read"}, "p, alice, *, task, read"},
		{"trailing empty", CasbinRule{Ptype: "p", V0: "alice", V1: "*", V2: "task", V3: "read", V4: "", V5: ""}, "p, alice, *, task, read"},
		{"empty in middle preserved", CasbinRule{Ptype: "p", V0: "alice", V1: "", V2: "task"}, "p, alice, , task"},
		{"only ptype", CasbinRule{Ptype: "p"}, "p"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatPolicyLine(tc.row)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
