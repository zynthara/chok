package log

import (
	"context"
	"testing"

	"github.com/zynthara/chok/v2/kernel"
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

func TestFrom_KernelRootLoggerAndFallback(t *testing.T) {
	// The real path: the registry's logger is a log.Logger.
	root := Empty().With("app", "t")
	if got := From(stubKernel{l: root}); got != root {
		t.Fatal("From must hand back the kernel's rich logger")
	}
	// Poorer harness logger: fall back to Empty, never nil.
	if got := From(stubKernel{l: poorLogger{}}); got == nil {
		t.Fatal("From must never return nil")
	}
}

// stubKernel overrides only Logger(); other Kernel methods are never
// touched by From.
type stubKernel struct {
	kernel.Kernel
	l kernel.Logger
}

func (s stubKernel) Logger() kernel.Logger { return s.l }

// poorLogger satisfies kernel.Logger but not log.Logger (no With).
type poorLogger struct{}

func (poorLogger) Debug(string, ...any)                         {}
func (poorLogger) Info(string, ...any)                          {}
func (poorLogger) Warn(string, ...any)                          {}
func (poorLogger) Error(string, ...any)                         {}
func (poorLogger) DebugContext(context.Context, string, ...any) {}
func (poorLogger) InfoContext(context.Context, string, ...any)  {}
func (poorLogger) WarnContext(context.Context, string, ...any)  {}
func (poorLogger) ErrorContext(context.Context, string, ...any) {}
