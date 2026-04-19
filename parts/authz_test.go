package parts

import (
	"context"
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
