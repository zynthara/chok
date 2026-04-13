package authz

import (
	"context"
	"testing"
)

func TestAuthorizerFunc(t *testing.T) {
	called := false
	f := AuthorizerFunc(func(_ context.Context, sub, obj, act string) (bool, error) {
		called = true
		if sub != "user1" || obj != "/api" || act != "GET" {
			t.Fatalf("unexpected args: sub=%q obj=%q act=%q", sub, obj, act)
		}
		return true, nil
	})

	allowed, err := f.Authorize(context.Background(), "user1", "/api", "GET")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("expected allowed")
	}
	if !called {
		t.Fatal("function was not called")
	}
}

func TestAuthorizerFunc_Denied(t *testing.T) {
	f := AuthorizerFunc(func(_ context.Context, _, _, _ string) (bool, error) {
		return false, nil
	})
	allowed, err := f.Authorize(context.Background(), "x", "y", "z")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("expected denied")
	}
}
