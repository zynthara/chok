package authz_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/authz/casbin"
	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/redis"
)

const sqliteYAML = `
db:
  driver: sqlite
  sqlite:
    path: ":memory:"
`

// --- audit-sink test double -------------------------------------------

type sinkEvent struct {
	Action   string
	Resource string
	Result   string
	Metadata map[string]string
	Sync     bool
}

type fakeAuditOptions struct {
	Enabled bool `mapstructure:"enabled" default:"true"`
}

func (o *fakeAuditOptions) Validate() error { return nil }

// fakeAudit fills the "audit" kind with the AuditSink face — the
// double for the truth-table branches (the real audit module lands in
// the next commit and re-verifies integration).
type fakeAudit struct {
	initErr error
	syncErr error

	mu     sync.Mutex
	events []sinkEvent
}

var _ authz.AuditSink = (*fakeAudit)(nil)

func (f *fakeAudit) Describe() kernel.Descriptor {
	return kernel.Descriptor{Kind: "audit", ConfigKey: "audit", Options: fakeAuditOptions{}}
}
func (f *fakeAudit) Init(ctx context.Context, k kernel.Kernel) error { return f.initErr }
func (f *fakeAudit) Close(context.Context) error                     { return nil }

func (f *fakeAudit) record(e sinkEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *fakeAudit) LogEvent(ctx context.Context, action, resource, result string, metadata map[string]string) error {
	f.record(sinkEvent{Action: action, Resource: resource, Result: result, Metadata: metadata})
	return nil
}

func (f *fakeAudit) LogEventSync(ctx context.Context, action, resource, result string, metadata map[string]string) error {
	if f.syncErr != nil {
		return f.syncErr
	}
	f.record(sinkEvent{Action: action, Resource: resource, Result: result, Metadata: metadata, Sync: true})
	return nil
}

func (f *fakeAudit) snapshot() []sinkEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sinkEvent(nil), f.events...)
}

// --- core lifecycle ----------------------------------------------------

