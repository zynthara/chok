package audit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/audit"
	"github.com/zynthara/chok/v2/auth"
	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/internal/testschema"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/middleware"
	"github.com/zynthara/chok/v2/scheduler"
)

// The audit component IS the sink shape authz consumes structurally
// when casbin.audit_enabled=true — drift breaks 7.E at compile time.
var _ authz.AuditSink = (*audit.Component)(nil)

const auditYAML = `
db:
  driver: sqlite
  sqlite:
    path: ":memory:"
audit:
  enabled: true
`

func TestModule_DisabledByDefault(t *testing.T) {
	// SPEC §6 pins the v1 opt-in default for audit: assembling the
	// module without yaml leaves it disabled.
	tk := choktest.NewTestKernel(t, `
db:
  driver: sqlite
  sqlite:
    path: ":memory:"
`, db.Module(), audit.Module())

	if _, ok := kernel.Get[*audit.Component](tk, "audit"); ok {
		t.Fatal("audit must stay disabled without an explicit audit.enabled: true")
	}
}

func TestModule_SinkRoundTrip_TableAtMigrate(t *testing.T) {
	component := audit.Module()
	tk := choktest.NewTestKernel(t, auditYAML, db.Module(), component)

	ac, ok := kernel.Get[*audit.Component](tk, "audit")
	if !ok {
		t.Fatal("audit component not visible")
	}
	testschema.AssertOwnership(t, db.From(tk), component)

	ctx := context.Background()
	if err := ac.LogEventSync(ctx, "user.login", "user", audit.ResultSuccess, map[string]string{"method": "password"}); err != nil {
		t.Fatal(err)
	}
	items, total, err := ac.Logger().Query(ctx, audit.Query{Action: "user.login"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("Query = %d items / total %d, want 1/1", len(items), total)
	}
	if items[0].Resource != "user" || items[0].Result != audit.ResultSuccess {
		t.Fatalf("stored row mismatch: %+v", items[0])
	}
}

func TestModule_MigrateOff_NoDDL_WritesFail(t *testing.T) {
	// off = the framework touches no schema; the sink comes up but a
	// missing table surfaces on the first synchronous write (and the
	// authz switch-on probe turns that into a startup failure for
	// audit-mandatory deployments).
	tk := choktest.NewTestKernel(t, `
db:
  driver: sqlite
  migrate: off
  sqlite:
    path: ":memory:"
audit:
  enabled: true
`, db.Module(), audit.Module())

	if db.From(tk).Unsafe(context.Background()).Migrator().HasTable("audit_logs") {
		t.Fatal("migrate off must not create audit_logs")
	}
	ac, _ := kernel.Get[*audit.Component](tk, "audit")
	if err := ac.LogEventSync(context.Background(), "x", "y", audit.ResultSuccess, nil); err == nil {
		t.Fatal("synchronous write against a missing table should error")
	}
}

// --- purge wiring (7.D) --------------------------------------------------

func TestModule_PurgeJob_RegisteredAndSweeps(t *testing.T) {
	tk := choktest.NewTestKernel(t, auditYAML+`
  retention_days: 30
  purge_batch_size: 2
`, db.Module(), scheduler.Module(), audit.Module())

	ac, _ := kernel.Get[*audit.Component](tk, "audit")
	if !ac.PurgeEnabled() {
		t.Fatal("purge should be wired when the scheduler module is assembled")
	}

	// Five expired rows (span the 2-row batch) + two live ones.
	ctx := context.Background()
	old := time.Now().AddDate(0, 0, -60)
	for i := 0; i < 5; i++ {
		if err := ac.Logger().LogSync(ctx, audit.Entry{Action: "old.event", OccurredAt: old}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := ac.Logger().LogSync(ctx, audit.Entry{Action: "fresh.event"}); err != nil {
			t.Fatal(err)
		}
	}

	sc, _ := kernel.Get[*scheduler.Component](tk, "scheduler")
	names := []string{}
	for _, e := range sc.Scheduler().Entries() {
		names = append(names, e.Name)
	}
	if !contains(names, "audit-purge") {
		t.Fatalf("audit-purge job not registered; jobs: %v", names)
	}
	if err := sc.Scheduler().RunNow("audit-purge"); err != nil {
		t.Fatal(err)
	}

	_, totalOld, err := ac.Logger().Query(ctx, audit.Query{Action: "old.event"})
	if err != nil {
		t.Fatal(err)
	}
	if totalOld != 0 {
		t.Fatalf("expired rows should be purged (batched), %d left", totalOld)
	}
	_, totalFresh, err := ac.Logger().Query(ctx, audit.Query{Action: "fresh.event"})
	if err != nil {
		t.Fatal(err)
	}
	if totalFresh != 2 {
		t.Fatalf("live rows must survive the sweep, got %d of 2", totalFresh)
	}
}

func TestModule_SchedulerAbsent_PurgeDisabled_StillBoots(t *testing.T) {
	tk := choktest.NewTestKernel(t, auditYAML, db.Module(), audit.Module())

	ac, ok := kernel.Get[*audit.Component](tk, "audit")
	if !ok {
		t.Fatal("audit should boot without the scheduler (purge degrades, sink lives)")
	}
	if ac.PurgeEnabled() {
		t.Fatal("purge must report disabled without a scheduler")
	}
	if err := ac.LogEventSync(context.Background(), "still.works", "x", "", nil); err != nil {
		t.Fatalf("sink must stay functional: %v", err)
	}
}

// --- admin API (fail-closed) ----------------------------------------------

func adminHandler(t *testing.T, tk *choktest.TestKernel) http.Handler {
	t.Helper()
	h, ok := tk.Router.Handler(http.MethodGet, "/audit/logs")
	if !ok {
		t.Fatal("GET /audit/logs not mounted")
	}
	return h
}

func TestModule_AdminAPI_FailClosedWithoutAuthz(t *testing.T) {
	tk := choktest.NewTestKernel(t, auditYAML, db.Module(), audit.Module())
	h := adminHandler(t, tk)

	// Anonymous: 401. No route serves data without a principal.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audit/logs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous request = %d, want 401", rec.Code)
	}

	// Authenticated but the authz module is absent (no Authorizer in
	// ctx): 500 fail-closed — never 200, never data.
	req := httptest.NewRequest(http.MethodGet, "/audit/logs", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Subject: "usr_admin"}))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("authz-absent request = %d, want 500 (fail-closed)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "items") {
		t.Fatal("fail-closed response must not carry audit data")
	}
}

