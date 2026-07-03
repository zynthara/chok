package apierr

import (
	"context"
	"testing"
)

func TestRenderHook_RunsInRegistrationOrder(t *testing.T) {
	reg := NewMapperRegistry()
	var order []string
	reg.RegisterRenderHook(func(_ context.Context, ae *Error) {
		order = append(order, "first")
		if ae.Message == "" {
			ae.Message = "from-first"
		}
	})
	reg.RegisterRenderHook(func(_ context.Context, ae *Error) {
		order = append(order, "second")
		if ae.Message == "" {
			ae.Message = "from-second"
		}
	})

	ae := &Error{Code: 400, Reason: "InvalidArgument"}
	reg.Render(context.Background(), ae)

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("hooks ran out of order: %v", order)
	}
	if ae.Message != "from-first" {
		t.Fatalf("first hook to set Message should win, got %q", ae.Message)
	}
}

func TestRenderHook_NilErrorIsNoop(t *testing.T) {
	reg := NewMapperRegistry()
	called := false
	reg.RegisterRenderHook(func(context.Context, *Error) { called = true })
	reg.Render(context.Background(), nil)
	if called {
		t.Fatal("hooks must not run for a nil *Error")
	}
}

func TestRegisterRenderHook_NilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil hook")
		}
	}()
	NewMapperRegistry().RegisterRenderHook(nil)
}

func TestRenderWithContext_UsesContextRegistry(t *testing.T) {
	reg := NewMapperRegistry()
	reg.RegisterRenderHook(func(_ context.Context, ae *Error) {
		ae.Message = "localized"
	})
	ctx := WithMapperRegistry(context.Background(), reg)

	ae := &Error{Code: 404, Reason: "NotFound"}
	RenderWithContext(ctx, ae)
	if ae.Message != "localized" {
		t.Fatalf("hook did not run via context registry, message=%q", ae.Message)
	}
}

func TestRenderWithContext_NoRegistryIsNoop(t *testing.T) {
	ae := &Error{Code: 404, Reason: "NotFound"}
	RenderWithContext(context.Background(), ae) // must not panic
	if ae.Message != "" {
		t.Fatalf("no registry ⇒ untouched, got %q", ae.Message)
	}
}

func TestRenderHook_ReadsRequestContext(t *testing.T) {
	type langKey struct{}
	reg := NewMapperRegistry()
	reg.RegisterRenderHook(func(ctx context.Context, ae *Error) {
		if lang, _ := ctx.Value(langKey{}).(string); lang == "zh" {
			ae.Message = "未找到"
		}
	})
	ctx := WithMapperRegistry(context.Background(), reg)
	ctx = context.WithValue(ctx, langKey{}, "zh")

	ae := &Error{Code: 404, Reason: "NotFound"}
	RenderWithContext(ctx, ae)
	if ae.Message != "未找到" {
		t.Fatalf("hook should read ctx values, got %q", ae.Message)
	}
}
