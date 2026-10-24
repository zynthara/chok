// Package authz connects identity/rbac to identity without widening
// identity's API: rbac installs its decision on an Identity, which asks it
// before the audit log's admin API answers.
package authz

import "context"

// Func decides whether subject may perform action on object.
type Func func(ctx context.Context, subject, object, action string) (bool, error)

// Attach, set by package identity when it initializes, installs fn on
// idn, an *identity.Identity.
var Attach func(idn any, fn Func)
