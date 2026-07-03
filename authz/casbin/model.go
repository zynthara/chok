package casbin

// rbacWithDomainsModel is the embedded Casbin model for chok's
// blessed RBAC-with-domains scheme.
//
// Structure:
//
//	r = sub, dom, obj, act         — request: who/where/what/how
//	p = sub, dom, obj, act         — policy: matches request shape
//	g = _, _, _                    — role assignment: user, role, domain
//	e = some(where (p.eft == allow))
//
// Matcher (SPEC §7.7 v0.3.4):
//  1. Subject: equal-string OR role-binding-in-domain OR
//     role-binding-globally. The string-equality clause lets
//     Service.GrantUser write `p(userID, ...)` directly without a
//     role; the two g(...) clauses pick up role-mediated grants
//     both inside the request domain and via a global "*" binding.
//  2. Domain: exact match OR policy declares "*" (global, applies
//     everywhere). Service-level normalisation maps incoming
//     domain="" to "*" so the matcher sees a consistent vocabulary.
//  3. Object: keyMatch (Casbin's URL-style prefix wildcard). Use
//     "task.*" / "task.read" / "*"; see SPEC §7.9 for the exact
//     semantics and pitfalls of "task*" vs "task.*".
//  4. Action: exact OR policy declares "*".
//
// The double-newline split makes the embedded string easy to read in
// godoc output.
const rbacWithDomainsModel = `[request_definition]
r = sub, dom, obj, act

[policy_definition]
p = sub, dom, obj, act

[role_definition]
g = _, _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = (r.sub == p.sub || g(r.sub, p.sub, r.dom) || g(r.sub, p.sub, "*")) && (r.dom == p.dom || p.dom == "*") && keyMatch(r.obj, p.obj) && (r.act == p.act || p.act == "*")
`
