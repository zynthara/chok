package chok

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strings"

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

// Wrap returns a copy whose cause is err. The cause is never sent to
// the client, and a 5xx logs what kind of error it is, never its text.
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
// anything else is a 500 that keeps its cause out of the response.
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

// WriteError renders err as the error envelope, the way a handler's
// returned error is rendered: the App's ErrorHook first, then *Error,
// Coder, validation errors and expired deadlines, anything else a 500;
// request_id in the body and, when the metadata carries retry_after, a
// Retry-After header. It is for middleware, which has no error to
// return: a handler returns its error instead. Inside an App's handler
// chain it uses that App's ErrorHook; elsewhere it renders without one.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	a, _ := r.Context().Value(appKey).(*App)
	a.writeError(w, r, err)
}

// writeError renders err as the envelope; a 5xx logs how it is
// classified: the status, the reason and describe(err), never the
// error's text. A nil App renders without an ErrorHook.
func (a *App) writeError(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	e := a.resolve(ctx, err)
	code := e.Code
	if code < 400 || code > 599 {
		code = http.StatusInternalServerError
	}
	if code >= 500 {
		LoggerFrom(ctx).ErrorContext(ctx, "chok: request failed", "status", code, "reason", e.Reason, "error", describe(err))
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

// describe names each error of err's chain, outermost first, joined by
// " <- ": an *Error by its reason and message, which chok or the
// application wrote, one of the sentinels below by its name, anything
// else by its Go type alone, because the text of a driver's or the
// standard library's error often quotes the input — the duplicate value,
// the string that did not parse. The causes of an error that wraps
// several are listed in brackets, separated by " | ". The request_id of
// the line ties it to whatever the code that met the error logged.
func describe(err error) string {
	var b strings.Builder
	describeChain(&b, err)
	return b.String()
}

func describeChain(b *strings.Builder, err error) {
	for {
		if e, ok := err.(*Error); ok {
			b.WriteString(e.Reason)
			if e.Message != "" {
				b.WriteString(": " + e.Message)
			}
		} else if name := sentinelName(err); name != "" {
			b.WriteString(name)
		} else {
			fmt.Fprintf(b, "%T", err)
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			if err = u.Unwrap(); err == nil {
				return
			}
			b.WriteString(" <- ")
		case interface{ Unwrap() []error }:
			causes := u.Unwrap()
			if len(causes) == 0 {
				return
			}
			b.WriteString(" <- [")
			for i, c := range causes {
				if i > 0 {
					b.WriteString(" | ")
				}
				describeChain(b, c)
			}
			b.WriteString("]")
			return
		default:
			return
		}
	}
}

// sentinels are the standard errors describe names, since the type of
// most of them, *errors.errorString, says nothing.
var sentinels = [...]struct {
	err  error
	name string
}{
	{context.Canceled, "context.Canceled"},
	{context.DeadlineExceeded, "context.DeadlineExceeded"},
	{io.EOF, "io.EOF"},
	{io.ErrUnexpectedEOF, "io.ErrUnexpectedEOF"},
}

// sentinelName is the name of the sentinel err is: err equals it, or
// err's own Is method matches it. That is the test errors.Is makes of
// each link of a chain, made here without unwrapping, so that an error
// wrapping a sentinel keeps its own type.
func sentinelName(err error) string {
	for _, s := range sentinels {
		if err == s.err {
			return s.name
		}
		if x, ok := err.(interface{ Is(error) bool }); ok && x.Is(s.err) {
			return s.name
		}
	}
	return ""
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
