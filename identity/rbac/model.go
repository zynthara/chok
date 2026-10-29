package rbac

// rbacWithDomainsModel is the default Casbin model: role-based access
// with domains.
//
//	r = sub, dom, obj, act    the request: who, where, what, how
//	p = sub, dom, obj, act    a policy, of the request's shape
//	g = _, _, _               a role binding: user, role, domain
//
// The matcher admits a request when a policy's subject is the request's
// — the user (GrantUser) or one of identity's roles, which decide asks
// about in turn (GrantRole) — or a role the user holds in the request's
// tenant (AddUserToRoleInDomain); the policy's domain is the request's or
// "*" (global, valid everywhere); the object matches with keyMatch
// ("post.*" covers "post.read", "*" everything); and the action is the
// request's or "*". A role binding in the global scope "*", which v2's
// AddUserToRole wrote, counts for nothing: the global roles are
// identity's.
const rbacWithDomainsModel = `[request_definition]
r = sub, dom, obj, act

[policy_definition]
p = sub, dom, obj, act

[role_definition]
g = _, _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = (r.sub == p.sub || (r.dom != "*" && g(r.sub, p.sub, r.dom))) && (r.dom == p.dom || p.dom == "*") && keyMatch(r.obj, p.obj) && (r.act == p.act || p.act == "*")
`
