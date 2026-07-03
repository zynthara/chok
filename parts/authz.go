package parts

import (
	"context"
	"fmt"
	"io"

	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/component"
)

// AuthzBuilder constructs the application's Authorizer. Typical impls:
// a Casbin enforcer (chok's blessed implementation, see
// authz/casbin.Builder), an OPA client, or a bespoke AuthorizerFunc
// over the user's role table.
type AuthzBuilder func(k component.Kernel) (authz.Authorizer, error)

// AuthzComponent holds the configured authz.Authorizer and ferries it
// into HTTPComponent's middleware chain via OptionalDependencies. Like
// JWTComponent it intentionally offers no Reload — authz policy
// reloads are the Authorizer's own responsibility (Casbin's Watcher,
// OPA bundle polling, etc. all manage their own reload semantics).
//
// SPEC v0.3.4 added Dependencies / OptionalDependencies + Close so
// the casbin Authorizer can declare hard dep on "db" and soft dep on
// "redis" / "audit", and so its Watcher subscriber + adapter pool
// release on App.Stop.
type AuthzComponent struct {
	build   AuthzBuilder
	auth    authz.Authorizer
	deps    []string // hard
	optDeps []string // soft
}

// NewAuthzComponent builds the component with the supplied factory.
//
// Use chained WithDependencies / WithOptionalDependencies to declare
// which other components must be initialised first (or which are
// optional sources of capability for the Authorizer). Casbin's
// blessed wiring is:
//
//	parts.NewAuthzComponent(casbin.Builder(opts)).
//	    WithDependencies("db").
//	    WithOptionalDependencies("redis", "audit")
//
// Without WithDependencies, the registry's topological sort can run
// the builder before "db" Init completes — the builder would crash
// reading a nil *gorm.DB. The optional deps let Builder nil-check
// "redis" (Watcher needs it) and "audit" (policy-change events).
func NewAuthzComponent(build AuthzBuilder) *AuthzComponent {
	return &AuthzComponent{build: build}
}

// WithDependencies appends names to the hard-dependency list. Must be
// called before App.Run / Registry.Start; calling it after has no
// effect because dependency planning runs before Init.
func (a *AuthzComponent) WithDependencies(names ...string) *AuthzComponent {
	a.deps = append(a.deps, names...)
	return a
}

// WithOptionalDependencies appends names to the soft-dependency list.
// The component still Init's even when these are absent; Builder
// implementations are expected to query them via the Kernel and
// nil-check.
func (a *AuthzComponent) WithOptionalDependencies(names ...string) *AuthzComponent {
	a.optDeps = append(a.optDeps, names...)
	return a
}

// Name implements component.Component.
func (a *AuthzComponent) Name() string { return "authz" }

// ConfigKey implements component.Component.
func (a *AuthzComponent) ConfigKey() string { return "authz" }

// Dependencies implements component.Dependent.
func (a *AuthzComponent) Dependencies() []string { return a.deps }

// OptionalDependencies implements component.OptionalDependent.
func (a *AuthzComponent) OptionalDependencies() []string { return a.optDeps }

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

// Close releases any resources the underlying Authorizer holds. Casbin
// implementations close their Watcher subscriber + clear audit hooks;
// pure-function authorizers without io.Closer are no-ops.
//
// The returned error surfaces in registry.Stop's aggregate so an
// operator sees a Watcher / adapter teardown failure without blocking
// other components from shutting down.
func (a *AuthzComponent) Close(ctx context.Context) error {
	if a.auth == nil {
		return nil
	}
	closer, ok := a.auth.(io.Closer)
	if !ok {
		return nil
	}
	return closer.Close()
}

// Authorizer returns the underlying interface, or nil before Init.
func (a *AuthzComponent) Authorizer() authz.Authorizer { return a.auth }

// Compile-time interface assertions.
var (
	_ component.Component         = (*AuthzComponent)(nil)
	_ component.Dependent         = (*AuthzComponent)(nil)
	_ component.OptionalDependent = (*AuthzComponent)(nil)
)
