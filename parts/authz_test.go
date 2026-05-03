package parts

import (
	"context"
	"errors"
	"testing"

	"github.com/zynthara/chok/authz"
	"github.com/zynthara/chok/component"
)

func TestAuthzComponent_Init_ExposesAuthorizer(t *testing.T) {
	allowAll := authz.AuthorizerFunc(func(ctx context.Context, s, o, a string) (bool, error) {
		return true, nil
	})
	c := NewAuthzComponent(func(component.Kernel) (authz.Authorizer, error) {
		return allowAll, nil
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if c.Authorizer() == nil {
		t.Fatal("Authorizer() should not be nil")
	}
	ok, err := c.Authorizer().Authorize(context.Background(), "u", "o", "a")
	if err != nil || !ok {
		t.Fatalf("unexpected: ok=%v err=%v", ok, err)
	}
}

func TestAuthzComponent_NilAuthorizer_Rejected(t *testing.T) {
	c := NewAuthzComponent(func(component.Kernel) (authz.Authorizer, error) { return nil, nil })
	if err := c.Init(context.Background(), newMockKernel(nil)); err == nil {
		t.Fatal("nil authorizer should be rejected")
	}
}

// closableAuthorizer is a test stub that implements both Authorizer
// and io.Closer so we can verify AuthzComponent.Close walks the
// type assertion. The casbin Authorizer is the production caller of
// this path; this stub keeps the test free of the casbin/db stack.
type closableAuthorizer struct {
	closed   int
	closeErr error
}

func (c *closableAuthorizer) Authorize(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}

func (c *closableAuthorizer) Close() error {
	c.closed++
	return c.closeErr
}

// TestAuthzComponent_Close_DelegatesToCloser pins the SPEC §7.4 v0.3.4
// behaviour: when the underlying Authorizer implements io.Closer (as
// *casbinAuthorizer does for its Watcher / audit cleanup), the
// component's Close MUST invoke it. Without this, Watcher subscribers
// outlive App.Stop and leak goroutines + Redis connections.
func TestAuthzComponent_Close_DelegatesToCloser(t *testing.T) {
	stub := &closableAuthorizer{}
	c := NewAuthzComponent(func(component.Kernel) (authz.Authorizer, error) {
		return stub, nil
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close returned %v", err)
	}
	if stub.closed != 1 {
		t.Errorf("expected stub.Close called once, got %d", stub.closed)
	}
}

// TestAuthzComponent_Close_PropagatesError verifies the closer error
// surfaces back through Component.Close so the registry's aggregate
// stop error catches Watcher / adapter teardown failures rather than
// swallowing them.
func TestAuthzComponent_Close_PropagatesError(t *testing.T) {
	want := errors.New("watcher subscriber teardown failed")
	stub := &closableAuthorizer{closeErr: want}
	c := NewAuthzComponent(func(component.Kernel) (authz.Authorizer, error) { return stub, nil })
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Close error = %v, want %v wrapped", err, want)
	}
}

// TestAuthzComponent_Close_NoOpForNonCloser covers the AuthorizerFunc
// path: pure-function authorizers don't satisfy io.Closer and Close
// must short-circuit cleanly rather than panic on a failed assertion.
func TestAuthzComponent_Close_NoOpForNonCloser(t *testing.T) {
	c := NewAuthzComponent(func(component.Kernel) (authz.Authorizer, error) {
		return authz.AuthorizerFunc(func(context.Context, string, string, string) (bool, error) {
			return true, nil
		}), nil
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close on non-Closer should be a no-op, got %v", err)
	}
}

// TestAuthzComponent_Close_BeforeInit covers the rollback path: if a
// peer component's Init fails before AuthzComponent.Init runs, the
// registry still calls Close on every registered component during
// rollback. Close must tolerate a nil Authorizer (Init never ran).
func TestAuthzComponent_Close_BeforeInit(t *testing.T) {
	c := NewAuthzComponent(func(component.Kernel) (authz.Authorizer, error) {
		return authz.AuthorizerFunc(func(context.Context, string, string, string) (bool, error) {
			return true, nil
		}), nil
	})
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close before Init should be safe, got %v", err)
	}
}

// TestAuthzComponent_DependenciesChain pins the chained-builder API
// SPEC §7.4 v0.3.4 added: WithDependencies + WithOptionalDependencies
// return the receiver so autoregister can wire "db" hard + "redis" /
// "audit" soft in one expression.
func TestAuthzComponent_DependenciesChain(t *testing.T) {
	c := NewAuthzComponent(func(component.Kernel) (authz.Authorizer, error) { return nil, nil }).
		WithDependencies("db").
		WithOptionalDependencies("redis", "audit")
	if got := c.Dependencies(); len(got) != 1 || got[0] != "db" {
		t.Errorf("Dependencies = %v, want [db]", got)
	}
	if got := c.OptionalDependencies(); len(got) != 2 || got[0] != "redis" || got[1] != "audit" {
		t.Errorf("OptionalDependencies = %v, want [redis audit]", got)
	}
}
