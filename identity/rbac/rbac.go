// Package rbac is role-based access control for identity's users, on
// casbin: what each role may do, stored in the application's database and
// decided per request by Require.
//
//	auth := identity.MustNew(cfg.Auth, d)
//	rb := rbac.MustNew(auth, d)
//	app.Use(d, auth, rb)
//	api := app.Group("/api", auth.Authn())
//	chok.POST(api, "/posts", createPost, chok.Use(rb.Require("post", "write")))
//
//	rb.Policy().GrantRole(ctx, "editor", "post", "write")  // what the role may do
//	auth.Users().SetRoles(ctx, userID, []string{"editor"})  // who holds it
//
// There is one set of global roles, identity's: a user's roles are the
// chok.Principal's Roles, which Authn reads from the database on every
// request. A decision admits the principal when its roles hold admin —
// the built-in role that may do everything, everywhere, and the one
// auth.admins grants — or when a policy allows its UserID (GrantUser, a
// role bound in the domain) or any of its roles (GrantRole). Roles within
// a tenant are casbin's: AddUserToRoleInDomain.
//
// The policies live in the table casbin_rule, registered under the owner
// "rbac": migrate auto creates it, versioned applies the SQL embedded in
// this package. With identity's audit log on, every policy change and
// every denied request is recorded in it, and the audit log's admin API
// asks this package for audit/read.
//
// casbin is imported here and nowhere else in chok: an application that
// does not import identity/rbac does not link it.
package rbac

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"

	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/identity/internal/authz"
	"github.com/zynthara/chok/v3/internal/dbx"
)

// watcherChannel is the Redis channel replicas announce policy changes on.
const watcherChannel = "chok:authz:policy"

// RBAC decides who may do what. Register it with app.Use: it reports
// health and, on close, stops the watcher.
type RBAC struct {
	idn    *identity.Identity
	e      *engine
	policy *PolicyService
}

// Option configures New.
type Option func(*options)

type options struct {
	watcher *goredis.Client
	model   string
}

// Watcher makes policy changes reach the other replicas: each change is
// announced on the Redis channel chok:authz:policy, and a replica that
// hears one reloads its policies. One replica needs no watcher.
func Watcher(client *goredis.Client) Option { return func(o *options) { o.watcher = client } }

// Model replaces the default model — roles with domains, see
// PolicyService — with casbin model text. A custom model must keep the
// request (sub, dom, obj, act) for Require and the PolicyService methods,
// and at most six policy fields.
func Model(text string) Option { return func(o *options) { o.model = text } }

// migrationFiles are the SQL migrations for migrate: versioned, one
// directory per dialect: casbin_rule as migrate: auto creates it.
//
//go:embed migrations
var migrationFiles embed.FS

func migrationsFS() (fs.FS, error) { return fs.Sub(migrationFiles, "migrations") }

// The chok command reports and applies rbac's migration through dbx, for
// an application that runs with migrate: off.
func init() {
	sub, err := migrationsFS()
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	dbx.Migrations["rbac"] = sub
}

// casbinTable declares casbin_rule: a plain table, the layout casbin's.
func casbinTable() db.TableSpec { return dbx.PlainTable(&casbinRule{}).(db.TableSpec) }

