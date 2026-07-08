package casbin_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/authz/casbin"
)

// newTestDB opens a fresh in-memory SQLite with the casbin_rule table
// pre-created — tests play the authz module's Migrate role, since the
// adapter itself no longer runs DDL (M4 / SPEC §5.3).
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&casbin.CasbinRule{}); err != nil {
		t.Fatal(err)
	}
	return gdb
}

// newTestEngine builds the blessed engine (default model) against an
// isolated database — the kernel-less construction path NewEngine
// exists for.
func newTestEngine(t *testing.T) *casbin.Engine {
	t.Helper()
	eng, err := casbin.NewEngine(casbin.Options{}.ModelOrDefault(), newTestDB(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// newTestSvc narrows the engine to the Service management face.
func newTestSvc(t *testing.T) casbin.Service {
	t.Helper()
	return newTestEngine(t)
}

// --- Service basic round-trip ----------------------------------------

func TestService_GrantRoleAndAuthorize(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	if err := svc.AddUserToRole(ctx, "usr_alice", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := svc.GrantRole(ctx, "admin", "task", "read"); err != nil {
		t.Fatal(err)
	}

	allowed, err := svc.HasPermission(ctx, "usr_alice", "task", "read")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("alice should be allowed to read task via global admin role")
	}

	denied, _ := svc.HasPermission(ctx, "usr_alice", "task", "delete")
	if denied {
		t.Error("alice should NOT be allowed to delete (no policy)")
	}
}

// --- Domain semantics ------------------------------------------------

func TestService_DomainScopedRoles(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	// alice is admin in workspace ws_abc only.
	if err := svc.AddUserToRoleInDomain(ctx, "usr_alice", "admin", "ws_abc"); err != nil {
		t.Fatal(err)
	}
	if err := svc.GrantRoleInDomain(ctx, "admin", "task", "read", "ws_abc"); err != nil {
		t.Fatal(err)
	}

	// allowed in ws_abc
	allowed, _ := svc.HasPermissionInDomain(ctx, "usr_alice", "task", "read", "ws_abc")
	if !allowed {
		t.Error("alice should be allowed in ws_abc")
	}
	// not allowed in ws_def
	denied, _ := svc.HasPermissionInDomain(ctx, "usr_alice", "task", "read", "ws_def")
	if denied {
		t.Error("alice should NOT cross-leak into ws_def (matcher domain check)")
	}
}

// TestService_GlobalAdminPasses_ThroughDomain proves the SPEC §7.7
// matcher passthrough: a global role (g(usr, role, "*")) with a
// global policy (p(role, "*", "*", "*")) authorises in any domain.
func TestService_GlobalAdminPasses_ThroughDomain(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	if err := svc.AddUserToRole(ctx, "usr_admin", "superadmin"); err != nil {
		t.Fatal(err)
	}
	if err := svc.GrantRole(ctx, "superadmin", "*", "*"); err != nil {
		t.Fatal(err)
	}

	for _, dom := range []string{"ws_abc", "ws_def", "tenant_42"} {
		allowed, err := svc.HasPermissionInDomain(ctx, "usr_admin", "task", "read", dom)
		if err != nil {
			t.Fatal(err)
		}
		if !allowed {
			t.Errorf("global superadmin should authorise in %q", dom)
		}
	}
}

// TestService_GrantUser_DirectAuthorize proves SPEC §7.7 v0.3.4
// `r.sub == p.sub` matcher clause: direct user grants (no role
// mediation) authorize.
func TestService_GrantUser_DirectAuthorize(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	if err := svc.GrantUser(ctx, "usr_bob", "task", "read"); err != nil {
		t.Fatal(err)
	}
	allowed, _ := svc.HasPermission(ctx, "usr_bob", "task", "read")
	if !allowed {
		t.Error("direct GrantUser should authorise via r.sub == p.sub clause")
	}
}

// TestService_RejectsGlobalAsTenant proves SPEC §7.7 v0.3.4: passing
// "*" as a tenant id is a structured error rather than a silent
// global grant.
func TestService_RejectsGlobalAsTenant(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	cases := []struct {
		name string
		fn   func() error
	}{
		{"AddUserToRoleInDomain", func() error {
			return svc.AddUserToRoleInDomain(ctx, "u", "r", "*")
		}},
		{"GrantRoleInDomain", func() error {
			return svc.GrantRoleInDomain(ctx, "r", "o", "a", "*")
		}},
		{"HasPermissionInDomain", func() error {
			_, err := svc.HasPermissionInDomain(ctx, "u", "o", "a", "*")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn()
			if err == nil {
				t.Fatal("expected error for tenant-domain=\"*\"")
			}
			if !strings.Contains(err.Error(), "reserved for global") {
				t.Fatalf("expected 'reserved for global' message, got %v", err)
			}
		})
	}
}

// TestService_NormalizeEmptyDomain covers the SPEC §7.7 alias: empty
// string in InDomain methods normalizes to "*" so callers can use
// "" as a global shorthand.
func TestService_NormalizeEmptyDomain(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	if err := svc.AddUserToRoleInDomain(ctx, "usr_x", "admin", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.GrantRoleInDomain(ctx, "admin", "task", "read", ""); err != nil {
		t.Fatal(err)
	}

	// Normalised to "*" in storage; visible via the no-suffix global
	// reader (UserRoles wraps GetRolesForUserInDomain with dom="*").
	roles, err := svc.UserRoles(ctx, "usr_x")
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || roles[0] != "admin" {
		t.Fatalf("UserRoles after AddUserToRoleInDomain(... \"\") = %v, want [admin]", roles)
	}
}

// --- Bootstrap ------------------------------------------------------

func TestBootstrap_IdempotentSeeding(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	cfg := casbin.BootstrapConfig{AdminUserID: "usr_root"}
	if err := casbin.Bootstrap(ctx, svc, cfg); err != nil {
		t.Fatal(err)
	}
	// Second call must not error (Casbin returns false on duplicate
	// AddPolicy; Service treats that as no-op success).
	if err := casbin.Bootstrap(ctx, svc, cfg); err != nil {
		t.Fatalf("second Bootstrap call should be idempotent, got %v", err)
	}

	allowed, _ := svc.HasPermission(ctx, "usr_root", "anything", "read")
	if !allowed {
		t.Error("bootstrapped admin should authorise on (*, *)")
	}
}

func TestBootstrap_RejectsEmptyAdminID(t *testing.T) {
	svc := newTestSvc(t)
	if err := casbin.Bootstrap(context.Background(), svc, casbin.BootstrapConfig{}); err == nil {
		t.Fatal("expected error on empty AdminUserID")
	}
}

// --- Authorizer interface contract ----------------------------------

func TestAuthorizer_SatisfiesDomainAuthorizer(t *testing.T) {
	var eng any = newTestEngine(t)
	az, ok := eng.(authz.DomainAuthorizer)
	if !ok {
		t.Fatal("Casbin Engine should implement DomainAuthorizer")
	}
	// And of course Authorizer (the supertype).
	var _ authz.Authorizer = az
}

// TestAuthorizer_Close releases the watcher / audit hook. Without a
// watcher attached the test only validates Close returns nil; the
// watcher tear-down is asserted in TestEngine_RedisWatcher_*.
func TestAuthorizer_Close(t *testing.T) {
	eng := newTestEngine(t)
	var closer io.Closer = eng
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
}

// --- Engine construction error paths ---------------------------------

func TestNewEngine_RejectsNilDB(t *testing.T) {
	if _, err := casbin.NewEngine(casbin.Options{}.ModelOrDefault(), nil, nil); err == nil {
		t.Fatal("expected error on nil *gorm.DB")
	}
}

// TestNewEngine_MissingTable_FailsAtLoad pins the fail-closed surface
// after the DDL split: constructing against a database without
// casbin_rule must error at the eager LoadPolicy — never silently
// produce an engine with no policies.
func TestNewEngine_MissingTable_FailsAtLoad(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := casbin.NewEngine(casbin.Options{}.ModelOrDefault(), gdb, nil); err == nil {
		t.Fatal("expected LoadPolicy failure when casbin_rule is missing")
	}
}

// --- Redis watcher wiring (engine layer) ------------------------------

// TestEngine_RedisWatcher_PublishesOnMutation exercises the positive
// wiring path against an in-process miniredis: an engine with the
// watcher attached drives a real PUBLISH on Service mutations, and an
// independent subscriber sees it. (Successor of the v1 Builder-layer
// test — the plumbing now lives in Engine.AttachRedisWatcher; the
// module-layer wiring is covered in authz's module tests.)
func TestEngine_RedisWatcher_PublishesOnMutation(t *testing.T) {
	mr := miniredis.RunT(t)
	eng := newTestEngine(t)

	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	if err := eng.AttachRedisWatcher(context.Background(), client, "chok:authz:policy"); err != nil {
		t.Fatal(err)
	}

	// Independent subscriber on the same channel: proves the watcher
	// actually publishes when the Service mutates.
	spy := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = spy.Close() })
	pubsub := spy.Subscribe(context.Background(), "chok:authz:policy")
	t.Cleanup(func() { _ = pubsub.Close() })
	if _, err := pubsub.Receive(context.Background()); err != nil {
		t.Fatalf("spy subscribe handshake: %v", err)
	}
	msgCh := pubsub.Channel()

	if err := eng.GrantRole(context.Background(), "admin", "task", "read"); err != nil {
		t.Fatalf("GrantRole: %v", err)
	}

	select {
	case msg := <-msgCh:
		// Payload is the chok watcher instance ID (rid prefix "ciw_").
		if !strings.HasPrefix(msg.Payload, "ciw_") {
			t.Errorf("expected ciw_-prefixed instance ID payload, got %q", msg.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher never published on Service mutation — AttachRedisWatcher / SetWatcher wiring broken")
	}

	if err := eng.Close(); err != nil {
		t.Errorf("Close should be clean, got %v", err)
	}
}

// TestEngine_RedisWatcher_StatsExposed proves the WatcherStats()
// escape hatch returns live counters from the underlying watcher.
func TestEngine_RedisWatcher_StatsExposed(t *testing.T) {
	mr := miniredis.RunT(t)
	eng := newTestEngine(t)
	t.Cleanup(func() { _ = eng.Close() })

	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	if err := eng.AttachRedisWatcher(context.Background(), client, "chok:authz:policy"); err != nil {
		t.Fatal(err)
	}

	if got := eng.WatcherStats(); got.PublishFailures != 0 || got.ReloadFailures != 0 {
		t.Errorf("baseline stats non-zero: %+v", got)
	}

	// Forcing publish failures: kill miniredis backend, then issue
	// a Service mutation. The mutation succeeds (DB commit); the
	// watcher publish silently fails and bumps the counter.
	mr.Close()
	if err := eng.GrantRole(context.Background(), "admin", "task", "read"); err != nil {
		t.Fatalf("GrantRole should succeed even when publish fails (best-effort), got %v", err)
	}
	if got := eng.WatcherStats(); got.PublishFailures < 1 {
		t.Errorf("after publish failure, PublishFailures = %d, want >=1", got.PublishFailures)
	}
}

// TestEngine_NoStatsWithoutWatcher pins the zero-value behaviour:
// without a watcher the stats accessor still works (empty value) so
// callers don't need to special-case the "no watcher" path.
func TestEngine_NoStatsWithoutWatcher(t *testing.T) {
	eng := newTestEngine(t)
	t.Cleanup(func() { _ = eng.Close() })

	if got := eng.WatcherStats(); got != (casbin.WatcherStats{}) {
		t.Errorf("disabled-watcher stats should be zero value, got %+v", got)
	}
}
