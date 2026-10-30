package chok

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/internal/jsonx"
)

// envelope is the error body.
type envelope struct {
	Code      int            `json:"code"`
	Reason    string         `json:"reason"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Metadata  map[string]any `json:"metadata"`
}

type call struct {
	method, path, body, contentType string
	header                          map[string]string
}

func do(t *testing.T, h http.Handler, c call) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if c.body != "" {
		body = strings.NewReader(c.body)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(c.method, c.path, body)
	if c.body != "" {
		ct := c.contentType
		if ct == "" {
			ct = "application/json"
		}
		if ct != "-" {
			req.Header.Set("Content-Type", ct)
		}
	}
	for k, v := range c.header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v, jsonx.Options); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func newTestApp(t *testing.T) *App {
	t.Helper()
	return New(testBase(t), Logger(quiet()))
}

type itemRequest struct {
	ID     string   `path:"id"`
	Expand []string `query:"expand"`
	Limit  *int     `query:"limit" validate:"omitempty,max=50"`
	Token  string   `header:"X-Token"`
	Title  string   `json:"title" validate:"required,max=10"`
	Tags   []string `json:"tags"`
}

type itemResponse struct {
	ID     string   `json:"id"`
	Expand []string `json:"expand"`
	Limit  int      `json:"limit"`
	Token  string   `json:"token"`
	Title  string   `json:"title"`
	Tags   []string `json:"tags"`
}

func echoItem(_ context.Context, req *itemRequest) (itemResponse, error) {
	r := itemResponse{ID: req.ID, Expand: req.Expand, Token: req.Token, Title: req.Title, Tags: req.Tags}
	if req.Limit != nil {
		r.Limit = *req.Limit
	}
	return r, nil
}

func TestBind_MultiSource(t *testing.T) {
	app := newTestApp(t)
	PUT(app.Router(), "/items/{id}", echoItem)
	rec := do(t, app.Handler(), call{method: http.MethodPut, path: "/items/i1?expand=a,b&expand=c&limit=5",
		body: `{"title":"hello","tags":["x"]}`, header: map[string]string{"X-Token": "tok"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got := decode[itemResponse](t, rec)
	if got.ID != "i1" || len(got.Expand) != 3 || got.Limit != 5 || got.Token != "tok" || got.Title != "hello" || got.Tags[0] != "x" {
		t.Fatalf("got %+v", got)
	}
}

func TestBind_PathOnlyIgnoresEmptyBody(t *testing.T) {
	app := newTestApp(t)
	type req struct {
		ID string `path:"id" validate:"required"`
	}
	GET(app.Router(), "/x/{id}", func(_ context.Context, r *req) (string, error) { return r.ID, nil })
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/x/abc"})
	if rec.Code != http.StatusOK || decode[string](t, rec) != "abc" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestBind_EmptyBodyRequiredJSONFieldFailsValidation(t *testing.T) {
	app := newTestApp(t)
	POST(app.Router(), "/items/{id}", echoItem)
	rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/items/1"})
	e := decode[envelope](t, rec)
	if rec.Code != http.StatusBadRequest || e.Reason != "Validation" || e.Metadata["fields"].(map[string]any)["title"] != "required" {
		t.Fatalf("%d %+v", rec.Code, e)
	}
}

func TestBind_ValidationErrorsUseSourceNames(t *testing.T) {
	app := newTestApp(t)
	POST(app.Router(), "/items/{id}", echoItem)
	rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/items/1?limit=99", body: `{"title":"far too long"}`})
	e := decode[envelope](t, rec)
	fields, _ := e.Metadata["fields"].(map[string]any)
	if rec.Code != http.StatusBadRequest || fields["title"] != "max" || fields["limit"] != "max" || len(fields) != 2 {
		t.Fatalf("%d %+v", rec.Code, e)
	}
	if e.RequestID == "" {
		t.Fatal("the envelope carries the request id")
	}
}

func TestBind_PathBeatsSpoofedBody(t *testing.T) {
	app := newTestApp(t)
	type req struct {
		ID    string `path:"id"`
		Owner string `query:"owner"`
		Title string `json:"title"`
	}
	PUT(app.Router(), "/x/{id}", func(_ context.Context, r *req) (string, error) { return r.ID + "|" + r.Owner, nil })
	rec := do(t, app.Handler(), call{method: http.MethodPut, path: "/x/real", body: `{"ID":"spoof","Owner":"root","title":"t"}`})
	if rec.Code != http.StatusOK || decode[string](t, rec) != "real|" {
		t.Fatalf("a body must not set path or query fields: %d %s", rec.Code, rec.Body)
	}
}

func TestBind_ConflictingTagsPanic(t *testing.T) {
	type bad struct {
		ID string `path:"id" json:"id"`
	}
	defer func() {
		if p := recover(); p == nil || !strings.Contains(p.(string), "has path and json tags") {
			t.Fatalf("got %v", p)
		}
	}()
	GET(newTestApp(t).Router(), "/x/{id}", func(context.Context, *bad) (None, error) { return None{}, nil })
}

func TestBind_UnknownFieldRejected(t *testing.T) {
	app := newTestApp(t)
	POST(app.Router(), "/items/{id}", echoItem)
	rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/items/1", body: `{"title":"ok","titel":"typo"}`})
	e := decode[envelope](t, rec)
	if rec.Code != http.StatusBadRequest || e.Reason != "BadRequest" || e.Metadata["field"] != "titel" {
		t.Fatalf("%d %+v", rec.Code, e)
	}
}

func TestBind_JSONIsCaseSensitive(t *testing.T) {
	app := newTestApp(t)
	POST(app.Router(), "/items/{id}", echoItem)
	rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/items/1", body: `{"Title":"x"}`})
	if e := decode[envelope](t, rec); rec.Code != http.StatusBadRequest || e.Metadata["field"] != "Title" {
		t.Fatalf("a differently cased name is unknown: %d %+v", rec.Code, e)
	}
}

func TestBind_DuplicateKeyRejected(t *testing.T) {
	app := newTestApp(t)
	POST(app.Router(), "/items/{id}", echoItem)
	rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/items/1", body: `{"title":"a","title":"b"}`})
	if e := decode[envelope](t, rec); rec.Code != http.StatusBadRequest || !strings.Contains(e.Message, "duplicate") {
		t.Fatalf("%d %+v", rec.Code, e)
	}
}

func TestBind_MalformedJSON(t *testing.T) {
	app := newTestApp(t)
	POST(app.Router(), "/items/{id}", echoItem)
	rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/items/1", body: `{"title":`})
	if e := decode[envelope](t, rec); rec.Code != http.StatusBadRequest || !strings.HasPrefix(e.Message, "invalid JSON body") {
		t.Fatalf("%d %+v", rec.Code, e)
	}
}

func TestBind_TypeConversionError(t *testing.T) {
	app := newTestApp(t)
	POST(app.Router(), "/items/{id}", echoItem)
	rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/items/1", body: `{"title":5}`})
	e := decode[envelope](t, rec)
	if rec.Code != http.StatusBadRequest || e.Metadata["field"] != "title" || !strings.Contains(e.Message, "cannot use a JSON number as string") {
		t.Fatalf("%d %+v", rec.Code, e)
	}
	rec = do(t, app.Handler(), call{method: http.MethodPost, path: "/items/1?limit=many", body: `{"title":"x"}`})
	if e := decode[envelope](t, rec); rec.Code != http.StatusBadRequest || e.Metadata["field"] != "limit" {
		t.Fatalf("query: %d %+v", rec.Code, e)
	}
}

func TestBind_ContentTypeMustBeJSON(t *testing.T) {
	app := newTestApp(t)
	POST(app.Router(), "/items/{id}", echoItem)
	for _, ct := range []string{"-", "text/plain", "application/x-www-form-urlencoded"} {
		rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/items/1", body: `{"title":"x"}`, contentType: ct})
		if e := decode[envelope](t, rec); rec.Code != http.StatusUnsupportedMediaType || e.Reason != "UnsupportedMediaType" {
			t.Fatalf("%s: %d %+v", ct, rec.Code, e)
		}
	}
	rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/items/1", body: `{"title":"x"}`, contentType: "application/merge-patch+json; charset=utf-8"})
	if rec.Code != http.StatusOK {
		t.Fatalf("+json types are JSON: %d %s", rec.Code, rec.Body)
	}
}

func TestBind_BodyLimit(t *testing.T) {
	app := newTestApp(t)
	POST(app.Router(), "/small/{id}", echoItem, MaxBody(16))
	rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/small/1", body: `{"title":"0123456789","tags":["a","b"]}`})
	if e := decode[envelope](t, rec); rec.Code != http.StatusRequestEntityTooLarge || e.Metadata["limit"] != float64(16) {
		t.Fatalf("%d %+v", rec.Code, e)
	}
}

type defaulted struct {
	Size  int    `query:"size"`
	Order string `query:"order"`
}

func (d *defaulted) Default() {
	if d.Size == 0 {
		d.Size = 20
	}
	if d.Order == "" {
		d.Order = "asc"
	}
}

func TestBind_DefaultRunsBeforeValidation(t *testing.T) {
	app := newTestApp(t)
	GET(app.Router(), "/d", func(_ context.Context, d *defaulted) (*defaulted, error) { return d, nil })
	got := decode[defaulted](t, do(t, app.Handler(), call{method: http.MethodGet, path: "/d"}))
	if got.Size != 20 || got.Order != "asc" {
		t.Fatalf("defaults: %+v", got)
	}
	got = decode[defaulted](t, do(t, app.Handler(), call{method: http.MethodGet, path: "/d?size=5&order=desc"}))
	if got.Size != 5 || got.Order != "desc" {
		t.Fatalf("explicit values are kept: %+v", got)
	}
}

func TestBind_NonStructRequestBindsJSON(t *testing.T) {
	app := newTestApp(t)
	POST(app.Router(), "/m", func(_ context.Context, m *map[string]any) (int, error) { return len(*m), nil })
	rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/m", body: `{"a":1,"b":2}`})
	if rec.Code != http.StatusOK || decode[int](t, rec) != 2 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestBind_TextKinds(t *testing.T) {
	app := newTestApp(t)
	type kinds struct {
		B   bool          `query:"b"`
		I8  int8          `query:"i8"`
		U   uint          `query:"u"`
		F   float64       `query:"f"`
		D   time.Duration `query:"d"`
		T   time.Time     `query:"t"`
		P   *string       `query:"p"`
		Ids []int         `query:"ids"`
	}
	GET(app.Router(), "/k", func(_ context.Context, k *kinds) (*kinds, error) { return k, nil })
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/k?b=true&i8=-3&u=7&f=1.5&d=2s&t=2026-01-02T03:04:05Z&p=x&ids=1,2&ids=3&b=false"})
	got := decode[kinds](t, rec)
	if !got.B || got.I8 != -3 || got.U != 7 || got.F != 1.5 || got.D != 2*time.Second || got.T.Year() != 2026 || *got.P != "x" || len(got.Ids) != 3 {
		t.Fatalf("scalars take the first value, lists every value: %+v", got)
	}
	if rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/k?i8=300"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("overflow: %d", rec.Code)
	}
}

func TestResponse_NoneIs204(t *testing.T) {
	app := newTestApp(t)
	DELETE(app.Router(), "/x/{id}", func(context.Context, *None) (None, error) { return None{}, nil })
	POST(app.Router(), "/accept", func(context.Context, *None) (None, error) { return None{}, nil }, Status(http.StatusAccepted))
	if rec := do(t, app.Handler(), call{method: http.MethodDelete, path: "/x/1"}); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
	if rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/accept"}); rec.Code != http.StatusAccepted {
		t.Fatalf("Status overrides 204: %d", rec.Code)
	}
}

func TestResponse_CreatedAndNilPointer(t *testing.T) {
	app := newTestApp(t)
	POST(app.Router(), "/c", func(context.Context, *None) (*itemResponse, error) { return &itemResponse{ID: "1"}, nil }, Created())
	GET(app.Router(), "/nil", func(context.Context, *None) (*itemResponse, error) { return nil, nil })
	if rec := do(t, app.Handler(), call{method: http.MethodPost, path: "/c"}); rec.Code != http.StatusCreated || decode[itemResponse](t, rec).ID != "1" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/nil"}); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
}

func TestResponse_NilSliceIsEmptyArray(t *testing.T) {
	app := newTestApp(t)
	GET(app.Router(), "/s", func(context.Context, *None) ([]string, error) { return nil, nil })
	if rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/s"}); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("%q", rec.Body)
	}
}

type stockError struct{}

func (stockError) Error() string { return "out of stock" }
func (stockError) APIError() *Error {
	return ErrConflict.Msgf("out of stock").With("retry_after", 30)
}

func TestErrors_ResolutionOrder(t *testing.T) {
	var logs bytes.Buffer
	app := New(testBase(t), Logger(slog.New(slog.NewJSONHandler(&logs, nil))))
	r := app.Router()
	GET(r, "/api", func(context.Context, *None) (None, error) { return None{}, ErrForbidden.Msgf("no") })
	GET(r, "/wrapped", func(context.Context, *None) (None, error) {
		return None{}, errors.Join(errors.New("context"), ErrNotFound.With("id", "7"))
	})
	GET(r, "/coder", func(context.Context, *None) (None, error) { return None{}, stockError{} })
	GET(r, "/secret", func(context.Context, *None) (None, error) { return None{}, errors.New("db password is hunter2") })
	cases := []struct {
		path   string
		code   int
		reason string
	}{
		{"/api", 403, "Forbidden"}, {"/wrapped", 404, "NotFound"}, {"/coder", 409, "Conflict"}, {"/secret", 500, "InternalError"},
	}
	for _, c := range cases {
		rec := do(t, app.Handler(), call{method: http.MethodGet, path: c.path})
		e := decode[envelope](t, rec)
		if rec.Code != c.code || e.Reason != c.reason {
			t.Fatalf("%s: %d %+v", c.path, rec.Code, e)
		}
		if c.path == "/coder" && rec.Header().Get("Retry-After") != "30" {
			t.Fatal("metadata retry_after becomes the Retry-After header")
		}
		if c.path == "/secret" {
			if strings.Contains(rec.Body.String(), "hunter2") || !strings.Contains(logs.String(), "hunter2") {
				t.Fatal("a 500's cause is logged, never sent")
			}
		}
	}
}

func TestErrors_OnErrorHookComesFirst(t *testing.T) {
	app := New(testBase(t), Logger(quiet()), OnError(func(_ context.Context, err error) *Error {
		if errors.Is(err, context.DeadlineExceeded) {
			return NewError(http.StatusServiceUnavailable, "Busy", "try later")
		}
		return nil
	}))
	GET(app.Router(), "/slow", func(context.Context, *None) (None, error) { return None{}, context.DeadlineExceeded })
	GET(app.Router(), "/other", func(context.Context, *None) (None, error) { return None{}, ErrConflict })
	if rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/slow"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("hook: %d", rec.Code)
	}
	if rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/other"}); rec.Code != http.StatusConflict {
		t.Fatalf("a nil hook result falls through: %d", rec.Code)
	}
}

type fakeLister struct {
	gotQuery url.Values
	err      error
}

func (l *fakeLister) ListFromQuery(_ context.Context, q url.Values) (*Page[itemResponse], error) {
	l.gotQuery = q
	if l.err != nil {
		return nil, l.err
	}
	return &Page[itemResponse]{Total: 3, Page: 2, Size: 1, HasMore: true}, nil
}

func (l *fakeLister) QueryFields() []string { return []string{"title"} }

func TestList_RendersPage(t *testing.T) {
	app := newTestApp(t)
	l := &fakeLister{}
	List(app.Router(), "/items", l)
	rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/items?page=2&size=1&title=x"})
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"items":[],"total":3,"page":2,"size":1,"has_more":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if l.gotQuery.Get("title") != "x" {
		t.Fatal("the query reaches the Lister")
	}
	l.err = ErrBadRequest.Msgf("unknown filter")
	if rec := do(t, app.Handler(), call{method: http.MethodGet, path: "/items?nope=1"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("Lister errors render: %d", rec.Code)
	}
}

// foreignRouter is a Router chok did not create.
type foreignRouter struct{ mux *http.ServeMux }

func (f foreignRouter) Handle(method, pattern string, h http.Handler, mw ...Middleware) {
	for _, m := range slices.Backward(mw) {
		h = m(h)
	}
	f.mux.Handle(method+" "+pattern, h)
}
func (f foreignRouter) Group(string, ...Middleware) Router { return f }

func TestVerbs_WorkOnForeignRouter(t *testing.T) {
	f := foreignRouter{mux: http.NewServeMux()}
	GET(f, "/x/{id}", func(_ context.Context, r *struct {
		ID string `path:"id"`
	}) (string, error) {
		if r.ID == "bad" {
			return "", ErrNotFound
		}
		return r.ID, nil
	})
	if rec := do(t, f.mux, call{method: http.MethodGet, path: "/x/ok"}); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	if rec := do(t, f.mux, call{method: http.MethodGet, path: "/x/bad"}); rec.Code != http.StatusNotFound {
		t.Fatalf("errors still render without an App: %d", rec.Code)
	}
}
