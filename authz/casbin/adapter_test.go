package casbin

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
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
	// Schema creation moved out of the adapter constructor (M4: the
	// authz module's Migrate owns it) — tests ensure it here, playing
	// the module's role.
	if err := db.AutoMigrate(&CasbinRule{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestNewGormAdapter_RunsNoDDL pins the M4 contract inversion: the
// constructor must NOT create casbin_rule — schema creation belongs
// to the authz module's Migrate phase so the framework migrate mode
// (auto/versioned/off, SPEC §5.3) governs battery tables uniformly.
func TestNewGormAdapter_RunsNoDDL(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newGormAdapter(db); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasTable("casbin_rule") {
		t.Fatal("newGormAdapter must not run DDL — table creation belongs to the module Migrate phase")
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

// TestRemoveFilteredPolicy_AllEmpty_Rejected pins the safety guard:
// calling RemoveFilteredPolicy with no constraints (or all-empty
// fieldValues) must NOT silently delete every row of the ptype.
// Casbin's RBAC API never calls in this shape — even DeleteRolesForUser
// supplies the user as v0 — so the adapter rejects rather than risk a
// data-loss footgun. Operators wanting a bulk clear should use
// SavePolicy with an empty model.
func TestRemoveFilteredPolicy_AllEmpty_Rejected(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range [][]string{
		{"admin", "*", "task", "read"},
		{"viewer", "*", "task", "read"},
	} {
		if err := a.AddPolicy("p", "p", r); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name        string
		fieldIndex  int
		fieldValues []string
	}{
		{"zero values", 0, nil},
		{"all empty 4", 0, []string{"", "", "", ""}},
		{"all empty 6", 0, []string{"", "", "", "", "", ""}},
		{"all empty starting at 1", 1, []string{"", "", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := a.RemoveFilteredPolicy("p", "p", tc.fieldIndex, tc.fieldValues...)
			if err == nil {
				t.Fatal("expected refusal error for all-empty filter")
			}
			if !strings.Contains(err.Error(), "refusing to delete all rows") {
				t.Fatalf("expected refusal message, got %v", err)
			}
			var count int64
			if err := db.Model(&CasbinRule{}).Where("ptype = ?", "p").Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 2 {
				t.Errorf("rows survived check: count = %d, want 2 (rejection must not have deleted anything)", count)
			}
		})
	}
}

// TestRemoveFilteredPolicy_PartialEmpty_KeepsCasbinSemantics verifies
// the guard does NOT regress Casbin's documented "empty values are
// wildcards" semantics — the legitimate path where at least one
// constraint is supplied still works. DeleteRolesForUserInDomain
// translates to RemoveFilteredGroupingPolicy(0, user, "", domain),
// which reaches the adapter as ["alice", "", "ws_abc"] — middle slot
// empty, but not all-empty.
func TestRemoveFilteredPolicy_PartialEmpty_KeepsCasbinSemantics(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range [][]string{
		{"alice", "admin", "ws_abc"},
		{"alice", "viewer", "ws_abc"},
		{"alice", "admin", "ws_def"},
		{"bob", "admin", "ws_abc"},
	} {
		if err := a.AddPolicy("g", "g", r); err != nil {
			t.Fatal(err)
		}
	}
	// Delete every alice grouping in ws_abc regardless of role.
	if err := a.RemoveFilteredPolicy("g", "g", 0, "alice", "", "ws_abc"); err != nil {
		t.Fatal(err)
	}
	m := model.NewModel()
	_ = m.LoadModelFromText(rbacWithDomainsModel)
	if err := a.LoadPolicy(m); err != nil {
		t.Fatal(err)
	}
	for _, r := range m["g"]["g"].Policy {
		if r[0] == "alice" && r[2] == "ws_abc" {
			t.Errorf("alice/ws_abc grouping survived: %v", r)
		}
	}
	// alice/ws_def and bob/ws_abc must still be there.
	survivors := map[string]bool{}
	for _, r := range m["g"]["g"].Policy {
		survivors[r[0]+"|"+r[2]] = true
	}
	if !survivors["alice|ws_def"] || !survivors["bob|ws_abc"] {
		t.Errorf("unrelated rules dropped: %+v", m["g"]["g"].Policy)
	}
}

// TestRemoveFilteredPolicy_FieldIndexOutOfRange_Rejected pins the
// guard mapping fix (round-3 review). Round-2's hasAnyConstraint only
// checked "any non-empty value", not "any value mapped to a real
// V0..V5 column". With fieldIndex=6 + a non-empty fieldValues entry,
// the guard let the call through, applyValueColumnsFiltered
// immediately broke (idx >= len(cols)), and the WHERE collapsed to
// ptype=? — silently deleting every row of the ptype.
// hasAnyMappedConstraint now requires the value to land in V0..V5.
func TestRemoveFilteredPolicy_FieldIndexOutOfRange_Rejected(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	if err := a.AddPolicy("p", "p", []string{"bob", "*", "task", "write"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name        string
		fieldIndex  int
		fieldValues []string
	}{
		{"index past V5", 6, []string{"x"}},
		{"index way past", 99, []string{"x", "y"}},
		{"negative index", -1, []string{"x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := a.RemoveFilteredPolicy("p", "p", tc.fieldIndex, tc.fieldValues...)
			if err == nil {
				t.Fatal("expected refusal for out-of-range fieldIndex")
			}
			var count int64
			if err := db.Model(&CasbinRule{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 2 {
				t.Errorf("guard violated: rows deleted despite refusal, count = %d", count)
			}
		})
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

// TestUpdatableAdapter_Contract proves the adapter satisfies the
// persist.UpdatableAdapter interface. Casbin v3's enforcer.UpdatePolicy
// path does a hard type assertion (internal_api.go:171) and panics
// when the adapter is missing this — chok's Service doesn't yet expose
// Update*, but the contract has to be honoured up front so any future
// extension or watcher path doesn't crash.
func TestUpdatableAdapter_Contract(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.(persist.UpdatableAdapter); !ok {
		t.Fatal("gormAdapter must satisfy persist.UpdatableAdapter")
	}
}

// TestUpdatePolicy_ReplacesMatchingRule exercises the happy path:
// the existing oldRule is deleted and the newRule inserted in one
// transaction; the unrelated bob row is left untouched. Pinning the
// post-update state catches any regression where Update degenerates
// to a prefix delete (e.g. drops every alice row regardless of
// v3) or skips the new-row insert.
//
// Renamed from TestUpdatePolicy_AtomicReplace (round-3 review): the
// old name claimed atomicity, but the test only asserts the final
// state of a happy-path call — no concurrent observer to verify
// in-tx invisibility. Match TestSavePolicy_RejectsOversize's
// result-semantic naming convention.
func TestUpdatePolicy_ReplacesMatchingRule(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ua := a.(persist.UpdatableAdapter)
	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	if err := a.AddPolicy("p", "p", []string{"bob", "*", "task", "write"}); err != nil {
		t.Fatal(err)
	}
	if err := ua.UpdatePolicy("p", "p",
		[]string{"alice", "*", "task", "read"},
		[]string{"alice", "*", "task", "delete"}); err != nil {
		t.Fatal(err)
	}

	m := model.NewModel()
	_ = m.LoadModelFromText(rbacWithDomainsModel)
	if err := a.LoadPolicy(m); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range m["p"]["p"].Policy {
		got[r[0]+"|"+r[3]] = true
	}
	if got["alice|read"] {
		t.Error("old alice/read rule should have been deleted")
	}
	if !got["alice|delete"] {
		t.Error("new alice/delete rule should have been inserted")
	}
	if !got["bob|write"] {
		t.Error("unrelated bob rule should not have been touched")
	}
}

// TestUpdatePolicy_RejectsOversizeNewRule covers the UpdatePolicy
// fast-fail when newRule has 7+ columns. ruleToRow returns an error
// before the transaction opens, so no DB write occurs and the
// pre-existing oldRule remains intact.
//
// Renamed from TestUpdatePolicy_Rollback (round-3 review): the old
// name implied transaction rollback, but the validation runs before
// the tx opens — same fix style as TestSavePolicy_RejectsOversize.
// Real rollback coverage lives in TestUpdatePolicy_RollbackOnUniqueConflict.
func TestUpdatePolicy_RejectsOversizeNewRule(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ua := a.(persist.UpdatableAdapter)
	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	err = ua.UpdatePolicy("p", "p",
		[]string{"alice", "*", "task", "read"},
		[]string{"a", "b", "c", "d", "e", "f", "g"}) // 7 cols, oversize
	if err == nil {
		t.Fatal("expected error for oversize newRule")
	}
	var count int64
	if err := db.Model(&CasbinRule{}).Where("ptype = ?", "p").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 row (no DB write before validation failed), got %d", count)
	}
}

// TestUpdatePolicy_ExactRule_PreservesUnrelatedRowsWithSamePrefix
// pins the High-severity round-3 finding: UpdatePolicy must use
// exact-rule matching, not prefix-match. Two stored rows share
// v0..v3 but differ at v4 — a prefix-WHERE on a 4-col oldRule would
// delete BOTH rows; applyExactRule constrains v4="" and v5="" too,
// so only the 4-col stored row matches.
func TestUpdatePolicy_ExactRule_PreservesUnrelatedRowsWithSamePrefix(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ua := a.(persist.UpdatableAdapter)

	// Direct DB inserts so we can control v4 — Casbin AddPolicy in a
	// 4-token model wouldn't write v4. The reviewer's scenario is "a
	// store that has both 4-col and 5-col rows for the same ptype",
	// which can arise from operator SQL or migration from a
	// variable-arity adapter.
	if err := db.Create(&CasbinRule{Ptype: "p", V0: "alice", V1: "*", V2: "task", V3: "read"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&CasbinRule{Ptype: "p", V0: "alice", V1: "*", V2: "task", V3: "read", V4: "dom1"}).Error; err != nil {
		t.Fatal(err)
	}

	if err := ua.UpdatePolicy("p", "p",
		[]string{"alice", "*", "task", "read"}, // 4-col oldRule
		[]string{"alice", "*", "task", "delete"}); err != nil {
		t.Fatal(err)
	}

	// The 5-col row (v4=dom1) must still be present. Under the
	// previous prefix-WHERE implementation it was lost.
	var domRow int64
	if err := db.Model(&CasbinRule{}).
		Where("v0 = ? AND v3 = ? AND v4 = ?", "alice", "read", "dom1").
		Count(&domRow).Error; err != nil {
		t.Fatal(err)
	}
	if domRow != 1 {
		t.Errorf("dom1 row was lost to prefix-delete: got %d, want 1", domRow)
	}
	// And the new 4-col row must exist.
	var newRow int64
	if err := db.Model(&CasbinRule{}).
		Where("v0 = ? AND v3 = ? AND v4 = ?", "alice", "delete", "").
		Count(&newRow).Error; err != nil {
		t.Fatal(err)
	}
	if newRow != 1 {
		t.Errorf("new alice/delete row missing: got %d, want 1", newRow)
	}
}

// TestUpdatePolicy_RejectsZeroMatch covers the contract:
// model.UpdatePolicy returns false on no-match, so the adapter must
// also refuse rather than insert newRule "for free". Without the
// RowsAffected==0 check, the DB would gain newRule while the model
// state was unchanged — silent divergence.
func TestUpdatePolicy_RejectsZeroMatch(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ua := a.(persist.UpdatableAdapter)
	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	err = ua.UpdatePolicy("p", "p",
		[]string{"ghost", "*", "task", "read"}, // not in DB
		[]string{"ghost", "*", "task", "write"})
	if err == nil {
		t.Fatal("expected error for zero-match oldRule")
	}
	// Insert must NOT have happened.
	var ghostRows int64
	if err := db.Model(&CasbinRule{}).Where("v0 = ?", "ghost").Count(&ghostRows).Error; err != nil {
		t.Fatal(err)
	}
	if ghostRows != 0 {
		t.Errorf("ghost row was inserted despite zero-match: %d rows", ghostRows)
	}
	// alice row untouched.
	var aliceRows int64
	if err := db.Model(&CasbinRule{}).Where("v0 = ?", "alice").Count(&aliceRows).Error; err != nil {
		t.Fatal(err)
	}
	if aliceRows != 1 {
		t.Errorf("alice row should be intact, got %d", aliceRows)
	}
}

// TestUpdatePolicy_RollbackOnUniqueConflict covers a real
// transaction rollback: the targeted Delete succeeds inside the tx,
// but the subsequent Insert collides with an unrelated row's
// (ptype, v0..v5) tuple — the tx rolls back and the supposedly-
// deleted row reappears. UpdatePolicy deliberately does NOT use
// OnConflict{DoNothing:true} (Update means replace, not merge), so
// the conflict surfaces as an error to the caller.
func TestUpdatePolicy_RollbackOnUniqueConflict(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ua := a.(persist.UpdatableAdapter)
	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	if err := a.AddPolicy("p", "p", []string{"bob", "*", "task", "write"}); err != nil {
		t.Fatal(err)
	}
	// Try to "update" alice/read into bob/write — newRule already
	// exists, so the insert hits the unique index.
	err = ua.UpdatePolicy("p", "p",
		[]string{"alice", "*", "task", "read"},
		[]string{"bob", "*", "task", "write"})
	if err == nil {
		t.Fatal("expected unique-conflict error, got nil")
	}
	// Both rows must remain (tx rolled back the alice delete).
	var count int64
	if err := db.Model(&CasbinRule{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("rollback failed: got %d rows, want 2 (both alice and bob should remain)", count)
	}
	var aliceCount int64
	if err := db.Model(&CasbinRule{}).Where("v0 = ? AND v3 = ?", "alice", "read").Count(&aliceCount).Error; err != nil {
		t.Fatal(err)
	}
	if aliceCount != 1 {
		t.Errorf("alice/read row missing post-rollback: got %d, want 1", aliceCount)
	}
}

// TestUpdatePolicies_Batch tests N→N atomic replacement.
func TestUpdatePolicies_Batch(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ua := a.(persist.UpdatableAdapter)
	for _, r := range [][]string{
		{"alice", "*", "task", "read"},
		{"alice", "*", "task", "write"},
	} {
		if err := a.AddPolicy("p", "p", r); err != nil {
			t.Fatal(err)
		}
	}
	old := [][]string{
		{"alice", "*", "task", "read"},
		{"alice", "*", "task", "write"},
	}
	new := [][]string{
		{"alice", "*", "task", "view"},
		{"alice", "*", "task", "edit"},
	}
	if err := ua.UpdatePolicies("p", "p", old, new); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&CasbinRule{}).Where("ptype = ? AND v0 = ?", "p", "alice").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("expected 2 alice rows post-update, got %d", count)
	}
}

// TestUpdatePolicies_LengthMismatch_Rejected pins Casbin's "old and
// new lengths must match" invariant — the adapter checks defensively
// even though the enforcer also checks (internal_api.go:202-204).
func TestUpdatePolicies_LengthMismatch_Rejected(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ua := a.(persist.UpdatableAdapter)
	err = ua.UpdatePolicies("p", "p",
		[][]string{{"alice", "*", "task", "read"}},
		[][]string{{"a", "*", "x", "1"}, {"b", "*", "y", "2"}})
	if err == nil {
		t.Fatal("expected error for mismatched lengths")
	}
}

// TestUpdatePolicies_PartialMiss_Rollback pins the batch contract:
// any oldRule that doesn't match a stored row aborts the entire
// transaction so the store stays consistent with model.UpdatePolicies
// (which only succeeds when every rule was found). The first oldRule
// matches and gets deleted inside the tx; the second misses, the
// adapter returns an error, and the tx rolls back so the first row
// reappears.
func TestUpdatePolicies_PartialMiss_Rollback(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ua := a.(persist.UpdatableAdapter)
	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	// Two oldRules: first matches, second is a ghost. Without
	// per-element RowsAffected==0 → rollback, the alice row would
	// be deleted and the bob/charlie new rows inserted, even though
	// the batch was partially incoherent.
	err = ua.UpdatePolicies("p", "p",
		[][]string{
			{"alice", "*", "task", "read"},
			{"ghost", "*", "task", "read"},
		},
		[][]string{
			{"alice", "*", "task", "view"},
			{"ghost", "*", "task", "view"},
		})
	if err == nil {
		t.Fatal("expected error for partial-miss batch")
	}
	// alice row must still be present (tx rolled back).
	var count int64
	if err := db.Model(&CasbinRule{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("rollback failed: got %d rows, want 1", count)
	}
	var aliceRead int64
	if err := db.Model(&CasbinRule{}).
		Where("v0 = ? AND v3 = ?", "alice", "read").
		Count(&aliceRead).Error; err != nil {
		t.Fatal(err)
	}
	if aliceRead != 1 {
		t.Errorf("alice/read row should be intact post-rollback, got %d", aliceRead)
	}
}

// TestUpdateFilteredPolicies_RoundTrip exercises the find→delete→insert
// cycle and verifies the returned old-rules slice matches what was
// removed (callers / watchers depend on this for sync).
func TestUpdateFilteredPolicies_RoundTrip(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ua := a.(persist.UpdatableAdapter)
	for _, r := range [][]string{
		{"admin", "*", "task", "read"},
		{"admin", "*", "task", "write"},
		{"viewer", "*", "task", "read"},
	} {
		if err := a.AddPolicy("p", "p", r); err != nil {
			t.Fatal(err)
		}
	}
	// Replace every "admin" rule with two new ones.
	newRules := [][]string{
		{"admin", "*", "audit", "read"},
		{"admin", "*", "user", "list"},
	}
	old, err := ua.UpdateFilteredPolicies("p", "p", newRules, 0, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 2 {
		t.Errorf("expected 2 old rules returned, got %d (%+v)", len(old), old)
	}
	for _, r := range old {
		if r[0] != "admin" {
			t.Errorf("returned rule should belong to admin, got %v", r)
		}
	}
	m := model.NewModel()
	_ = m.LoadModelFromText(rbacWithDomainsModel)
	if err := a.LoadPolicy(m); err != nil {
		t.Fatal(err)
	}
	if got := len(m["p"]["p"].Policy); got != 3 {
		t.Errorf("post-update row count = %d, want 3 (2 new admin + 1 viewer)", got)
	}
	// viewer must survive untouched.
	survived := false
	for _, r := range m["p"]["p"].Policy {
		if r[0] == "viewer" {
			survived = true
		}
	}
	if !survived {
		t.Error("unrelated viewer rule was deleted by filter")
	}
}

// TestUpdateFilteredPolicies_AllEmpty_Rejected mirrors the
// RemoveFilteredPolicy guard: a filter with no constraints would
// "replace every row of this ptype with newRules", which is a
// data-loss footgun. Refuse instead.
func TestUpdateFilteredPolicies_AllEmpty_Rejected(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ua := a.(persist.UpdatableAdapter)
	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	_, err = ua.UpdateFilteredPolicies("p", "p",
		[][]string{{"x", "*", "y", "z"}}, 0, "", "", "", "")
	if err == nil {
		t.Fatal("expected refusal for all-empty fieldValues")
	}
	if !strings.Contains(err.Error(), "refusing to replace all rows") {
		t.Fatalf("expected refusal message, got %v", err)
	}
	var count int64
	if err := db.Model(&CasbinRule{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("guard violated: count = %d, want 1 (rejection must not have written or deleted)", count)
	}
}

// TestUpdateFilteredPolicies_ZeroHit_NonEmptyNewRules_Rejected pins
// the round-3 divergence guard: when the filter matches no stored
// rules but newRules is non-empty, Casbin v3.10.0's upper layer
// (internal_api.go:317-374) treats the empty oldRules return as "no
// rule changed" and skips watcher notification, so peer instances
// never learn about the inserted newRules. To avoid that silent
// multi-instance divergence the adapter refuses the call — operators
// wanting a pure insert should use AddPolicies instead.
func TestUpdateFilteredPolicies_ZeroHit_NonEmptyNewRules_Rejected(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ua := a.(persist.UpdatableAdapter)
	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	old, err := ua.UpdateFilteredPolicies("p", "p",
		[][]string{{"new", "*", "task", "read"}},
		0, "ghost") // filter matches nothing
	if err == nil {
		t.Fatal("expected error when filter matches nothing but newRules non-empty")
	}
	if old != nil {
		t.Errorf("expected nil oldRules on rejection, got %v", old)
	}
	if !strings.Contains(err.Error(), "filter matched no rules") {
		t.Fatalf("expected divergence-guard message, got %v", err)
	}
	// alice still present, no "new" row inserted.
	var count, newCount int64
	if err := db.Model(&CasbinRule{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 row (alice still present), got %d", count)
	}
	if err := db.Model(&CasbinRule{}).Where("v0 = ?", "new").Count(&newCount).Error; err != nil {
		t.Fatal(err)
	}
	if newCount != 0 {
		t.Errorf("new row was inserted despite zero-hit refusal: %d rows", newCount)
	}
}

// TestUpdateFilteredPolicies_ZeroHit_EmptyNewRules_NoOp covers the
// adjacent legitimate case: zero-hit + empty newRules. This is "try
// to delete rules matching the filter, there are none, also nothing
// to insert" — a benign no-op. We must NOT refuse this, because a
// caller using the filtered path purely for delete-by-filter would
// otherwise see a confusing error on an empty match.
func TestUpdateFilteredPolicies_ZeroHit_EmptyNewRules_NoOp(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	ua := a.(persist.UpdatableAdapter)
	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	old, err := ua.UpdateFilteredPolicies("p", "p",
		[][]string{}, 0, "ghost")
	if err != nil {
		t.Fatalf("zero-hit + empty newRules should be a no-op, got %v", err)
	}
	if len(old) != 0 {
		t.Errorf("expected empty oldRules, got %v", old)
	}
}

// TestLoadPolicy_RejectsEmptyPtype pins the round-3 LoadPolicy panic
// guard: a row with empty Ptype would have caused
// persist.LoadPolicyArray to panic at key[:1] (slice bounds out of
// range). Production AddPolicy paths always set Ptype, but the
// column has no NOT NULL — operator SQL or import from another store
// could leave one. The adapter now returns a structured error with
// the row id rather than crashing on startup.
func TestLoadPolicy_RejectsEmptyPtype(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&CasbinRule{Ptype: "", V0: "alice"}).Error; err != nil {
		t.Fatal(err)
	}
	m := model.NewModel()
	if err := m.LoadModelFromText(rbacWithDomainsModel); err != nil {
		t.Fatal(err)
	}
	err = a.LoadPolicy(m)
	if err == nil {
		t.Fatal("expected error for empty-Ptype row")
	}
	if !strings.Contains(err.Error(), "empty Ptype") {
		t.Errorf("expected 'empty Ptype' message, got %v", err)
	}
	if !strings.Contains(err.Error(), "row id=") {
		t.Errorf("expected error to carry row id for operator triage, got %v", err)
	}
}

// TestRowToRule_TrimsTrailingEmpty pins the slice projection used by
// LoadPolicy: stored CasbinRule values come back as a slice with
// trailing empties elided so the in-memory model's policy section
// matches the shape AddPolicy originally wrote (and what callers see
// via GetPolicy / GetFilteredPolicy).
func TestRowToRule_TrimsTrailingEmpty(t *testing.T) {
	tests := []struct {
		name string
		row  CasbinRule
		want []string
	}{
		{"all populated", CasbinRule{V0: "a", V1: "b", V2: "c", V3: "d"}, []string{"a", "b", "c", "d"}},
		{"trailing empty trimmed", CasbinRule{V0: "a", V1: "b", V2: "", V3: ""}, []string{"a", "b"}},
		{"empty middle preserved", CasbinRule{V0: "a", V1: "", V2: "c"}, []string{"a", "", "c"}},
		{"completely empty", CasbinRule{}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rowToRule(tc.row)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
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

// TestRuleToRow_RejectsOversize verifies the boundary check that
// stops custom Options.Model with policy width > V0..V5 from silently
// truncating data on AddPolicy / SavePolicy. Any rule longer than 6
// must error rather than store a corrupted prefix.
func TestRuleToRow_RejectsOversize(t *testing.T) {
	cases := [][]string{
		{"a", "b", "c", "d", "e", "f", "g"},                // 7 cols
		{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}, // way over
	}
	for _, c := range cases {
		_, err := ruleToRow("p", c)
		if err == nil {
			t.Errorf("expected error for %d-col rule, got nil", len(c))
			continue
		}
		if !strings.Contains(err.Error(), "max supported is 6") {
			t.Errorf("expected 'max supported is 6' message, got %q", err)
		}
	}
}

// TestRuleToRow_AcceptsBoundary covers the 6-col limit (max storage
// width). Failing this would mean the boundary check is too strict.
func TestRuleToRow_AcceptsBoundary(t *testing.T) {
	r, err := ruleToRow("p", []string{"a", "b", "c", "d", "e", "f"})
	if err != nil {
		t.Fatalf("6-col rule should be accepted, got %v", err)
	}
	if r.V5 != "f" {
		t.Errorf("V5 = %q, want %q", r.V5, "f")
	}
}

// TestSavePolicy_RejectsOversize covers the SavePolicy fast-fail when
// the model carries a 7+ col policy. Without the check the SavePolicy
// transaction would commit truncated rows that would not round-trip.
func TestSavePolicy_RejectsOversize(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	m := model.NewModel()
	if err := m.LoadModelFromText(rbacWithDomainsModel); err != nil {
		t.Fatal(err)
	}
	m["p"]["p"].Policy = [][]string{{"a", "b", "c", "d", "e", "f", "g"}}
	if err := a.SavePolicy(m); err == nil {
		t.Fatal("expected error on oversize policy")
	}
	// Pre-existing rows must remain (transaction rolled back).
	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&CasbinRule{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("rollback failed, expected 1 row, got %d", count)
	}
}

// TestSavePolicy_EmptyModel_TruncatesTable covers the documented
// "empty rules → bulk-replace clears the table" semantics. The
// current impl returns nil after the Delete; the test pins this
// behaviour so a refactor doesn't accidentally turn empty save into
// "leave existing rows alone".
func TestSavePolicy_EmptyModel_TruncatesTable(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	m := model.NewModel()
	if err := m.LoadModelFromText(rbacWithDomainsModel); err != nil {
		t.Fatal(err)
	}
	// m has no policies; SavePolicy should clear the table.
	if err := a.SavePolicy(m); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&CasbinRule{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected empty table after SavePolicy(empty), got %d rows", count)
	}
}

// TestAdapter_UniqueIndex_DuplicateInsert verifies the chok adapter
// rejects (or silently ignores via OnConflict) duplicate (ptype, V0..V5)
// tuples at the database layer. Without this guarantee, multi-instance
// Bootstrap leaves duplicate rows in casbin_rule that LoadPolicy then
// has to dedupe at runtime.
func TestAdapter_UniqueIndex_DuplicateInsert(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	rule := []string{"alice", "*", "task", "read"}
	for i := range 3 {
		if err := a.AddPolicy("p", "p", rule); err != nil {
			t.Fatalf("AddPolicy iteration %d: %v", i, err)
		}
	}
	var count int64
	if err := db.Model(&CasbinRule{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("3 AddPolicy of identical rule should leave 1 row, got %d", count)
	}
}

// TestAdapter_AddPolicies_DuplicateInBatch covers two dedupe paths:
// (a) duplicate rule inside the same batch slice, (b) duplicate of a
// pre-existing row. Both must converge on a single row.
func TestAdapter_AddPolicies_DuplicateInBatch(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AddPolicy("p", "p", []string{"alice", "*", "task", "read"}); err != nil {
		t.Fatal(err)
	}
	ba := a.(persist.BatchAdapter)
	rules := [][]string{
		{"alice", "*", "task", "read"},   // dup of pre-existing
		{"alice", "*", "task", "read"},   // dup inside batch
		{"bob", "*", "task", "write"},    // new
		{"alice", "*", "task", "delete"}, // new
	}
	if err := ba.AddPolicies("p", "p", rules); err != nil {
		t.Fatalf("batch AddPolicies should not error on duplicates: %v", err)
	}
	var count int64
	if err := db.Model(&CasbinRule{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		var rows []CasbinRule
		_ = db.Find(&rows).Error
		t.Errorf("expected 3 distinct rows, got %d (rows: %+v)", count, rows)
	}
}

// TestAdapter_MultiInstance_BootstrapIdempotent simulates two
// independent enforcer instances over the same DB (a multi-pod
// deployment) Bootstrapping concurrently. With the unique index +
// OnConflict{DoNothing:true}, the final state must have exactly one
// (g, usr_root, admin, *) and one (p, admin, *, *, *) row regardless
// of which instance won the race for either INSERT.
//
// Uses a temp file because SQLite ":memory:" gives each *sql.DB
// connection its own private database, which would defeat the
// "shared storage" premise of this test.
func TestAdapter_MultiInstance_BootstrapIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "casbin.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	// Schema up-front: the adapter no longer runs DDL (module Migrate
	// owns it), so the test ensures the shared table before the race.
	if err := db.AutoMigrate(&CasbinRule{}); err != nil {
		t.Fatal(err)
	}
	build := func() Service {
		adapter, aerr := newGormAdapter(db)
		if aerr != nil {
			t.Fatal(aerr)
		}
		auth, aerr := newAuthorizer(rbacWithDomainsModel, adapter, nil)
		if aerr != nil {
			t.Fatal(aerr)
		}
		return auth
	}
	svc1 := build()
	svc2 := build()
	cfg := BootstrapConfig{AdminUserID: "usr_root"}
	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() { defer wg.Done(); err1 = Bootstrap(context.Background(), svc1, cfg) }()
	go func() { defer wg.Done(); err2 = Bootstrap(context.Background(), svc2, cfg) }()
	wg.Wait()
	if err1 != nil {
		t.Errorf("svc1 Bootstrap: %v", err1)
	}
	if err2 != nil {
		t.Errorf("svc2 Bootstrap: %v", err2)
	}

	var rows []CasbinRule
	if err := db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	gCount, pCount := 0, 0
	for _, r := range rows {
		if r.Ptype == "g" && r.V0 == "usr_root" {
			gCount++
		}
		if r.Ptype == "p" && r.V0 == "admin" {
			pCount++
		}
	}
	if gCount != 1 {
		t.Errorf("g(usr_root, admin, *) count = %d, want 1 (rows: %+v)", gCount, rows)
	}
	if pCount != 1 {
		t.Errorf("p(admin, *, *, *) count = %d, want 1 (rows: %+v)", pCount, rows)
	}
}

// TestBootstrap_BatchPath verifies the *Engine.grantRoleBatch
// fast path is exercised when Bootstrap's Service argument satisfies
// batchGranter (the chok-shipped enforcer does). The functional check
// is "all perms persisted in one go"; we can't directly count round-
// trips from inside the test, but the type assertion path is the
// invariant — without it we'd fall back to the per-perm GrantRole loop.
func TestBootstrap_BatchPath(t *testing.T) {
	db := newAdapterDB(t)
	adapter, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := newAuthorizer(rbacWithDomainsModel, adapter, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Service(auth).(batchGranter); !ok {
		t.Fatal("*Engine must satisfy batchGranter for Bootstrap fast path")
	}
	cfg := BootstrapConfig{
		AdminUserID: "usr_root",
		AdminPerms: []Permission{
			{Object: "task", Action: "read"},
			{Object: "task", Action: "write"},
			{Object: "audit", Action: "read"},
			{Object: "user", Action: "list"},
		},
	}
	if err := Bootstrap(context.Background(), auth, cfg); err != nil {
		t.Fatal(err)
	}
	var pCount int64
	if err := db.Model(&CasbinRule{}).Where("ptype = ? AND v0 = ?", "p", "admin").
		Count(&pCount).Error; err != nil {
		t.Fatal(err)
	}
	if pCount != 4 {
		t.Errorf("expected 4 admin policy rows, got %d", pCount)
	}
	// Re-running Bootstrap with same cfg must remain idempotent —
	// unique index makes it a no-op even via the batch path.
	if err := Bootstrap(context.Background(), auth, cfg); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&CasbinRule{}).Where("ptype = ? AND v0 = ?", "p", "admin").
		Count(&pCount).Error; err != nil {
		t.Fatal(err)
	}
	if pCount != 4 {
		t.Errorf("after re-Bootstrap, expected 4 admin policy rows, got %d", pCount)
	}
}

// TestWithAuditHook_AtomicSwap exercises the atomic.Pointer auditFn:
// installing a hook fires it once per mutation; clearing it stops
// further events. The authz module is the real producer (M4 7.E);
// the storage primitive must stay race-free regardless.
func TestWithAuditHook_AtomicSwap(t *testing.T) {
	db := newAdapterDB(t)
	adapter, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := newAuthorizer(rbacWithDomainsModel, adapter, nil)
	if err != nil {
		t.Fatal(err)
	}
	var fired int
	auth.AttachAuditHook(func(_ context.Context, _, _, _, _ string) { fired++ })
	if err := auth.GrantRole(context.Background(), "admin", "task", "read"); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Errorf("after GrantRole, fired = %d, want 1", fired)
	}
	auth.AttachAuditHook(nil)
	if err := auth.GrantRole(context.Background(), "admin", "task", "write"); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Errorf("after nil-swap, fired = %d, want 1 (no new fires)", fired)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}
	if err := auth.GrantRole(context.Background(), "admin", "audit", "read"); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Errorf("after Close, fired = %d, want 1 (Close also clears hook)", fired)
	}
}

// TestLoadPolicy_PreservesDelimiterCharsInFields pins the round-trip
// invariant LoadPolicy MUST hold: any byte AddPolicy stored in v0..v5
// must come back identical, regardless of whether it contains the
// adapter's previous CSV delimiter (", "), an embedded quote, or
// leading whitespace. The earlier impl rebuilt rows into a CSV string
// and re-parsed via persist.LoadPolicyLine, so a subject like
// "task,delete" was mis-split into two fields and the load either
// corrupted the model or failed with an arity mismatch at startup.
// Switching to persist.LoadPolicyArray (no parsing) eliminates the
// failure mode; this test makes sure no future refactor reintroduces
// CSV rebuild on the load path.
func TestLoadPolicy_PreservesDelimiterCharsInFields(t *testing.T) {
	db := newAdapterDB(t)
	a, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	rules := [][]string{
		{`alice, bob`, "*", `task,delete`, `read"`},
		{"  carol  ", "*", "task", " write "},
		{"dave", "*", `obj"with"quotes`, "act"},
	}
	for _, r := range rules {
		if err := a.AddPolicy("p", "p", r); err != nil {
			t.Fatal(err)
		}
	}

	m := model.NewModel()
	if err := m.LoadModelFromText(rbacWithDomainsModel); err != nil {
		t.Fatal(err)
	}
	if err := a.LoadPolicy(m); err != nil {
		t.Fatal(err)
	}
	pol := m["p"]["p"].Policy
	if len(pol) != len(rules) {
		t.Fatalf("expected %d rules, got %d (CSV mis-split would inflate / arity-fail this): %v", len(rules), len(pol), pol)
	}
	// Order("id") in LoadPolicy preserves insertion order.
	for i, want := range rules {
		got := pol[i]
		if len(got) != len(want) {
			t.Errorf("rule %d field count: got %d (%v), want %d (%v)", i, len(got), got, len(want), want)
			continue
		}
		for j := range want {
			if got[j] != want[j] {
				t.Errorf("rule %d field %d: got %q, want %q", i, j, got[j], want[j])
			}
		}
	}
}
