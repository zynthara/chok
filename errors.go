package chok

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sort"

	"github.com/go-playground/validator/v10"
)

// Error is the body of every failed response:
//
//	{"code":404,"reason":"NotFound","message":"not found","request_id":"…","metadata":{…}}
//
// Return one from a handler, or return any error that implements Coder.
type Error struct {
	Code     int            `json:"code"`
	Reason   string         `json:"reason"`
	Message  string         `json:"message"`
	Metadata map[string]any `json:"metadata,omitzero"`

	cause error
}

// NewError returns an Error with the HTTP status code, a machine-readable
// reason and a message for people.
func NewError(code int, reason, message string) *Error {
	return &Error{Code: code, Reason: reason, Message: message}
}

// The errors every application meets. Derive variants with With, Msgf
// and Wrap; errors.Is matches a variant against its sentinel.
var (
	ErrBadRequest   = NewError(http.StatusBadRequest, "BadRequest", "bad request")
	ErrValidation   = NewError(http.StatusBadRequest, "Validation", "validation failed")
	ErrUnauthorized = NewError(http.StatusUnauthorized, "Unauthorized", "unauthorized")
	ErrForbidden    = NewError(http.StatusForbidden, "Forbidden", "forbidden")
	ErrNotFound     = NewError(http.StatusNotFound, "NotFound", "not found")
	ErrConflict     = NewError(http.StatusConflict, "Conflict", "conflict")
	ErrRateLimited  = NewError(http.StatusTooManyRequests, "RateLimited", "rate limited")
	ErrInternal     = NewError(http.StatusInternalServerError, "InternalError", "internal server error")
)

func (e *Error) Error() string {
	msg := e.Reason
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.cause != nil {
		msg += ": " + e.cause.Error()
	}
	return msg
}

// Unwrap returns the cause given to Wrap.
func (e *Error) Unwrap() error { return e.cause }

// Is reports whether target is an *Error with the same code and reason,
// so errors.Is(err, chok.ErrConflict) holds for any conflict.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && e.Code == t.Code && e.Reason == t.Reason
}

func (e *Error) clone() *Error {
	cp := *e
	cp.Metadata = maps.Clone(e.Metadata)
	return &cp
}

// With returns a copy that also carries key in its metadata.
func (e *Error) With(key string, v any) *Error {
	cp := e.clone()
	if cp.Metadata == nil {
		cp.Metadata = map[string]any{}
	}
	cp.Metadata[key] = v
	return cp
}

// Msgf returns a copy with another message.
func (e *Error) Msgf(format string, args ...any) *Error {
	cp := e.clone()
	cp.Message = fmt.Sprintf(format, args...)
	return cp
}

// Wrap returns a copy whose cause is err. The cause is logged, never
// sent to the client.
func (e *Error) Wrap(cause error) *Error {
	cp := e.clone()
	cp.cause = cause
	return cp
}

// Coder is implemented by errors that know their HTTP response. Errors
// from store, identity and jobs implement it, so a handler returns them
// unchanged.
type Coder interface{ APIError() *Error }

// ErrorHook turns an error into a response before the built-in rules;
// it returns nil to leave the error to them.
type ErrorHook func(ctx context.Context, err error) *Error

// OnError installs the App's ErrorHook; a second OnError replaces the first.
func OnError(h ErrorHook) Option { return func(a *App) { a.onError = h } }

// resolve maps any error to its response: the ErrorHook, then *Error,
// then Coder, then validator errors, then an expired deadline (504);
// anything else is a 500 whose cause is only logged.
func (a *App) resolve(ctx context.Context, err error) *Error {
	if a != nil && a.onError != nil {
		if e := a.onError(ctx, err); e != nil {
			return e
		}
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	if c, ok := errors.AsType[interface {
		error
		Coder
	}](err); ok {
		if e := c.APIError(); e != nil {
			return e
		}
	}
	if ve, ok := errors.AsType[validator.ValidationErrors](err); ok {
		return validationError(ve, nil)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errTimeout.Wrap(err)
	}
	return ErrInternal.Wrap(err)
}

// writeError renders err as the envelope; a 5xx logs its cause.
func (a *App) writeError(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	e := a.resolve(ctx, err)
	code := e.Code
	if code < 400 || code > 599 {
		code = http.StatusInternalServerError
	}
	if code >= 500 {
		LoggerFrom(ctx).ErrorContext(ctx, "chok: request failed", "status", code, "error", err)
	}
	if ra, ok := e.Metadata["retry_after"]; ok {
		w.Header().Set("Retry-After", fmt.Sprint(ra))
	}
	body := struct {
		Code      int            `json:"code"`
		Reason    string         `json:"reason"`
		Message   string         `json:"message"`
		RequestID string         `json:"request_id,omitzero"`
		Metadata  map[string]any `json:"metadata,omitzero"`
	}{code, e.Reason, e.Message, RequestID(ctx), e.Metadata}
	writeJSON(w, r, code, body)
}

// validationError is the 400 for failed validate rules: metadata.fields
// maps each field's source name (json, path, query or header) to the
// rule it broke.
func validationError(ve validator.ValidationErrors, cause error) *Error {
	fields := make(map[string]string, len(ve))
	names := make([]string, 0, len(ve))
	for _, fe := range ve {
		name := fe.Field()
		if _, dup := fields[name]; !dup {
			names = append(names, name)
		}
		fields[name] = fe.Tag()
	}
	sort.Strings(names)
	e := ErrValidation.With("fields", fields)
	if len(names) == 1 {
		e.Message = "validation failed: " + names[0] + " " + ruleText(fields[names[0]])
	}
	if cause != nil {
		e.cause = cause
	}
	return e
}

func ruleText(tag string) string {
	switch tag {
	case "required":
		return "is required"
	case "email":
		return "must be an email address"
	case "oneof":
		return "is not one of the allowed values"
	case "min", "gte", "gt":
		return "is too small or too short"
	case "max", "lte", "lt":
		return "is too large or too long"
	}
	return "fails " + tag
}
