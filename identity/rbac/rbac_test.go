package rbac_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
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

func policy(t *testing.T) (*identity.Identity, *rbac.PolicyService) {
	t.Helper()
	idn, rb, _ := setup(t, identity.Config{})
	return idn, rb.Policy()
}

// user creates an identity account holding roles and returns its id.
func user(t *testing.T, idn *identity.Identity, email string, roles ...string) string {
	t.Helper()
	u, err := idn.Users().Create(t.Context(), email, "password123", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) > 0 {
		if err := idn.Users().SetRoles(t.Context(), u.RID, roles); err != nil {
			t.Fatal(err)
		}
	}
	return u.RID
}

// TestPolicy_GrantRoleAndAuthorize: a permission granted to a role
// reaches the users identity gives the role.
func TestPolicy_GrantRoleAndAuthorize(t *testing.T) {
	idn, p := policy(t)
	ctx := context.Background()
	alice, bob := user(t, idn, "alice@test.com", "editor"), user(t, idn, "bob@test.com")
	if ok, err := p.HasPermission(ctx, alice, "task", "read"); err != nil || ok {
		t.Fatalf("no policies deny: %v %v", ok, err)
	}
	if err := p.GrantRole(ctx, "editor", "task", "read"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.HasPermission(ctx, alice, "task", "read"); !ok {
		t.Fatal("alice reads tasks through the editor role")
	}
	if ok, _ := p.HasPermission(ctx, alice, "task", "delete"); ok {
		t.Fatal("no policy grants delete")
	}
	if ok, _ := p.HasPermission(ctx, bob, "task", "read"); ok {
		t.Fatal("bob holds no role")
	}
	// Roles are identity's: taking the role away takes the permission.
	if err := idn.Users().SetRoles(ctx, alice, nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.HasPermission(ctx, alice, "task", "read"); ok {
		t.Fatal("without the role alice reads nothing")
	}
}

// TestPolicy_UnknownUserHasNoPermission: HasPermission reads the user's
// roles from identity; a user it does not know has none, and no error.
func TestPolicy_UnknownUserHasNoPermission(t *testing.T) {
	_, p := policy(t)
	ctx := context.Background()
	if err := p.GrantUser(ctx, "usr_ghost", "task", "read"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"usr_ghost", ""} {
		if ok, err := p.HasPermission(ctx, id, "task", "read"); ok || err != nil {
			t.Errorf("HasPermission(%q) = %v %v", id, ok, err)
		}
		if ok, err := p.HasPermissionInDomain(ctx, id, "task", "read", "ws_1"); ok || err != nil {
			t.Errorf("HasPermissionInDomain(%q) = %v %v", id, ok, err)
		}
	}
}

func TestPolicy_DomainScopedRoles(t *testing.T) {
	idn, p := policy(t)
	ctx := context.Background()
	alice := user(t, idn, "alice@test.com")
	if err := p.AddUserToRoleInDomain(ctx, alice, "owner", "ws_abc"); err != nil {
		t.Fatal(err)
	}
	if err := p.GrantRoleInDomain(ctx, "owner", "task", "read", "ws_abc"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.HasPermissionInDomain(ctx, alice, "task", "read", "ws_abc"); !ok {
		t.Fatal("alice is owner in ws_abc")
	}
	if ok, _ := p.HasPermissionInDomain(ctx, alice, "task", "read", "ws_def"); ok {
		t.Fatal("a tenant's role does not leak into another tenant")
	}
	if ok, _ := p.HasPermission(ctx, alice, "task", "read"); ok {
		t.Fatal("nor into the global scope")
	}
}

// TestPolicy_GlobalRolesPassThroughDomains: a global role's grants count
// in every domain, and the built-in admin needs no grant at all.
func TestPolicy_GlobalRolesPassThroughDomains(t *testing.T) {
	idn, p := policy(t)
	ctx := context.Background()
	super, admin := user(t, idn, "super@test.com", "superadmin"), user(t, idn, "admin@test.com", "admin")
	if err := p.GrantRole(ctx, "superadmin", "*", "*"); err != nil {
		t.Fatal(err)
	}
	for _, dom := range []string{"ws_abc", "ws_def", "tenant_42"} {
		for who, id := range map[string]string{"superadmin": super, "admin": admin} {
			if ok, err := p.HasPermissionInDomain(ctx, id, "task", "read", dom); err != nil || !ok {
				t.Errorf("a global %s in %s: %v %v", who, dom, ok, err)
			}
		}
	}
	if ok, err := p.HasPermission(ctx, admin, "anything", "delete"); err != nil || !ok {
		t.Fatalf("admin may do anything: %v %v", ok, err)
	}
}

func TestPolicy_GrantUserDirect(t *testing.T) {
	idn, p := policy(t)
	ctx := context.Background()
	bob := user(t, idn, "bob@test.com")
	if err := p.GrantUser(ctx, bob, "task", "read"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.HasPermission(ctx, bob, "task", "read"); !ok {
		t.Fatal("a direct grant authorizes without a role")
	}
	if err := p.RevokeUser(ctx, bob, "task", "read"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.HasPermission(ctx, bob, "task", "read"); ok {
		t.Fatal("revoked")
	}
}

func TestPolicy_RejectsGlobalAsTenant(t *testing.T) {
	_, p := policy(t)
	ctx := context.Background()
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
	// Global roles are identity's: the error says where they live.
	if err := p.AddUserToRoleInDomain(ctx, "u", "r", "*"); err == nil || !strings.Contains(err.Error(), "Users().SetRoles") {
		t.Errorf("the error points to identity: %v", err)
	}
}

func TestPolicy_EmptyDomainIsGlobal(t *testing.T) {
	idn, p := policy(t)
	ctx := context.Background()
	x := user(t, idn, "x@test.com")
	if err := p.AddUserToRoleInDomain(ctx, x, "member", ""); err != nil {
		t.Fatal(err)
	}
	if err := p.GrantRoleInDomain(ctx, "member", "task", "read", ""); err != nil {
		t.Fatal(err)
	}
	if roles, err := p.UserRolesInDomain(ctx, x, ""); err != nil || len(roles) != 1 || roles[0] != "member" {
		t.Fatalf("roles = %v %v", roles, err)
	}
	if ok, _ := p.HasPermission(ctx, x, "task", "read"); !ok {
		t.Fatal("a grant in domain \"\" is global")
	}
}

func TestPolicy_ListsAndRevokes(t *testing.T) {
	_, p := policy(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(p.AddUserToRoleInDomain(ctx, "usr_a", "editor", "ws_2"))
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
	must(p.RemoveUserFromRoleInDomain(ctx, "usr_a", "editor", "ws_2"))
	must(p.RemoveUserFromRoleInDomain(ctx, "usr_a", "owner", "ws_1"))
	must(p.RevokeRoleInDomain(ctx, "owner", "ws", "manage", "ws_1"))
	if doms, _ := p.DomainsForUser(ctx, "usr_a"); len(doms) != 0 {
		t.Fatalf("after the removals: %v", doms)
	}
	must(p.ReloadPolicy(ctx))
}

// TestPolicy_NoGlobalRoleAPI: the global role bindings and BootstrapAdmin
// are gone — global roles are identity's, and auth.admins seeds admin.
func TestPolicy_NoGlobalRoleAPI(t *testing.T) {
	typ := reflect.TypeFor[*rbac.PolicyService]()
	for _, name := range []string{"AddUserToRole", "RemoveUserFromRole", "UserRoles", "BootstrapAdmin"} {
		if _, ok := typ.MethodByName(name); ok {
			t.Errorf("PolicyService still has %s", name)
		}
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

// TestMustNew_PanicsOnError: MustNew is New that panics, as the other
// batteries' are.
func TestMustNew_PanicsOnError(t *testing.T) {
	d := dbtest.Open(t)
	func() {
		defer func() {
			if p := recover(); p == nil || !strings.Contains(fmt.Sprint(p), "rbac: New needs the Identity and the database") {
				t.Fatalf("recovered %v", p)
			}
		}()
		rbac.MustNew(nil, d)
	}()
	idn := identity.MustNew(identity.Config{SigningKey: key}, d)
	t.Cleanup(func() { _ = idn.Close(context.Background()) })
	rb := rbac.MustNew(idn, d)
	t.Cleanup(func() { _ = rb.Close(context.Background()) })
	if rb.Name() != "rbac" {
		t.Fatalf("MustNew = %v", rb)
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
	if err := rb.Policy().GrantRole(context.Background(), "editor", "post", "write"); err != nil {
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

// asPrincipal stores p on every request, as an application's own
// authentication would: rbac decides on any principal, not only one
// identity stored.
func asPrincipal(p chok.Principal) chok.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(chok.WithPrincipal(r.Context(), p)))
		})
	}
}

// TestRequire_RolesFromThePrincipal: Require reads the principal's Roles —
// admin passes everything in every domain, a role passes with what
// GrantRole gave it — and its UserID for the direct grants.
func TestRequire_RolesFromThePrincipal(t *testing.T) {
	idn, rb, _ := setup(t, identity.Config{})
	ctx := context.Background()
	if err := rb.Policy().GrantRole(ctx, "editor", "task", "write"); err != nil {
		t.Fatal(err)
	}
	if err := rb.Policy().GrantUser(ctx, "usr_direct", "task", "write"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		p    chok.Principal
		want int
	}{
		{"admin", chok.Principal{UserID: "usr_admin", Roles: []string{"admin"}}, http.StatusOK},
		{"a granted role", chok.Principal{UserID: "usr_editor", Roles: []string{"viewer", "editor"}}, http.StatusOK},
		{"a direct grant", chok.Principal{UserID: "usr_direct"}, http.StatusOK},
		{"no role", chok.Principal{UserID: "usr_nobody"}, http.StatusForbidden},
		{"a role without the grant", chok.Principal{UserID: "usr_viewer", Roles: []string{"viewer"}}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := choktest.App(t, chok.Base{}, func(app *chok.App) {
				app.Use(idn, rb)
				api := app.Group("/api", asPrincipal(tc.p))
				chok.POST(api, "/tasks", ok, chok.Use(rb.Require("task", "write")))
				chok.POST(api, "/workspaces/{ws}/tasks", ok, chok.Use(rb.RequireInDomain("task", "write", "ws")))
			})
			if code := c.PostJSON("/api/tasks", nil, nil); code != tc.want {
				t.Errorf("Require = %d, want %d", code, tc.want)
			}
			// Global grants and admin count in every domain.
			if code := c.PostJSON("/api/workspaces/ws_9/tasks", nil, nil); code != tc.want {
				t.Errorf("RequireInDomain = %d, want %d", code, tc.want)
			}
		})
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
	if err := rb.Policy().AddUserToRoleInDomain(ctx, "usr_root", "owner", "ws_1"); err != nil {
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
	if bind := find(url.Values{"action": {"authz.AddUserToRoleInDomain"}}); len(bind) != 1 || bind[0].Metadata["role"] != "owner" {
		t.Fatalf("the binding is recorded: %+v", bind)
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
	// The admin role reads the log without a grant: rbac lets it do
	// everything.
	if err := rb.Policy().RevokeUser(context.Background(), uid, "audit", "read"); err != nil {
		t.Fatal(err)
	}
	if err := idn.Users().SetRoles(context.Background(), uid, []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	var fresh struct {
		Token string `json:"token"`
	}
	if code := c.PostJSON("/auth/login", map[string]string{"email": "alice@test.com", "password": "password123"}, &fresh); code != http.StatusOK {
		t.Fatalf("login = %d", code)
	}
	if code := c.WithBearer(fresh.Token).GetJSON("/audit/logs", nil); code != http.StatusOK {
		t.Fatalf("an admin = %d", code)
	}
}

func TestWatcher_PeersReload(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := dbtest.Config(t)
	// Two replicas: two handles on one database, one Redis.
	var idnA *identity.Identity
	replica := func() *rbac.RBAC {
		d := dbtest.OpenConfig(t, cfg)
		idn, err := identity.New(identity.Config{SigningKey: key}, d)
		if err != nil {
			t.Fatal(err)
		}
		if idnA == nil {
			idnA = idn
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
	alice := user(t, idnA, "alice@test.com")
	if err := a.Policy().GrantUser(ctx, alice, "task", "read"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ok, _ := b.Policy().HasPermission(ctx, alice, "task", "read"); ok {
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