// New builds the access control of idn's users on d: it registers the
// casbin_rule table under the owner "rbac", loads the policies, subscribes
// the watcher and, when idn's audit log is on, checks that it can write
// and records policy changes and denials in it.
func New(idn *identity.Identity, d *db.DB, opts ...Option) (*RBAC, error) {
	if idn == nil || d == nil {
		return nil, errors.New("rbac: New needs the Identity and the database")
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.model == "" {
		o.model = rbacWithDomainsModel
	}
	sub, err := migrationsFS()
	if err != nil {
		return nil, err
	}
	if err := d.RegisterSchema("rbac", []db.TableSpec{casbinTable()}, sub); err != nil {
		return nil, fmt.Errorf("rbac: %w", err)
	}
	adapter, err := newGormAdapter(func() *gorm.DB { return d.Unsafe(context.Background()) })
	if err != nil {
		return nil, err
	}
	e, err := newEngine(o.model, adapter, d.Logger())
	if err != nil {
		return nil, err
	}
	r := &RBAC{idn: idn, e: e, policy: &PolicyService{e: e, idn: idn}}
	if o.watcher != nil {
		if err := e.attachRedisWatcher(context.Background(), o.watcher, watcherChannel); err != nil {
			e.close()
			return nil, err
		}
	}
	if audit := idn.Audit(); audit.Enabled() {
		// The log must work before the first change goes unrecorded.
		if err := audit.LogSync(context.Background(), identity.Event{Action: "authz.audit.enabled", Resource: "casbin_rule", ActorType: identity.ActorSystem}); err != nil {
			e.close()
			return nil, fmt.Errorf("rbac: the audit log does not take records: %w", err)
		}
		e.attachAudit(func(ctx context.Context, action, role, obj, act string) {
			audit.Log(ctx, identity.Event{
				Action: "authz." + action, Resource: "casbin_rule",
				Metadata: map[string]any{"role": role, "object": obj, "action": act},
			})
		})
	}
	authz.Attach(idn, func(_ context.Context, p chok.Principal, obj, act string) (bool, error) {
		return e.decide(p.UserID, p.Roles, "", obj, act)
	})
	return r, nil
}

// MustNew is New that panics on error.
func MustNew(idn *identity.Identity, d *db.DB, opts ...Option) *RBAC {
	r, err := New(idn, d, opts...)
	if err != nil {
		panic(err)
	}
	return r
}

// Policy manages the roles and permissions.
func (r *RBAC) Policy() *PolicyService { return r.policy }

// Require is the middleware that admits the request's chok.Principal to
// act on obj: when its Roles hold admin, or a global policy allows its
// UserID or one of its Roles. No principal is a 401, a refusal a 403, a
// failed decision a 500. Put it behind identity's Authn — any middleware
// that stores a principal with a UserID will do.
func (r *RBAC) Require(obj, act string) chok.Middleware {
	return r.require(obj, act, "")
}

// RequireInDomain is Require within a tenant: the domain is the path
// parameter domainParam ({wsid} in /workspaces/{wsid}/posts). The roles a
// user holds in the domain (AddUserToRoleInDomain) count there, a global
// policy counts in every domain, and admin passes in all of them. A route
// without the parameter is a 400.
func (r *RBAC) RequireInDomain(obj, act, domainParam string) chok.Middleware {
	if domainParam == "" {
		panic("rbac.RequireInDomain: name the path parameter that holds the domain")
	}
	return r.require(obj, act, domainParam)
}

func (r *RBAC) require(obj, act, domainParam string) chok.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := req.Context()
			p, ok := chok.PrincipalFrom(ctx)
			if !ok || p.UserID == "" {
				chok.WriteError(w, req, chok.ErrUnauthorized)
				return
			}
			var dom string
			if domainParam != "" {
				if dom = req.PathValue(domainParam); dom == "" {
					chok.WriteError(w, req, chok.ErrBadRequest.Msgf("the path has no domain parameter %q", domainParam))
					return
				}
			}
			allowed, err := r.e.decide(p.UserID, p.Roles, dom, obj, act)
			if err != nil {
				chok.LoggerFrom(ctx).ErrorContext(ctx, "rbac: the decision failed", "error", err, "subject", p.UserID, "domain", dom, "obj", obj, "act", act)
				chok.WriteError(w, req, chok.ErrInternal.Wrap(err))
				return
			}
			if !allowed {
				if audit := r.idn.Audit(); audit.Enabled() {
					audit.Log(ctx, identity.Event{
						Action: "authz.deny", Result: identity.ResultDenied, Resource: obj,
						Metadata: map[string]any{"action": act, "domain": dom},
					})
				}
				chok.WriteError(w, req, chok.ErrForbidden)
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}

// Name is "rbac", its name in the health report.
func (r *RBAC) Name() string { return "rbac" }

// Health is nil: the policies are in memory once New returns; a failed
// reload after a peer's change is logged and counted, and the last good
// policies stay in force.
func (r *RBAC) Health(context.Context) error { return nil }

// Close stops the watcher and the audit hook.
func (r *RBAC) Close(context.Context) error {
	r.e.close()
	return nil
}
