// Package authz defines the authorization interface.
//
// The framework provides only the interface and a function adapter.
// Concrete implementations (Casbin, OPA, custom logic) are provided by the
// application or optional sub-packages.
package authz

import "context"

// Authorizer makes authorization decisions.
type Authorizer interface {
	// Authorize checks whether subject may perform action on object.
	Authorize(ctx context.Context, subject, object, action string) (bool, error)
}

// AuthorizerFunc adapts a plain function into an Authorizer.
type AuthorizerFunc func(ctx context.Context, subject, object, action string) (bool, error)

// Authorize implements the Authorizer interface.
func (f AuthorizerFunc) Authorize(ctx context.Context, sub, obj, act string) (bool, error) {
	return f(ctx, sub, obj, act)
}
