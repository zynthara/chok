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
	// Headers holds response headers the caller must emit alongside the
	// body — e.g. Retry-After for 429/503. Responders set these via
	// WithHeader. Not serialized into JSON.
	Headers map[string]string `json:"-"`
	cause   error             // not serialized; preserved for error chain / logging
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Reason
	}
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
	if e.Headers != nil {
		cp.Headers = make(map[string]string, len(e.Headers))
		for hk, hv := range e.Headers {
			cp.Headers[hk] = hv
		}
	}
	return &cp
}

// WithMetadata returns a copy with an additional metadata key.
func (e *Error) WithMetadata(k string, v any) *Error {
	cp := *e
	cp.Metadata = make(map[string]any, len(e.Metadata)+1)
	maps.Copy(cp.Metadata, e.Metadata)
	cp.Metadata[k] = v
	// Headers is a separate map: deep-copy so downstream mutation on the
	// returned copy doesn't race with the shared sentinel's Headers (e.g.
	// ErrTooManyRequests.WithHeader(...).WithMetadata(...)).
	if e.Headers != nil {
		cp.Headers = make(map[string]string, len(e.Headers))
		for hk, hv := range e.Headers {
			cp.Headers[hk] = hv
		}
	}
	return &cp
}

// WithHeader returns a copy with an additional response header.
// Typical use: apierr.ErrTooManyRequests.WithHeader("Retry-After", "30").
// The responder writes these headers before emitting the JSON body.
func (e *Error) WithHeader(k, v string) *Error {
	cp := *e
	cp.Headers = make(map[string]string, len(e.Headers)+1)
	for hk, hv := range e.Headers {
		cp.Headers[hk] = hv
	}
	cp.Headers[k] = v
	return &cp
}

// FromError converts any error to *Error.
// Unwraps via errors.As; non-*Error defaults to 500 InternalError with
// the original error attached as the Wrap cause so errors.Unwrap and
// structured logging still reach the root. The user-facing Message
// stays the generic "internal server error" so internal detail is not
// leaked into the response body.
func FromError(err error) *Error {
	if err == nil {
		return nil
	}
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	return ErrInternal.Wrap(err)
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
	if e.Headers != nil {
		cp.Headers = make(map[string]string, len(e.Headers))
		for hk, hv := range e.Headers {
			cp.Headers[hk] = hv
		}
	}
	cp.cause = cause
	return &cp
}

// Unwrap returns the wrapped cause, if any.
func (e *Error) Unwrap() error { return e.cause }

// Predefined errors.
var (
	ErrInternal         = New(500, "InternalError", "internal server error")
	ErrNotFound         = New(404, "NotFound", "resource not found")
	ErrBind             = New(400, "BindError", "request bind error")
	ErrInvalidArgument  = New(400, "InvalidArgument", "invalid argument")
	ErrUnauthenticated  = New(401, "Unauthenticated", "unauthenticated")
	ErrPermissionDenied = New(403, "PermissionDenied", "permission denied")
	ErrConflict         = New(409, "Conflict", "resource version conflict")
	ErrTooManyRequests  = New(429, "TooManyRequests", "too many requests, please try again later")
	ErrGatewayTimeout   = New(504, "GatewayTimeout", "request timed out")
)
