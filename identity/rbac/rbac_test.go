package rbac_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/choktest"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/identity/rbac"
	"github.com/zynthara/chok/v3/internal/dbtest"
)

const key = "this-is-a-test-signing-key-32bytes!"

// setup is an Identity and an RBAC on one fresh database of the lane.
func setup(t *testing.T, cfg identity.Config, opts ...rbac.Option) (*identity.Identity, *rbac.RBAC, *db.DB) {
	t.Helper()
	d := dbtest.Open(t)
	cfg.SigningKey = key
	idn, err := identity.New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idn.Close(context.Background()) })
	rb, err := rbac.New(idn, d, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rb.Close(context.Background()) })
	return idn, rb, d
}

func policy(t *testing.T) *rbac.PolicyService {
	t.Helper()
	_, rb, _ := setup(t, identity.Config{})
	return rb.Policy()
}

func TestPolicy_GrantRoleAndAuthorize(t *testing.T) {
	p, ctx := policy(t), context.Background()
	if ok, err := p.HasPermission(ctx, "usr_alice", "task", "read"); err != nil || ok {
		t.Fatalf("no policies deny: %v %v", ok, err)
	}
	if err := p.AddUserToRole(ctx, "usr_alice", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := p.GrantRole(ctx, "admin", "task", "read"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.HasPermission(ctx, "usr_alice", "task", "read"); !ok {
		t.Fatal("alice reads tasks through the admin role")
	}
	if ok, _ := p.HasPermission(ctx, "usr_alice", "task", "delete"); ok {
		t.Fatal("no policy grants delete")
	}
	if ok, _ := p.HasPermission(ctx, "usr_bob", "task", "read"); ok {
		t.Fatal("bob holds no role")
	}
}

func TestPolicy_DomainScopedRoles(t *testing.T) {
	p, ctx := policy(t), context.Background()
	if err := p.AddUserToRoleInDomain(ctx, "usr_alice", "admin", "ws_abc"); err != nil {
		t.Fatal(err)
	}
	if err := p.GrantRoleInDomain(ctx, "admin", "task", "read", "ws_abc"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.HasPermissionInDomain(ctx, "usr_alice", "task", "read", "ws_abc"); !ok {
		t.Fatal("alice is admin in ws_abc")
	}
	if ok, _ := p.HasPermissionInDomain(ctx, "usr_alice", "task", "read", "ws_def"); ok {
		t.Fatal("a tenant's role does not leak into another tenant")
	}
	if ok, _ := p.HasPermission(ctx, "usr_alice", "task", "read"); ok {
		t.Fatal("nor into the global scope")
	}
}

func TestPolicy_GlobalAdminPassesThroughDomains(t *testing.T) {
	p, ctx := policy(t), context.Background()
	if err := p.AddUserToRole(ctx, "usr_admin", "superadmin"); err != nil {
		t.Fatal(err)
	}
	if err := p.GrantRole(ctx, "superadmin", "*", "*"); err != nil {
		t.Fatal(err)
	}
	for _, dom := range []string{"ws_abc", "ws_def", "tenant_42"} {
		if ok, err := p.HasPermissionInDomain(ctx, "usr_admin", "task", "read", dom); err != nil || !ok {
			t.Errorf("a global superadmin in %s: %v %v", dom, ok, err)
		}
	}
}

func TestPolicy_GrantUserDirect(t *testing.T) {
	p, ctx := policy(t), context.Background()
	if err := p.GrantUser(ctx, "usr_bob", "task", "read"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.HasPermission(ctx, "usr_bob", "task", "read"); !ok {
		t.Fatal("a direct grant authorizes without a role")
	}
	if err := p.RevokeUser(ctx, "usr_bob", "task", "read"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.HasPermission(ctx, "usr_bob", "task", "read"); ok {
		t.Fatal("revoked")
	}
}

func TestPolicy_RejectsGlobalAsTenant(t *testing.T) {
	p, ctx := policy(t), context.Background()
	for name, fn := range map[string]func() error{
		"AddUserToRoleInDomain":      func() error { return p.AddUserToRoleInDomain(ctx, "u", "r", "*") },
		"RemoveUserFromRoleInDomain": func() error { return p.RemoveUserFromRoleInDomain(ctx, "u", "r", "*") },
		"GrantRoleInDomain":          func() error { return p.GrantRoleInDomain(ctx, "r", "o", "a", "*") },
		"RevokeRoleInDomain":         func() error { return p.RevokeRoleInDomain(ctx, "r", "o", "a", "*") },
		"UserRolesInDomain":          func() error { _, err := p.UserRolesInDomain(ctx, "u", "*"); return err },
		"RolePermissionsInDomain":    func() error { _, err := p.RolePermissionsInDomain(ctx, "r", "*"); return err },
		"HasPermissionInDomain":      func() error { _, err := p.HasPermissionInDomain(ctx, "u", "o", "a", "*"); return err },
	} {
		if err := fn(); err == nil || !strings.Contains(err.Error(), "reserved for global") {
			t.Errorf("%s with domain *: %v", name, err)
		}
	}
}

func TestPolicy_EmptyDomainIsGlobal(t *testing.T) {
	p, ctx := policy(t), context.Background()
	if err := p.AddUserToRoleInDomain(ctx, "usr_x", "admin", ""); err != nil {
		t.Fatal(err)
	}
	if err := p.GrantRoleInDomain(ctx, "admin", "task", "read", ""); err != nil {
		t.Fatal(err)
	}
	if roles, err := p.UserRoles(ctx, "usr_x"); err != nil || len(roles) != 1 || roles[0] != "admin" {
		t.Fatalf("roles = %v %v", roles, err)
	}
	if ok, _ := p.HasPermission(ctx, "usr_x", "task", "read"); !ok {
		t.Fatal("a grant in domain \"\" is global")
	}
}

func TestPolicy_ListsAndRevokes(t *testing.T) {
	p, ctx := policy(t), context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(p.AddUserToRole(ctx, "usr_a", "editor"))
	must(p.AddUserToRoleInDomain(ctx, "usr_a", "owner", "ws_1"))
	must(p.GrantRole(ctx, "editor", "post", "write"))
	must(p.GrantRoleInDomain(ctx, "owner", "ws", "manage", "ws_1"))
	if doms, _ := p.DomainsForUser(ctx, "usr_a"); len(doms) != 2 {
		t.Fatalf("domains = %v", doms)
	}
	if perms, _ := p.RolePermissions(ctx, "editor"); len(perms) != 1 || perms[0] != (rbac.Permission{Object: "post", Action: "write"}) {
		t.Fatalf("editor's permissions = %v", perms)
	}
	if perms, _ := p.RolePermissionsInDomain(ctx, "owner", "ws_1"); len(perms) != 1 || perms[0].Action != "manage" {
		t.Fatalf("owner's permissions = %v", perms)
	}
	if roles, _ := p.UserRolesInDomain(ctx, "usr_a", "ws_1"); len(roles) != 1 || roles[0] != "owner" {
		t.Fatalf("roles in ws_1 = %v", roles)
	}
	must(p.RevokeRole(ctx, "editor", "post", "write"))
	must(p.RemoveUserFromRole(ctx, "usr_a", "editor"))
	must(p.RemoveUserFromRoleInDomain(ctx, "usr_a", "owner", "ws_1"))
	must(p.RevokeRoleInDomain(ctx, "owner", "ws", "manage", "ws_1"))
	if doms, _ := p.DomainsForUser(ctx, "usr_a"); len(doms) != 0 {
		t.Fatalf("after the removals: %v", doms)
	}
	must(p.ReloadPolicy(ctx))
}

func TestBootstrapAdmin(t *testing.T) {
	p, ctx := policy(t), context.Background()
	for range 2 { // idempotent
		if err := p.BootstrapAdmin(ctx, "usr_root"); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := p.HasPermission(ctx, "usr_root", "anything", "read"); !ok {
		t.Fatal("the bootstrapped admin may do anything")
	}
	if ok, _ := p.HasPermissionInDomain(ctx, "usr_root", "anything", "write", "ws_1"); !ok {
		t.Fatal("in every domain")
	}
	if err := p.BootstrapAdmin(ctx, ""); err == nil {
		t.Fatal("an empty user id is refused")
	}
}

func TestNew_RefusesNil(t *testing.T) {
	d := dbtest.Open(t)
	if _, err := rbac.New(nil, d); err == nil {
		t.Fatal("nil Identity")
	}
	idn, _, _ := setup(t, identity.Config{})
	if _, err := rbac.New(idn, nil); err == nil {
		t.Fatal("nil database")
	}
}

func TestNew_FailsClosedWithoutTheTable(t *testing.T) {
	// migrate: off on a database with identity's tables (versioned) but
	// no casbin_rule: the policies cannot load.
	cfg := dbtest.Config(t)
	cfg.Migrate = "versioned"
	d := dbtest.OpenConfig(t, cfg)
	if _, err := identity.New(identity.Config{SigningKey: key}, d); err != nil {
		t.Fatal(err)
	}
	cfg.Migrate = "off"
	off := dbtest.OpenConfig(t, cfg)
	idn, err := identity.New(identity.Config{SigningKey: key}, off)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rbac.New(idn, off); err == nil || !strings.Contains(err.Error(), "migrations are not applied") {
		t.Fatalf("rbac on migrate off without casbin_rule: %v", err)
	}
}

func TestNew_MigrateOffOnAPreparedSchema(t *testing.T) {
	cfg := dbtest.Config(t)
	cfg.Migrate = "versioned"
	d := dbtest.OpenConfig(t, cfg)
	idn, err := identity.New(identity.Config{SigningKey: key}, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rbac.New(idn, d); err != nil {
		t.Fatal(err)
	}
	cfg.Migrate = "off"
	off := dbtest.OpenConfig(t, cfg)
	idn2, err := identity.New(identity.Config{SigningKey: key}, off)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := rbac.New(idn2, off)
	if err != nil {
		t.Fatalf("migrate off on a migrated database: %v", err)
	}
	if err := rb.Policy().BootstrapAdmin(context.Background(), "usr_root"); err != nil {
		t.Fatal(err)
	}
}

// serve runs idn and rb with a guarded route, and returns the client and
// a token for a registered user.
func serve(t *testing.T, idn *identity.Identity, rb *rbac.RBAC, routes func(api chok.Router)) (*choktest.Client, string, string) {
	t.Helper()
	c := choktest.App(t, chok.Base{}, func(app *chok.App) {
		app.Use(idn, rb)
		routes(app.Group("/api", idn.Authn()))
	})
	var tok struct {
		Token string `json:"token"`
	}
	if code := c.PostJSON("/auth/register", map[string]string{"email": "alice@test.com", "password": "password123"}, &tok); code != http.StatusCreated {
		t.Fatalf("register = %d", code)
	}
	var me struct {
		ID string `json:"id"`
	}
	c.WithBearer(tok.Token).GetJSON("/auth/me", &me)
	return c, tok.Token, me.ID
}

func ok(ctx context.Context, _ *chok.None) (string, error) { return "ok", nil }

type envelope struct {
	Code      int    `json:"code"`
	Reason    string `json:"reason"`
	RequestID string `json:"request_id"`
}

func TestRequire(t *testing.T) {
	idn, rb, _ := setup(t, identity.Config{})
	c, tok, uid := serve(t, idn, rb, func(api chok.Router) {
		chok.GET(api, "/tasks", ok, chok.Use(rb.Require("task", "read")))
	})
	var e envelope
	if code := c.WithBearer(tok).GetJSON("/api/tasks", &e); code != http.StatusForbidden || e.Reason != "Forbidden" || e.RequestID == "" {
		t.Fatalf("no grant = %d %+v", code, e)
	}
	if err := rb.Policy().GrantUser(context.Background(), uid, "task", "read"); err != nil {
		t.Fatal(err)
	}
	var body string
	if code := c.WithBearer(tok).GetJSON("/api/tasks", &body); code != http.StatusOK || body != "ok" {
		t.Fatalf("granted = %d %q", code, body)
	}
}

func TestRequire_NoPrincipalIs401(t *testing.T) {
	idn, rb, _ := setup(t, identity.Config{})
	// Require without Authn in front: no principal, a 401.
	c := choktest.App(t, chok.Base{}, func(app *chok.App) {
		app.Use(idn, rb)
		chok.GET(app.Router(), "/open", ok, chok.Use(rb.Require("task", "read")))
	})
	var e envelope
	if code := c.GetJSON("/open", &e); code != http.StatusUnauthorized || e.Reason != "Unauthorized" || e.RequestID == "" {
		t.Fatalf("no principal = %d %+v", code, e)
	}
}

func TestRequire_FailedDecisionIs500(t *testing.T) {
	// A model whose matcher calls a function casbin does not have fails
	// every decision.
	broken := `[request_definition]
r = sub, dom, obj, act

[policy_definition]
p = sub, dom, obj, act

[role_definition]
g = _, _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = noSuchFunction(r.sub, p.sub)
`
	idn, rb, _ := setup(t, identity.Config{}, rbac.Model(broken))
	c, tok, _ := serve(t, idn, rb, func(api chok.Router) {
		chok.GET(api, "/tasks", ok, chok.Use(rb.Require("task", "read")))
	})
	if err := rb.Policy().GrantRole(context.Background(), "x", "task", "read"); err != nil {
		t.Fatal(err)
	}
	var e envelope
	if code := c.WithBearer(tok).GetJSON("/api/tasks", &e); code != http.StatusInternalServerError || e.Reason != "InternalError" {
		t.Fatalf("a failed decision = %d %+v", code, e)
	}
}

func TestRequireInDomain(t *testing.T) {
	idn, rb, _ := setup(t, identity.Config{})
	c, tok, uid := serve(t, idn, rb, func(api chok.Router) {
		chok.GET(api, "/workspaces/{wsid}/tasks", ok, chok.Use(rb.RequireInDomain("task", "read", "wsid")))
		chok.GET(api, "/tasks", ok, chok.Use(rb.RequireInDomain("task", "read", "wsid")))
	})
	ctx := context.Background()
	if err := rb.Policy().AddUserToRoleInDomain(ctx, uid, "member", "ws-abc"); err != nil {
		t.Fatal(err)
	}
	if err := rb.Policy().GrantRoleInDomain(ctx, "member", "task", "read", "ws-abc"); err != nil {
		t.Fatal(err)
	}
	alice := c.WithBearer(tok)
	if code := alice.GetJSON("/api/workspaces/ws-abc/tasks", nil); code != http.StatusOK {
		t.Fatalf("her workspace = %d", code)
	}
	if code := alice.GetJSON("/api/workspaces/ws-def/tasks", nil); code != http.StatusForbidden {
		t.Fatalf("another workspace = %d", code)
	}
	var e envelope
	if code := alice.GetJSON("/api/tasks", &e); code != http.StatusBadRequest || e.Reason != "BadRequest" {
		t.Fatalf("a route without the domain parameter = %d %+v", code, e)
	}
	if code := c.GetJSON("/api/workspaces/ws-abc/tasks", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", code)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("RequireInDomain without a parameter name panics")
		}
	}()
	rb.RequireInDomain("task", "read", "")
}

func TestAudit_RecordsPolicyChangesAndDenials(t *testing.T) {
	idn, rb, _ := setup(t, identity.Config{Audit: identity.AuditConfig{Enabled: true}})
	ctx := context.Background()
	if err := rb.Policy().BootstrapAdmin(ctx, "usr_root"); err != nil {
		t.Fatal(err)
	}
	if err := rb.Policy().GrantRole(ctx, "editor", "post", "write"); err != nil {
		t.Fatal(err)
	}
	c, tok, _ := serve(t, idn, rb, func(api chok.Router) {
		chok.GET(api, "/posts", ok, chok.Use(rb.Require("post", "read")))
	})
	if code := c.WithBearer(tok).GetJSON("/api/posts", nil); code != http.StatusForbidden {
		t.Fatalf("denied = %d", code)
	}
	find := func(q url.Values) []identity.Event {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			page, err := idn.Audit().ListFromQuery(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) > 0 || time.Now().After(deadline) {
				return page.Items
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// New proved the log writes, synchronously.
	if probe := find(url.Values{"action": {"authz.audit.enabled"}}); len(probe) != 1 || probe[0].ActorType != identity.ActorSystem {
		t.Fatalf("the switch-on probe: %+v", probe)
	}
	if boot := find(url.Values{"action": {"authz.AddUserToRole"}}); len(boot) != 1 || boot[0].Metadata["role"] != "admin" {
		t.Fatalf("the bootstrap is recorded: %+v", boot)
	}
	grants := find(url.Values{"action": {"authz.GrantRole"}})
	editor := false
	for _, g := range grants {
		editor = editor || g.Metadata["role"] == "editor"
	}
	if !editor || grants[0].Resource != "casbin_rule" {
		t.Fatalf("the grant is recorded: %+v", grants)
	}
	deny := find(url.Values{"action": {"authz.deny"}})
	if len(deny) != 1 || deny[0].Result != identity.ResultDenied || deny[0].Resource != "post" || deny[0].Metadata["action"] != "read" || deny[0].ActorID == "" {
		t.Fatalf("the denial is recorded: %+v", deny)
	}
}

func TestAudit_AnUnwritableLogFailsNew(t *testing.T) {
	d := dbtest.Open(t)
	idn, err := identity.New(identity.Config{SigningKey: key, Audit: identity.AuditConfig{Enabled: true}}, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idn.Close(context.Background()) })
	if err := d.Unsafe(t.Context()).Migrator().DropTable("audit_logs"); err != nil {
		t.Fatal(err)
	}
	if _, err := rbac.New(idn, d); err == nil || !strings.Contains(err.Error(), "the audit log does not take records") {
		t.Fatalf("an audit log that cannot write: %v", err)
	}
}

func TestAudit_AdminAPIAsksRBAC(t *testing.T) {
	idn, rb, _ := setup(t, identity.Config{Audit: identity.AuditConfig{Enabled: true, AdminAPI: true}})
	c, tok, uid := serve(t, idn, rb, func(chok.Router) {})
	if code := c.WithBearer(tok).GetJSON("/audit/logs", nil); code != http.StatusForbidden {
		t.Fatalf("without audit/read = %d", code)
	}
	if err := rb.Policy().GrantUser(context.Background(), uid, "audit", "read"); err != nil {
		t.Fatal(err)
	}
	var page chok.Page[identity.Event]
	if code := c.WithBearer(tok).GetJSON("/audit/logs?action=authz.audit.enabled", &page); code != http.StatusOK || page.Total != 1 {
		t.Fatalf("with audit/read = %d %+v", code, page)
	}
}

func TestWatcher_PeersReload(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := dbtest.Config(t)
	// Two replicas: two handles on one database, one Redis.
	replica := func() *rbac.RBAC {
		d := dbtest.OpenConfig(t, cfg)
		idn, err := identity.New(identity.Config{SigningKey: key}, d)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = idn.Close(context.Background()) })
		client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = client.Close() })
		rb, err := rbac.New(idn, d, rbac.Watcher(client))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rb.Close(context.Background()) })
		return rb
	}
	a, b := replica(), replica()
	ctx := context.Background()
	if err := a.Policy().GrantUser(ctx, "usr_alice", "task", "read"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ok, _ := b.Policy().HasPermission(ctx, "usr_alice", "task", "read"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the other replica never reloaded the grant")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatcher_PublishesOnMutation(t *testing.T) {
	mr := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	_, rb, _ := setup(t, identity.Config{}, rbac.Watcher(client))
	spy := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = spy.Close() })
	sub := spy.Subscribe(context.Background(), "chok:authz:policy")
	t.Cleanup(func() { _ = sub.Close() })
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := rb.Policy().GrantRole(context.Background(), "admin", "task", "read"); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-sub.Channel():
		if !strings.HasPrefix(msg.Payload, "ciw_") {
			t.Fatalf("the payload is the replica's id: %q", msg.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no announcement of the change")
	}
	if err := rb.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWatcher_UnreachableRedisFailsNew(t *testing.T) {
	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })
	d := dbtest.Open(t)
	idn, err := identity.New(identity.Config{SigningKey: key}, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idn.Close(context.Background()) })
	if _, err := rbac.New(idn, d, rbac.Watcher(client)); err == nil || !strings.Contains(err.Error(), "redis watcher") {
		t.Fatalf("an unreachable Redis: %v", err)
	}
}

func TestComponent(t *testing.T) {
	_, rb, _ := setup(t, identity.Config{})
	if rb.Name() != "rbac" || rb.Health(context.Background()) != nil {
		t.Fatal("name and health")
	}
	if err := rb.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := rb.Close(context.Background()); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}
