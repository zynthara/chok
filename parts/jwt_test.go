package parts

import (
	"context"
	"testing"

	"github.com/zynthara/chok/auth/jwt"
	"github.com/zynthara/chok/component"
)

func TestJWTComponent_Init_ExposesManager(t *testing.T) {
	c := NewJWTComponent("jwt", func(component.Kernel) (*jwt.Manager, error) {
		return jwt.NewManager(jwt.Options{SigningKey: "this-is-a-test-signing-key-32byt"})
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	mgr := c.Manager()
	if mgr == nil {
		t.Fatal("Manager() should not be nil after Init")
	}
	// Sanity: manager can sign + parse.
	tok, _, err := mgr.Sign("sub", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.Parse(tok); err != nil {
		t.Fatalf("round-trip failed: %v", err)
	}
}

func TestJWTComponent_BuilderError(t *testing.T) {
	c := NewJWTComponent("jwt", func(component.Kernel) (*jwt.Manager, error) {
		// Empty key → constructor error.
		return jwt.NewManager(jwt.Options{})
	})
	if err := c.Init(context.Background(), newMockKernel(nil)); err == nil {
		t.Fatal("expected builder error to surface")
	}
}

func TestJWTComponent_NilManager_Rejected(t *testing.T) {
	c := NewJWTComponent("jwt", func(component.Kernel) (*jwt.Manager, error) { return nil, nil })
	if err := c.Init(context.Background(), newMockKernel(nil)); err == nil {
		t.Fatal("nil manager should be rejected")
	}
}

func TestJWTComponent_CustomName(t *testing.T) {
	c := NewJWTComponent("reset-jwt", func(component.Kernel) (*jwt.Manager, error) {
		return jwt.NewManager(jwt.Options{SigningKey: "this-is-a-test-signing-key-32byt"})
	})
	if c.Name() != "reset-jwt" {
		t.Fatalf("Name should be %q, got %q", "reset-jwt", c.Name())
	}
	if c.ConfigKey() != "reset-jwt" {
		t.Fatalf("ConfigKey should be %q, got %q", "reset-jwt", c.ConfigKey())
	}
}
