package casbin_test

import (
	"context"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/zynthara/chok/authz"
	"github.com/zynthara/chok/authz/casbin"
	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/parts"
)

// fakeKernel is a single-component Kernel just for the casbin
// Builder, which only ever asks for "db". Other Kernel methods are
// stubs the Builder never calls.
type fakeKernel struct {
	db *parts.DBComponent
}

func (k *fakeKernel) Get(name string) component.Component {
	if name == "db" {
		return k.db
	}
	return nil
}

func (k *fakeKernel) Config() any                          { return nil }
func (k *fakeKernel) ConfigSnapshot() any                  { return nil }
func (k *fakeKernel) Logger() log.Logger                   { return log.Empty() }
func (k *fakeKernel) On(component.Event, component.Hook)   {}
func (k *fakeKernel) Health(context.Context) component.HealthReport {
	return component.HealthReport{Status: component.HealthOK}
}
func (k *fakeKernel) ReadyCheck(context.Context) error { return nil }

// newTestSvc spins up a Casbin Service backed by an in-memory SQLite
// DBComponent. The DBComponent is constructed/Init'd against a fresh
// database for each test so policy state is isolated.
func newTestSvc(t *testing.T) (casbin.Service, *parts.DBComponent) {
	t.Helper()
	dbc := parts.NewDBComponent(func(component.Kernel) (*gorm.DB, error) {
		return gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	})
	if err := dbc.Init(context.Background(), &fakeKernel{}); err != nil {
		t.Fatal(err)
	}

	az, err := casbin.Builder(casbin.Options{})(&fakeKernel{db: dbc})
	if err != nil {
		t.Fatal(err)
	}
	svc, ok := az.(casbin.Service)
	if !ok {
		t.Fatalf("authorizer does not implement casbin.Service: %T", az)
	}
	return svc, dbc
}

// --- Service basic round-trip ----------------------------------------

func TestService_GrantRoleAndAuthorize(t *testing.T) {
	svc, _ := newTestSvc(t)
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
	svc, _ := newTestSvc(t)
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
	svc, _ := newTestSvc(t)
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
	svc, _ := newTestSvc(t)
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
	svc, _ := newTestSvc(t)
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
	svc, _ := newTestSvc(t)
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
	svc, _ := newTestSvc(t)
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
	svc, _ := newTestSvc(t)
	if err := casbin.Bootstrap(context.Background(), svc, casbin.BootstrapConfig{}); err == nil {
		t.Fatal("expected error on empty AdminUserID")
	}
}

// --- Authorizer interface contract ----------------------------------

func TestAuthorizer_SatisfiesDomainAuthorizer(t *testing.T) {
	svc, _ := newTestSvc(t)
	az, ok := svc.(authz.DomainAuthorizer)
	if !ok {
		t.Fatal("Casbin Authorizer should implement DomainAuthorizer")
	}
	// And of course Authorizer (the supertype).
	var _ authz.Authorizer = az
}

// TestAuthorizer_Close releases the watcher / audit hook. Without a
// watcher attached the test only validates Close returns nil; future
// PR with redis-watcher will extend this to assert subscriber tear-
// down.
func TestAuthorizer_Close(t *testing.T) {
	svc, _ := newTestSvc(t)
	closer, ok := svc.(interface {
		Close() error
	})
	if !ok {
		t.Fatal("authorizer should be io.Closer")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
}

// --- Builder error paths --------------------------------------------

func TestBuilder_RejectsRedisWatcherWithoutImpl(t *testing.T) {
	dbc := parts.NewDBComponent(func(component.Kernel) (*gorm.DB, error) {
		return gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	})
	if err := dbc.Init(context.Background(), &fakeKernel{}); err != nil {
		t.Fatal(err)
	}

	_, err := casbin.Builder(casbin.Options{RedisWatcher: true})(&fakeKernel{db: dbc})
	if err == nil || !strings.Contains(err.Error(), "RedisWatcher=true") {
		t.Fatalf("expected RedisWatcher fail-fast, got %v", err)
	}
}

func TestBuilder_RejectsAuditEnabledWithoutImpl(t *testing.T) {
	dbc := parts.NewDBComponent(func(component.Kernel) (*gorm.DB, error) {
		return gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	})
	if err := dbc.Init(context.Background(), &fakeKernel{}); err != nil {
		t.Fatal(err)
	}

	_, err := casbin.Builder(casbin.Options{AuditEnabled: true})(&fakeKernel{db: dbc})
	if err == nil || !strings.Contains(err.Error(), "AuditEnabled=true") {
		t.Fatalf("expected AuditEnabled fail-fast, got %v", err)
	}
}

func TestBuilder_RejectsMissingDB(t *testing.T) {
	_, err := casbin.Builder(casbin.Options{})(&fakeKernel{db: nil})
	if err == nil {
		t.Fatal("expected error when DB component absent")
	}
}
