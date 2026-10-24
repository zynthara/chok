package chok

import (
	"context"
	"errors"
	"testing"
)

func TestError_ErrorString(t *testing.T) {
	if got := ErrNotFound.Error(); got != "NotFound: not found" {
		t.Fatalf("got %q", got)
	}
	if got := NewError(418, "Teapot", "").Error(); got != "Teapot" {
		t.Fatalf("got %q", got)
	}
	if got := ErrInternal.Wrap(errors.New("disk full")).Error(); got != "InternalError: internal server error: disk full" {
		t.Fatalf("got %q", got)
	}
}

func TestError_IsMatchesCodeAndReason(t *testing.T) {
	variant := ErrConflict.Msgf("email taken").With("field", "email")
	if !errors.Is(variant, ErrConflict) || !errors.Is(errors.Join(errors.New("ctx"), variant), ErrConflict) {
		t.Fatal("a variant matches its sentinel")
	}
	if errors.Is(variant, ErrBadRequest) || errors.Is(NewError(409, "Other", "x"), ErrConflict) {
		t.Fatal("a different code or reason does not match")
	}
}

func TestError_DerivedCopiesDoNotMutate(t *testing.T) {
	a := ErrValidation.With("fields", map[string]string{"x": "required"})
	b := a.With("extra", 1).Msgf("changed")
	if len(ErrValidation.Metadata) != 0 || ErrValidation.Message != "validation failed" {
		t.Fatal("the sentinel must stay untouched")
	}
	if len(a.Metadata) != 1 || len(b.Metadata) != 2 || a.Message == b.Message {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
}

func TestError_WrapPreservesCause(t *testing.T) {
	cause := context.Canceled
	e := ErrInternal.Wrap(cause)
	if !errors.Is(e, context.Canceled) || errors.Unwrap(e) != cause {
		t.Fatal("Wrap keeps the cause in the chain")
	}
	if ErrInternal.Unwrap() != nil || ErrInternal.Wrap(nil).Unwrap() != nil {
		t.Fatal("no cause, no unwrap")
	}
}

func TestResolve_GenericErrorIs500(t *testing.T) {
	e := (*App)(nil).resolve(context.Background(), errors.New("boom"))
	if e.Code != 500 || e.Reason != "InternalError" || errors.Unwrap(e) == nil {
		t.Fatalf("got %+v", e)
	}
	if e := (*App)(nil).resolve(context.Background(), context.DeadlineExceeded); e.Code != 504 {
		t.Fatalf("an expired deadline is a 504, got %d", e.Code)
	}
}
