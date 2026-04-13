package apierr

import (
	"errors"
	"maps"
)

type Error struct {
	Code     int            `json:"code"`
	Reason   string         `json:"reason"`
	Message  string         `json:"message"`
	Metadata map[string]any `json:"metadata,omitempty"`
	cause    error          // not serialized; preserved for error chain / logging
}

func (e *Error) Error() string {
	return e.Reason + ": " + e.Message
}

// Is matches by Code+Reason, ignoring Message/Metadata.
// This allows WithMessage()/WithMetadata() copies to match the original sentinel.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return e.Code == t.Code && e.Reason == t.Reason
}

func New(code int, reason, message string) *Error {
	return &Error{Code: code, Reason: reason, Message: message}
}

// WithMessage returns a copy with a different message.
func (e *Error) WithMessage(msg string) *Error {
	cp := *e
	cp.Message = msg
	if e.Metadata != nil {
		cp.Metadata = make(map[string]any, len(e.Metadata))
		maps.Copy(cp.Metadata, e.Metadata)
	}
	return &cp
}

// WithMetadata returns a copy with an additional metadata key.
func (e *Error) WithMetadata(k string, v any) *Error {
	cp := *e
	cp.Metadata = make(map[string]any, len(e.Metadata)+1)
	maps.Copy(cp.Metadata, e.Metadata)
	cp.Metadata[k] = v
	return &cp
}

// FromError converts any error to *Error.
// Unwraps via errors.As; non-*Error defaults to 500 InternalError.
func FromError(err error) *Error {
	if err == nil {
		return nil
	}
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	// Do not leak internal error details into the response message.
	return ErrInternal
}

// Wrap returns a copy with the given cause attached for error-chain traversal.
// The cause is not serialized into JSON responses (security: internal errors
// are not leaked). Use errors.Unwrap or %w logging to inspect the chain.
//
// Implementation note: WithMessage/WithMetadata use `cp := *e` (shallow copy),
// which automatically preserves cause. If those methods are ever refactored to
// explicit field assignment, cause must be copied too.
func (e *Error) Wrap(cause error) *Error {
	cp := *e
	if e.Metadata != nil {
		cp.Metadata = make(map[string]any, len(e.Metadata))
		maps.Copy(cp.Metadata, e.Metadata)
	}
	cp.cause = cause
	return &cp
}

// Unwrap returns the wrapped cause, if any.
func (e *Error) Unwrap() error { return e.cause }

// Predefined errors.
var (
	ErrInternal        = New(500, "InternalError", "internal server error")
	ErrNotFound        = New(404, "NotFound", "resource not found")
	ErrBind            = New(400, "BindError", "request bind error")
	ErrInvalidArgument = New(400, "InvalidArgument", "invalid argument")
	ErrUnauthenticated = New(401, "Unauthenticated", "unauthenticated")
	ErrPermissionDenied = New(403, "PermissionDenied", "permission denied")
	ErrConflict        = New(409, "Conflict", "resource version conflict")
)
