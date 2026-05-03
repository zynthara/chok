// Package authz defines the authorization interface.
//
// The framework provides only the interface and a function adapter.
// Concrete implementations (Casbin, OPA, custom logic) are provided by the
// application or optional sub-packages — the blessed implementation is
// authz/casbin (Phase 6).
package authz

import "context"

// Authorizer makes authorization decisions for the global / single-tenant
// case. Multi-tenant deployments wanting per-domain policy should
// additionally implement DomainAuthorizer; chok's middleware
// RequireAuthzInDomain does a type assertion and fails closed (500)
// when the configured Authorizer doesn't satisfy that interface.
type Authorizer interface {
	// Authorize checks whether subject may perform action on object.
	Authorize(ctx context.Context, subject, object, action string) (bool, error)
}

// DomainAuthorizer is the optional multi-tenant extension of Authorizer.
// Implementations that want to be usable with
// middleware.RequireAuthzInDomain MUST implement both Authorize (from
// Authorizer) and AuthorizeInDomain. The casbin-backed *casbinAuthorizer
// satisfies both naturally; pure-function AuthorizerFunc adapters stay
// at single-tenant Authorize.
//
// Empty domain ("") is treated as the global scope by both the
// middleware (it normalizes "" → "*") and the casbin Service layer.
// Implementations that don't normalize internally still interoperate
// correctly because middleware.RequireAuthz forwards a literal "" via
// Authorize, never AuthorizeInDomain.
type DomainAuthorizer interface {
	Authorizer
	AuthorizeInDomain(ctx context.Context, subject, domain, object, action string) (bool, error)
}

// AuthorizerFunc adapts a plain function into an Authorizer. It does
// NOT implement DomainAuthorizer — multi-tenant routes need either a
// real Casbin enforcer or a custom struct that wires both methods.
type AuthorizerFunc func(ctx context.Context, subject, object, action string) (bool, error)

// Authorize implements the Authorizer interface.
func (f AuthorizerFunc) Authorize(ctx context.Context, sub, obj, act string) (bool, error) {
	return f(ctx, sub, obj, act)
}

// DomainAuthorizerFunc adapts a plain function into a DomainAuthorizer.
// Useful for tests that want a one-line stub against tenant-scoped
// middleware. The Authorize method (single-tenant) is implemented by
// forwarding with domain="".
type DomainAuthorizerFunc func(ctx context.Context, subject, domain, object, action string) (bool, error)

// Authorize implements the Authorizer half of DomainAuthorizer by
// passing an empty domain through.
func (f DomainAuthorizerFunc) Authorize(ctx context.Context, sub, obj, act string) (bool, error) {
	return f(ctx, sub, "", obj, act)
}

// AuthorizeInDomain implements the DomainAuthorizer half.
func (f DomainAuthorizerFunc) AuthorizeInDomain(ctx context.Context, sub, dom, obj, act string) (bool, error) {
	return f(ctx, sub, dom, obj, act)
}

// Compile-time interface assertions keep the adapter contracts honest
// across refactors.
var (
	_ Authorizer       = (AuthorizerFunc)(nil)
	_ Authorizer       = (DomainAuthorizerFunc)(nil)
	_ DomainAuthorizer = (DomainAuthorizerFunc)(nil)
)
