package parts

import (
	"context"
	"fmt"

	"github.com/zynthara/chok/authz"
	"github.com/zynthara/chok/component"
)

// AuthzBuilder constructs the application's Authorizer. Typical impls:
// a Casbin enforcer, an OPA client, or a bespoke AuthorizerFunc over
// the user's role table.
type AuthzBuilder func(k component.Kernel) (authz.Authorizer, error)

// AuthzComponent holds the configured authz.Authorizer. Like
// JWTComponent it intentionally offers no Reload — authz policy
// reloads are the Authorizer's own responsibility (Casbin, OPA,
// etc. have their own reload mechanisms).
type AuthzComponent struct {
	build AuthzBuilder
	auth  authz.Authorizer
}

// NewAuthzComponent builds the component with the supplied factory.
func NewAuthzComponent(build AuthzBuilder) *AuthzComponent {
	return &AuthzComponent{build: build}
}

// Name implements component.Component.
func (a *AuthzComponent) Name() string { return "authz" }

// ConfigKey implements component.Component.
func (a *AuthzComponent) ConfigKey() string { return "authz" }

// Init invokes the builder.
func (a *AuthzComponent) Init(ctx context.Context, k component.Kernel) error {
	az, err := a.build(k)
	if err != nil {
		return fmt.Errorf("authz init: %w", err)
	}
	if az == nil {
		return fmt.Errorf("authz init: builder returned nil Authorizer")
	}
	a.auth = az
	return nil
}

// Close is a no-op; Authorizer implementations with resources should
// close them themselves or be wrapped in a component that does.
func (a *AuthzComponent) Close(ctx context.Context) error { return nil }

// Authorizer returns the underlying interface, or nil before Init.
func (a *AuthzComponent) Authorizer() authz.Authorizer { return a.auth }