func TestModule_GrantAndAuthorize_FailClosedDefault(t *testing.T) {
	tk := choktest.NewTestKernel(t, sqliteYAML, db.Module(), authz.Module())

	ac, ok := kernel.Get[*authz.Component](tk, "authz")
	if !ok {
		t.Fatal("authz component not visible")
	}
	az := ac.Authorizer()
	if az == nil {
		t.Fatal("Authorizer() must be wired after startup (Migrate published the engine)")
	}
	ctx := context.Background()

	// Empty policy set: everything denies (fail-closed default).
	if allowed, err := az.Authorize(ctx, "usr_alice", "task", "read"); err != nil || allowed {
		t.Fatalf("empty policies should deny: allowed=%v err=%v", allowed, err)
	}

	svc := ac.Service()
	if svc == nil {
		t.Fatal("Service() must be wired after startup")
	}
	if err := svc.AddUserToRole(ctx, "usr_alice", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := svc.GrantRole(ctx, "admin", "task", "read"); err != nil {
		t.Fatal(err)
	}
	if allowed, err := az.Authorize(ctx, "usr_alice", "task", "read"); err != nil || !allowed {
		t.Fatalf("granted subject should authorize: allowed=%v err=%v", allowed, err)
	}
	if allowed, _ := az.Authorize(ctx, "usr_bob", "task", "read"); allowed {
		t.Fatal("ungranted subject must stay denied")
	}
}

func TestModule_CasbinRuleCreatedAtMigratePhase(t *testing.T) {
	tk := choktest.NewTestKernel(t, sqliteYAML, db.Module(), authz.Module())

	h := db.From(tk)
	if !h.Unsafe(context.Background()).Migrator().HasTable("casbin_rule") {
		t.Fatal("casbin_rule must exist after startup — the authz Migrator owns its creation (SPEC §5.3)")
	}
}

func TestModule_WebRoleInterface_Satisfied(t *testing.T) {
	// Pin the exact shape web/module.go asserts for AttachAuthz.
	tk := choktest.NewTestKernel(t, sqliteYAML, db.Module(), authz.Module())

	ac, ok := kernel.Get[interface{ Authorizer() authz.Authorizer }](tk, "authz")
	if !ok {
		t.Fatal("authz component must satisfy the web attach role interface")
	}
	if ac.Authorizer() == nil {
		t.Fatal("role-interface Authorizer() returned nil after startup")
	}
}

func TestModule_Disabled_NotVisible(t *testing.T) {
	tk := choktest.NewTestKernel(t, sqliteYAML+`
authz:
  enabled: false
`, db.Module(), authz.Module())

	if _, ok := kernel.Get[*authz.Component](tk, "authz"); ok {
		t.Fatal("disabled authz must not be reachable via kernel.Get")
	}
}

// --- migrate-off semantics (SPEC §5.3) ---------------------------------

func TestModule_MigrateOff_MissingTable_FailsClosed(t *testing.T) {
	// off = the framework touches no schema, battery tables included.
	// With no pre-existing casbin_rule the eager policy load must fail
	// startup — never a silently policy-less authorizer.
	_, err := choktest.StartKernel(t, `
db:
  driver: sqlite
  migrate: off
  sqlite:
    path: ":memory:"
`, db.Module(), authz.Module())
	if err == nil {
		t.Fatal("expected startup failure: migrate off with no casbin_rule table")
	}
	if !strings.Contains(err.Error(), "casbin") {
		t.Fatalf("error should surface the casbin load failure, got: %v", err)
	}
}

func TestModule_MigrateOff_OpsManagedSchema_Boots(t *testing.T) {
	// Operations pre-created the schema (the off-mode contract): the
	// module must boot against it without running any DDL of its own.
	path := filepath.Join(t.TempDir(), "authz.db")
	h, err := db.Open(db.Options{Driver: "sqlite", SQLite: db.SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Unsafe(context.Background()).AutoMigrate(&casbin.CasbinRule{}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	tk := choktest.NewTestKernel(t, fmt.Sprintf(`
db:
  driver: sqlite
  migrate: off
  sqlite:
    path: %q
`, path), db.Module(), authz.Module())

	ac, _ := kernel.Get[*authz.Component](tk, "authz")
	if ac.Authorizer() == nil {
		t.Fatal("authorizer should be up against the ops-managed schema")
	}
}

// --- bootstrap seeding --------------------------------------------------

func TestModule_BootstrapSeedsAdmin(t *testing.T) {
	tk := choktest.NewTestKernel(t, sqliteYAML+`
authz:
  casbin:
    bootstrap_admin_user_id: usr_root
`, db.Module(), authz.Module())

	ac, _ := kernel.Get[*authz.Component](tk, "authz")
	allowed, err := ac.Authorizer().Authorize(context.Background(), "usr_root", "anything", "read")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("bootstrapped admin should authorize on (*, *)")
	}
}

// --- audit_enabled truth table (SPEC §6, 7.E) ---------------------------

func TestModule_AuditEnabled_NotAssembled_FailsStartup(t *testing.T) {
	_, err := choktest.StartKernel(t, sqliteYAML+`
authz:
  casbin:
    audit_enabled: true
`, db.Module(), authz.Module())
	if err == nil {
		t.Fatal("expected startup failure: audit_enabled=true without the audit module")
	}
	if !strings.Contains(err.Error(), "audit module") {
		t.Fatalf("error should point at the audit module, got: %v", err)
	}
}

func TestModule_AuditEnabled_AuditDisabledByConfig_FailsStartup(t *testing.T) {
	_, err := choktest.StartKernel(t, sqliteYAML+`
audit:
  enabled: false
authz:
  casbin:
    audit_enabled: true
`, db.Module(), authz.Module(), &fakeAudit{})
	if err == nil {
		t.Fatal("expected startup failure: audit assembled but disabled by config")
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Fatalf("error should name audit, got: %v", err)
	}
}

func TestModule_AuditEnabled_AuditInitFailed_FailsStartup(t *testing.T) {
	// A failed audit Init aborts startup at the kernel level (audit is
	// not Optional) — authz never comes up with an un-audited engine.
	_, err := choktest.StartKernel(t, sqliteYAML+`
authz:
  casbin:
    audit_enabled: true
`, db.Module(), authz.Module(), &fakeAudit{initErr: fmt.Errorf("sink construction boom")})
	if err == nil {
		t.Fatal("expected startup failure: audit Init failed")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should carry the audit Init failure, got: %v", err)
	}
}

func TestModule_AuditEnabled_SinkUnavailable_FailsStartup(t *testing.T) {
	// Branch 4: audit is up but cannot land a synchronous entry — the
	// probe write fails startup instead of degrading to no-op audit.
	_, err := choktest.StartKernel(t, sqliteYAML+`
authz:
  casbin:
    audit_enabled: true
`, db.Module(), authz.Module(), &fakeAudit{syncErr: fmt.Errorf("sink down")})
	if err == nil {
		t.Fatal("expected startup failure: audit sink unavailable")
	}
	if !strings.Contains(err.Error(), "audit sink is unavailable") {
		t.Fatalf("error should name the unavailable sink, got: %v", err)
	}
}

func TestModule_AuditEnabled_HookEmitsMutations_IncludingBootstrap(t *testing.T) {
	sink := &fakeAudit{}
	tk := choktest.NewTestKernel(t, sqliteYAML+`
authz:
  casbin:
    audit_enabled: true
    bootstrap_admin_user_id: usr_root
`, db.Module(), authz.Module(), sink)

	// The synchronous switch-on probe must be there.
	events := sink.snapshot()
	var probe, bootstrapAudited bool
	for _, e := range events {
		if e.Sync && e.Action == "authz.audit.enabled" {
			probe = true
		}
		if strings.HasPrefix(e.Action, "authz.") && e.Action != "authz.audit.enabled" {
			bootstrapAudited = true
		}
	}
	if !probe {
		t.Fatalf("missing synchronous switch-on probe entry; events: %+v", events)
	}
	if !bootstrapAudited {
		t.Fatalf("bootstrap seeding must be audited (hook attaches before seeding); events: %+v", events)
	}

	// Runtime mutations keep flowing through the hook.
	ac, _ := kernel.Get[*authz.Component](tk, "authz")
	if err := ac.Service().GrantRole(context.Background(), "editor", "post", "write"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range sink.snapshot() {
		if e.Action == "authz.GrantRole" && e.Metadata["role"] == "editor" {
			found = true
		}
	}
	if !found {
		t.Fatalf("GrantRole mutation not audited; events: %+v", sink.snapshot())
	}
}

// --- redis watcher (module layer) ---------------------------------------

func TestModule_RedisWatcher_RequiresRedisModule(t *testing.T) {
	_, err := choktest.StartKernel(t, sqliteYAML+`
authz:
  casbin:
    redis_watcher: true
`, db.Module(), authz.Module())
	if err == nil {
		t.Fatal("expected startup failure: redis_watcher without the redis module")
	}
	if !strings.Contains(err.Error(), "redis module") {
		t.Fatalf("error should point at the redis module, got: %v", err)
	}
}

func TestModule_RedisWatcher_PublishesOnMutation(t *testing.T) {
	mr := miniredis.RunT(t)
	tk := choktest.NewTestKernel(t, fmt.Sprintf(`
db:
  driver: sqlite
  sqlite:
    path: ":memory:"
redis:
  addr: %s
authz:
  casbin:
    redis_watcher: true
`, mr.Addr()), db.Module(), redis.Module(), authz.Module())

	spy := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = spy.Close() })
	pubsub := spy.Subscribe(context.Background(), "chok:authz:policy")
	t.Cleanup(func() { _ = pubsub.Close() })
	if _, err := pubsub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgCh := pubsub.Channel()

	ac, _ := kernel.Get[*authz.Component](tk, "authz")
	if err := ac.Service().GrantRole(context.Background(), "admin", "task", "read"); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-msgCh:
		if !strings.HasPrefix(msg.Payload, "ciw_") {
			t.Errorf("expected ciw_-prefixed watcher instance ID, got %q", msg.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("module-wired watcher never published on Service mutation")
	}
	if stats := ac.WatcherStats(); stats.PublishFailures != 0 {
		t.Errorf("unexpected publish failures: %+v", stats)
	}
}
