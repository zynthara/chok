package log

import (
	"context"
	"testing"
)

func TestWithContext_FromContext_Roundtrip(t *testing.T) {
	l := Empty()
	ctx := WithContext(context.Background(), l)

	got := FromContext(ctx)
	if got == nil {
		t.Fatal("FromContext returned nil after WithContext")
	}
	if got != l {
		t.Fatalf("FromContext returned different logger: got %v, want %v", got, l)
	}
}

func TestFromContext_EmptyCtx_ReturnsNil(t *testing.T) {
	got := FromContext(context.Background())
	if got != nil {
		t.Fatalf("FromContext(empty) = %v, want nil", got)
	}
}

func TestWithContext_OverwritesPrevious(t *testing.T) {
	l1 := Empty()
	l2 := Empty().With("key", "val")

	ctx := WithContext(context.Background(), l1)
	ctx = WithContext(ctx, l2)

	got := FromContext(ctx)
	if got != l2 {
		t.Fatal("FromContext should return the most recently set logger")
	}
}