func TestModule_AdminAPI_AuthorizedFlow(t *testing.T) {
	tk := choktest.NewTestKernel(t, auditYAML, db.Module(), authz.Module(), audit.Module())

	ac, _ := kernel.Get[*audit.Component](tk, "audit")
	azc, _ := kernel.Get[*authz.Component](tk, "authz")
	ctx := context.Background()
	if err := ac.LogEventSync(ctx, "user.login", "user", audit.ResultSuccess, nil); err != nil {
		t.Fatal(err)
	}
	if err := azc.Service().GrantUser(ctx, "usr_auditor", "audit", "read"); err != nil {
		t.Fatal(err)
	}

	h := adminHandler(t, tk)
	authed := func(subject string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/audit/logs?action=user.login", nil)
		rctx := auth.WithPrincipal(req.Context(), auth.Principal{Subject: subject})
		rctx = middleware.WithAuthorizer(rctx, azc.Authorizer())
		return req.WithContext(rctx)
	}

	// Granted auditor: 200 with the row.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed("usr_auditor"))
	if rec.Code != http.StatusOK {
		t.Fatalf("granted auditor = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []audit.Log `json:"items"`
		Total int64       `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}
	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("expected the seeded row, got total=%d items=%d", body.Total, len(body.Items))
	}

	// Ungranted user: 403 (authorize says no).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authed("usr_nobody"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted user = %d, want 403", rec.Code)
	}
}

func TestModule_AdminAPIDisabled_NotMounted(t *testing.T) {
	tk := choktest.NewTestKernel(t, auditYAML+`
  enable_admin_api: false
`, db.Module(), audit.Module())

	if _, ok := tk.Router.Handler(http.MethodGet, "/audit/logs"); ok {
		t.Fatal("enable_admin_api=false must not mount the admin route")
	}
}

// --- 7.E end-to-end against the real modules -------------------------------

func TestModule_AuthzDecisionAudit_EndToEnd(t *testing.T) {
	tk := choktest.NewTestKernel(t, auditYAML+`
authz:
  casbin:
    audit_enabled: true
    bootstrap_admin_user_id: usr_root
`, db.Module(), audit.Module(), authz.Module())

	ac, _ := kernel.Get[*audit.Component](tk, "audit")
	azc, _ := kernel.Get[*authz.Component](tk, "authz")
	ctx := context.Background()

	// The synchronous switch-on probe is durable evidence.
	_, probeTotal, err := ac.Logger().Query(ctx, audit.Query{Action: "authz.audit.enabled"})
	if err != nil {
		t.Fatal(err)
	}
	if probeTotal != 1 {
		t.Fatalf("switch-on probe entries = %d, want 1", probeTotal)
	}

	// Bootstrap seeding happened after the hook attach — audited.
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, total, err := ac.Logger().Query(ctx, audit.Query{Resource: "casbin_rule"})
		if err != nil {
			t.Fatal(err)
		}
		if total >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bootstrap policy mutations never reached the audit sink")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A runtime mutation flows through the async hook as well.
	if err := azc.Service().GrantRole(ctx, "editor", "post", "write"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		items, _, err := ac.Logger().Query(ctx, audit.Query{Action: "authz.GrantRole"})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, it := range items {
			var md map[string]any
			_ = json.Unmarshal(it.Metadata, &md)
			if md["role"] == "editor" {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("GrantRole mutation never reached the audit sink")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
