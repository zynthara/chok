package apierr

import (
	"errors"
	"fmt"
	"testing"
)

func TestError_Error(t *testing.T) {
	e := New(400, "Bad", "bad request")
	if e.Error() != "Bad: bad request" {
		t.Fatalf("got %q", e.Error())
	}
}

func TestError_Is_MatchesCodeReason(t *testing.T) {
	copy := ErrNotFound.WithMessage("user not found")
	if !errors.Is(copy, ErrNotFound) {
		t.Fatal("WithMessage copy should match original sentinel")
	}
}

func TestError_Is_DifferentReason(t *testing.T) {
	if errors.Is(ErrNotFound, ErrBind) {
		t.Fatal("different reason should not match")
	}
}

func TestWithMessage_DoesNotMutateOriginal(t *testing.T) {
	orig := ErrNotFound.Message
	_ = ErrNotFound.WithMessage("custom")
	if ErrNotFound.Message != orig {
		t.Fatal("original sentinel was mutated")
	}
}

func TestWithMetadata_DoesNotMutateOriginal(t *testing.T) {
	_ = ErrNotFound.WithMetadata("key", "val")
	if ErrNotFound.Metadata != nil {
		t.Fatal("original sentinel was mutated")
	}
}

func TestWithMetadata_CopiesExisting(t *testing.T) {
	a := ErrBind.WithMetadata("a", 1)
	b := a.WithMetadata("b", 2)
	if b.Metadata["a"] != 1 || b.Metadata["b"] != 2 {
		t.Fatalf("metadata not merged: %v", b.Metadata)
	}
	// a should not have "b".
	if _, ok := a.Metadata["b"]; ok {
		t.Fatal("chaining mutated earlier copy")
	}
}

func TestFromError_Nil(t *testing.T) {
	if FromError(nil) != nil {
		t.Fatal("expected nil")
	}
}

func TestFromError_APIError(t *testing.T) {
	err := ErrNotFound.WithMessage("gone")
	got := FromError(err)
	if got.Code != 404 || got.Message != "gone" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestFromError_WrappedAPIError(t *testing.T) {
	wrapped := fmt.Errorf("context: %w", ErrNotFound)
	got := FromError(wrapped)
	if got.Code != 404 {
		t.Fatalf("expected 404, got %d", got.Code)
	}
}

func TestFromError_GenericError(t *testing.T) {
	got := FromError(errors.New("boom"))
	if got.Code != 500 {
		t.Fatalf("expected 500, got %d", got.Code)
	}
	if got.Reason != "InternalError" {
		t.Fatalf("expected InternalError, got %s", got.Reason)
	}
	// Must not leak internal error details.
	if got.Message != "internal server error" {
		t.Fatalf("expected generic message, got %q", got.Message)
	}
}

// --- Wrap / Unwrap ---

func TestWrap_PreservesCause(t *testing.T) {
	cause := fmt.Errorf("db: connection refused")
	wrapped := ErrInternal.Wrap(cause)

	if wrapped.Unwrap() != cause {
		t.Fatal("Unwrap should return the original cause")
	}
	// errors.Is should match cause via Unwrap chain.
	if !errors.Is(wrapped, cause) {
		t.Fatal("errors.Is should match wrapped cause")
	}
	// errors.Is should also match the apierr sentinel via Is method.
	if !errors.Is(wrapped, ErrInternal) {
		t.Fatal("errors.Is should match ErrInternal sentinel")
	}
}

func TestWrap_CopySemantics(t *testing.T) {
	cause := fmt.Errorf("oops")
	original := New(400, "Bad", "original")
	wrapped := original.Wrap(cause)

	// Original should not have a cause.
	if original.Unwrap() != nil {
		t.Fatal("original should not be modified")
	}
	// Wrapped should have the cause.
	if wrapped.Unwrap() != cause {
		t.Fatal("wrapped should have cause")
	}
	// Different message should not affect cause.
	withMsg := wrapped.WithMessage("changed")
	if withMsg.Unwrap() != cause {
		t.Fatal("WithMessage should preserve cause (shallow copy)")
	}
}

func TestWrap_NilCause(t *testing.T) {
	wrapped := ErrNotFound.Wrap(nil)
	if wrapped.Unwrap() != nil {
		t.Fatal("Unwrap of nil cause should be nil")
	}
}

// --- RegisterMapper / Resolve ---

func TestRegisterMapper_And_Resolve(t *testing.T) {
	ResetMappersForTest()

	sentinel := fmt.Errorf("custom: not found")
	RegisterMapper(func(err error) *Error {
		if errors.Is(err, sentinel) {
			return ErrNotFound
		}
		return nil
	})

	// Should resolve.
	got := Resolve(sentinel)
	if got == nil || got.Code != 404 {
		t.Fatalf("expected ErrNotFound, got %v", got)
	}

	// Unrecognized error should return nil.
	got = Resolve(fmt.Errorf("other"))
	if got != nil {
		t.Fatalf("expected nil for unrecognized error, got %v", got)
	}

	ResetMappersForTest()
}

func TestRegisterMapper_MultipleMappers(t *testing.T) {
	ResetMappersForTest()

	errA := fmt.Errorf("errA")
	errB := fmt.Errorf("errB")

	RegisterMapper(func(err error) *Error {
		if errors.Is(err, errA) {
			return ErrNotFound
		}
		return nil
	})
	RegisterMapper(func(err error) *Error {
		if errors.Is(err, errB) {
			return ErrConflict
		}
		return nil
	})

	if got := Resolve(errA); got == nil || got.Code != 404 {
		t.Fatalf("errA should map to 404, got %v", got)
	}
	if got := Resolve(errB); got == nil || got.Code != 409 {
		t.Fatalf("errB should map to 409, got %v", got)
	}

	ResetMappersForTest()
}

func TestRegisterMapper_NilPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil mapper")
		}
	}()
	RegisterMapper(nil)
}

func TestResolve_NoMappers(t *testing.T) {
	ResetMappersForTest()
	got := Resolve(fmt.Errorf("anything"))
	if got != nil {
		t.Fatalf("expected nil with no mappers, got %v", got)
	}
}

func TestRegisterMapper_MultipleApps(t *testing.T) {
	// Simulates two App.Run calls in the same process (e.g. integration tests).
	// Both should be able to register mappers without panic.
	ResetMappersForTest()

	sentinel := fmt.Errorf("app1 error")
	RegisterMapper(func(err error) *Error {
		if errors.Is(err, sentinel) {
			return ErrNotFound
		}
		return nil
	})

	// "First app finishes" — no freeze, mappers stay.
	// "Second app starts" — registers another mapper.
	RegisterMapper(func(err error) *Error {
		return nil // no-op mapper from second app
	})

	// First mapper should still work.
	got := Resolve(sentinel)
	if got == nil || got.Code != 404 {
		t.Fatalf("expected 404, got %v", got)
	}

	ResetMappersForTest()
}
