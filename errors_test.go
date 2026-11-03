package chok

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestWriteError_MiddlewareRendersTheEnvelope(t *testing.T) {
	app := newTestApp(t)
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			WriteError(w, r, ErrUnauthorized.Msgf("token missing"))
		})
	}
	reached := false
	GET(app.Group("/api", deny), "/x", func(context.Context, *None) (None, error) { reached = true; return None{}, nil })
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/api/x"})
	e := decode[envelope](t, rec)
	if rec.Code != http.StatusUnauthorized || e.Code != 401 || e.Reason != "Unauthorized" || e.Message != "token missing" {
		t.Fatalf("%d %+v", rec.Code, e)
	}
	if e.RequestID == "" || e.RequestID != rec.Header().Get(requestIDHeader) {
		t.Fatalf("the envelope carries the request id %q, got %q", rec.Header().Get(requestIDHeader), e.RequestID)
	}
	if reached || rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("reached=%v content-type=%q", reached, rec.Header().Get("Content-Type"))
	}
}

var errTeapot = errors.New("teapot")

func TestWriteError_UsesTheAppErrorHook(t *testing.T) {
	app := New(testBase(t), Logger(quiet()), OnError(func(_ context.Context, err error) *Error {
		if errors.Is(err, errTeapot) {
			return NewError(http.StatusTeapot, "Teapot", "short and stout")
		}
		return nil
	}))
	mw := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			WriteError(w, r, fmt.Errorf("brewing: %w", errTeapot))
		})
	}
	app.Router().Handle(http.MethodGet, "/tea", http.NotFoundHandler(), mw)
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/tea"})
	e := decode[envelope](t, rec)
	if rec.Code != http.StatusTeapot || e.Reason != "Teapot" || e.Message != "short and stout" || e.RequestID == "" {
		t.Fatalf("%d %+v", rec.Code, e)
	}
	// An error the hook declines falls through to the built-in rules.
	app2 := New(testBase(t), Logger(quiet()), OnError(func(context.Context, error) *Error { return nil }))
	app2.Router().Handle(http.MethodGet, "/x", http.NotFoundHandler(), func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { WriteError(w, r, ErrForbidden) })
	})
	if rec := do(t, app2.Handler(), call{method: http.MethodGet, path: "/x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("declined by the hook: %d", rec.Code)
	}
}

func TestWriteError_OutsideAnApp(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), ErrRateLimited.With("retry_after", 30))
	e := decode[envelope](t, rec)
	if rec.Code != http.StatusTooManyRequests || e.Reason != "RateLimited" || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("%d %+v %v", rec.Code, e, rec.Header())
	}
	if e.RequestID != "" {
		t.Fatalf("no request id outside the handler chain, got %q", e.RequestID)
	}
	// A plain error is a 500 that hides its cause, hook or no hook.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	WriteError(rec, req.WithContext(context.WithValue(req.Context(), loggerKey{}, quiet())), errors.New("disk full"))
	e = decode[envelope](t, rec)
	if rec.Code != http.StatusInternalServerError || e.Reason != "InternalError" || strings.Contains(rec.Body.String(), "disk full") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// A Coder error describes itself here too.
	rec = httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), coded{})
	if e := decode[envelope](t, rec); rec.Code != http.StatusConflict || e.Message != "already there" {
		t.Fatalf("%d %+v", rec.Code, e)
	}
}

type coded struct{}

func (coded) Error() string    { return "coded" }
func (coded) APIError() *Error { return ErrConflict.Msgf("already there") }

