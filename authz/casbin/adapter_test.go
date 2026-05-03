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
	build := func() Service {
		adapter, aerr := newGormAdapter(db)
		if aerr != nil {
			t.Fatal(aerr)
		}
		auth, aerr := newAuthorizer(rbacWithDomainsModel, adapter)
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

// TestBootstrap_BatchPath verifies the *casbinAuthorizer.grantRoleBatch
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
	auth, err := newAuthorizer(rbacWithDomainsModel, adapter)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Service(auth).(batchGranter); !ok {
		t.Fatal("*casbinAuthorizer must satisfy batchGranter for Bootstrap fast path")
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
// further events. Phase 6 doesn't wire a real audit producer (the
// Builder rejects AuditEnabled=true), but the storage primitive is
// the eventual integration point and must be race-free.
func TestWithAuditHook_AtomicSwap(t *testing.T) {
	db := newAdapterDB(t)
	adapter, err := newGormAdapter(db)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := newAuthorizer(rbacWithDomainsModel, adapter)
	if err != nil {
		t.Fatal(err)
	}
	var fired int
	auth.withAuditHook(func(_, _, _, _ string) { fired++ })
	if err := auth.GrantRole(context.Background(), "admin", "task", "read"); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Errorf("after GrantRole, fired = %d, want 1", fired)
	}
	auth.withAuditHook(nil)
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