// TestWriteError_5xxLogsTheClassificationOnly: the line a 5xx logs names
// the status, the reason and each error of the chain — an *Error by its
// reason and message, anything else by its Go type — and never carries
// the text of an error chok did not write: a driver's or the standard
// library's message often quotes the input. The request_id ties the line
// to whatever the error's origin logged.
func TestWriteError_5xxLogsTheClassificationOnly(t *testing.T) {
	var logs bytes.Buffer
	app := New(testBase(t), Logger(slog.New(slog.NewTextHandler(&logs, nil))))
	leaky := errors.New(`strconv.Atoi: parsing "s3cret-value": invalid syntax`)
	cases := []struct {
		path   string
		err    error
		status int
		reason string
		chain  string
	}{
		{"/plain", leaky, 500, "InternalError", `error=*errors.errorString`},
		{"/wrapped", fmt.Errorf("load: %w", leaky), 500, "InternalError", `error="*fmt.wrapError <- *errors.errorString"`},
		{"/joined", errors.Join(leaky, ErrInternal.Msgf("two failures")), 500, "InternalError",
			`error="*errors.joinError <- [*errors.errorString | InternalError: two failures]"`},
		{"/timeout", fmt.Errorf("query: %w", context.DeadlineExceeded), 504, "Timeout",
			`error="*fmt.wrapError <- context.DeadlineExceeded"`},
		// The message is chok's or the application's own words; the cause
		// below it is not.
		{"/chok", ErrInternal.Msgf("settings unavailable").Wrap(fmt.Errorf("load: %w", leaky)), 500, "InternalError",
			`error="InternalError: settings unavailable <- *fmt.wrapError <- *errors.errorString"`},
	}
	for _, c := range cases {
		GET(app.Router(), c.path, func(context.Context, *None) (None, error) { return None{}, c.err })
	}
	for _, c := range cases {
		logs.Reset()
		rec := do(t, app.Handler(), call{method: http.MethodGet, path: c.path})
		if rec.Code != c.status || strings.Contains(rec.Body.String(), "s3cret-value") {
			t.Fatalf("%s: %d %s", c.path, rec.Code, rec.Body)
		}
		line := logs.String()
		for _, want := range []string{`level=ERROR`, `msg="chok: request failed"`, "request_id=" + rec.Header().Get(requestIDHeader),
			fmt.Sprintf("status=%d", c.status), "reason=" + c.reason, c.chain} {
			if !strings.Contains(line, want) {
				t.Errorf("%s: the log lacks %s:\n%s", c.path, want, line)
			}
		}
		if strings.Contains(line, "s3cret-value") || strings.Contains(line, "load:") || strings.Contains(line, "query:") {
			t.Errorf("%s: the log carries the text of an error chok did not write:\n%s", c.path, line)
		}
	}
}

// timeoutLike is an error whose own Is method matches
// context.DeadlineExceeded, as the timeout of net/http's client does.
type timeoutLike struct{}

func (timeoutLike) Error() string        { return "timed out reading s3cret-value" }
func (timeoutLike) Is(target error) bool { return target == context.DeadlineExceeded }

// TestDescribe_NamesTheCommonSentinels: context.Canceled,
// context.DeadlineExceeded, io.EOF and io.ErrUnexpectedEOF are named
// where they sit in the chain, as errors.Is matches one link, instead of
// shown as a type that says nothing; a wrapper around one keeps its own
// type, and an error that only reads like one is not it.
func TestDescribe_NamesTheCommonSentinels(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{context.Canceled, "context.Canceled"},
		{context.DeadlineExceeded, "context.DeadlineExceeded"},
		{io.EOF, "io.EOF"},
		{fmt.Errorf("read body: %w", io.ErrUnexpectedEOF), "*fmt.wrapError <- io.ErrUnexpectedEOF"},
		{ErrInternal.Wrap(io.EOF), "InternalError: internal server error <- io.EOF"},
		{errors.Join(context.Canceled, errors.New("other")), "*errors.joinError <- [context.Canceled | *errors.errorString]"},
		{fmt.Errorf("call: %w", timeoutLike{}), "*fmt.wrapError <- context.DeadlineExceeded"},
		{errors.New("context canceled"), "*errors.errorString"},
	} {
		if got := describe(c.err); got != c.want {
			t.Errorf("describe = %q, want %q", got, c.want)
		}
	}
}

// TestDescribe_RuntimeErrorsKeepTheirText: a runtime.Error is the Go
// runtime's own words, an index, a length or type names, so describe
// writes its text where it sits in the chain; a wrapper keeps its type.
func TestDescribe_RuntimeErrorsKeepTheirText(t *testing.T) {
	var rt error
	func() {
		defer func() { rt, _ = recover().(error) }()
		func(s []int, i int) { _ = s[i] }(nil, 5)
	}()
	if got, want := describe(fmt.Errorf("step: %w", rt)), "*fmt.wrapError <- runtime error: index out of range [5] with length 0"; got != want {
		t.Fatalf("describe = %q, want %q", got, want)
	}
}
