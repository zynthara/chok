package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/internal/ctxval"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/validate"
)

// mux builds a ServeMux with one route — the stdlib stand-in for the
// v1 gin engine in these tests. Patterns use Go 1.22 method syntax.
func mux(pattern string, h http.Handler) *http.ServeMux {
	m := http.NewServeMux()
	m.Handle(pattern, h)
	return m
}

// --- request types ---

type uriReq struct {
	RID string `uri:"rid" binding:"required"`
}

type jsonReq struct {
	Name  string `json:"name" binding:"required"`
	Email string `json:"email" binding:"required,email"`
}

type queryReq struct {
	Page int `form:"page"`
	Size int `form:"size"`
}

type multiReq struct {
	RID  string `uri:"rid" binding:"required"`
	Name string `json:"name" binding:"required"`
}

// conflicting tags — should panic
type conflictReq struct {
	Bad string `uri:"bad" json:"bad"`
}

// --- tests ---

func TestHandleRequest_URIBinding(t *testing.T) {
	r := mux("GET /users/{rid}", HandleRequest(func(_ context.Context, req *uriReq) (*uriReq, error) {
		return req, nil
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/users/usr_123", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp uriReq
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.RID != "usr_123" {
		t.Fatalf("expected usr_123, got %s", resp.RID)
	}
}

func TestHandleRequest_JSONBinding(t *testing.T) {
	r := mux("POST /users", HandleRequest(func(_ context.Context, req *jsonReq) (*jsonReq, error) {
		return req, nil
	}, WithSuccessCode(201)))

	body := `{"name":"alice","email":"alice@example.com"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/users", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleRequest_QueryBinding(t *testing.T) {
	r := mux("GET /users", HandleRequest(func(_ context.Context, req *queryReq) (*queryReq, error) {
		return req, nil
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/users?page=2&size=10", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp queryReq
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page != 2 || resp.Size != 10 {
		t.Fatalf("expected page=2 size=10, got %+v", resp)
	}
}

func TestHandleRequest_MultiSourceBinding(t *testing.T) {
	r := mux("PUT /users/{rid}", HandleRequest(func(_ context.Context, req *multiReq) (*multiReq, error) {
		return req, nil
	}))

	body := `{"name":"bob"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/users/usr_456", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp multiReq
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.RID != "usr_456" || resp.Name != "bob" {
		t.Fatalf("got %+v", resp)
	}
}

func TestHandleRequest_ConflictingTags_Panics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for conflicting tags")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "conflicting") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()

	HandleRequest(func(_ context.Context, req *conflictReq) (*conflictReq, error) {
		return req, nil
	})
}

func TestHandleRequest_DisallowUnknownFields(t *testing.T) {
	r := mux("POST /users", HandleRequest(func(_ context.Context, req *jsonReq) (*jsonReq, error) {
		return req, nil
	}))

	body := `{"name":"alice","email":"alice@example.com","unknown":"field"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/users", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400 for unknown field, got %d: %s", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Reason != "BindError" {
		t.Fatalf("expected BindError, got %s", resp.Reason)
	}
}

func TestHandleRequest_MalformedJSON(t *testing.T) {
	r := mux("POST /users", HandleRequest(func(_ context.Context, req *jsonReq) (*jsonReq, error) {
		return req, nil
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/users", strings.NewReader(`{bad json`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleRequest_TypeConversionError(t *testing.T) {
	r := mux("POST /test", HandleRequest(func(_ context.Context, req *struct {
		Count int `json:"count"`
	}) (any, error) {
		return req, nil
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/test", strings.NewReader(`{"count":"not_a_number"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleRequest_EmptyBody_URIOnly(t *testing.T) {
	r := mux("GET /users/{rid}", HandleRequest(func(_ context.Context, req *uriReq) (*uriReq, error) {
		return req, nil
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/users/usr_1", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleRequest_EmptyBody_RequiredJSONField(t *testing.T) {
	r := mux("POST /users", HandleRequest(func(_ context.Context, req *jsonReq) (*jsonReq, error) {
		return req, nil
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/users", http.NoBody)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400 for missing required, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleAction_Default204(t *testing.T) {
	r := mux("DELETE /users/{rid}", HandleAction(func(_ context.Context, req *uriReq) error {
		return nil
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/users/usr_1", nil)
	r.ServeHTTP(w, req)

	if w.Code != 204 {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("expected empty body, got %s", w.Body.String())
	}
}

func TestHandleRequest_APIErrorPassthrough(t *testing.T) {
	r := mux("GET /fail", HandleRequest(func(_ context.Context, req *struct{}) (any, error) {
		return nil, apierr.ErrNotFound.WithMessage("user not found")
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/fail", nil)
	r.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	var resp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Message != "user not found" {
		t.Fatalf("expected custom message, got %q", resp.Message)
	}
}

// withCtx wraps h, deriving the request context before it runs — the
// stdlib replacement for the gin middleware the old tests used.
func withCtx(h http.Handler, derive func(context.Context) context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(derive(r.Context())))
	})
}

func TestHandleRequest_InternalError_NoLeak(t *testing.T) {
	h := HandleRequest(func(_ context.Context, req *struct{}) (any, error) {
		return nil, errors.New("db connection failed")
	})
	r := mux("GET /fail", withCtx(h, func(ctx context.Context) context.Context {
		return ctxval.WithLogger(ctx, log.Empty())
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/fail", nil)
	r.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	var resp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	// Should not leak internal error message.
	if resp.Message != "internal server error" {
		t.Fatalf("expected generic message, got %q", resp.Message)
	}
}

func TestHandleRequest_NoLogger_NoPanic(t *testing.T) {
	// No logger in context — should not panic.
	r := mux("GET /fail", HandleRequest(func(_ context.Context, req *struct{}) (any, error) {
		return nil, errors.New("boom")
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/fail", nil)
	r.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestWriteResponse_InjectsRequestID(t *testing.T) {
	h := HandleRequest(func(_ context.Context, req *struct{}) (any, error) {
		return nil, apierr.ErrNotFound
	})
	r := mux("GET /fail", withCtx(h, func(ctx context.Context) context.Context {
		return ctxval.WithRequestID(ctx, "req-abc")
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/fail", nil)
	r.ServeHTTP(w, req)

	var resp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.RequestID != "req-abc" {
		t.Fatalf("expected request_id=req-abc, got %q", resp.RequestID)
	}
}

func TestValidated(t *testing.T) {
	checkName := validate.Func[jsonReq](func(_ context.Context, req *jsonReq) error {
		if req.Name == "forbidden" {
			return apierr.ErrInvalidArgument.WithMessage("forbidden name")
		}
		return nil
	})

	r := mux("POST /users", HandleRequest(
		Validated(func(_ context.Context, req *jsonReq) (*jsonReq, error) {
			return req, nil
		}, checkName),
		WithSuccessCode(201),
	))

	// Valid request.
	body := `{"name":"alice","email":"a@b.com"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/users", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Invalid request.
	body = `{"name":"forbidden","email":"a@b.com"}`
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/users", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestValidatedAction(t *testing.T) {
	called := false
	checkRID := validate.Func[uriReq](func(_ context.Context, req *uriReq) error {
		if req.RID == "bad" {
			return apierr.ErrInvalidArgument.WithMessage("bad rid")
		}
		return nil
	})

	r := mux("DELETE /users/{rid}", HandleAction(
		ValidatedAction(func(_ context.Context, req *uriReq) error {
			called = true
			return nil
		}, checkRID),
	))

	// Rejected.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/users/bad", nil)
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if called {
		t.Fatal("handler should not be called when validation fails")
	}

	// Accepted.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/users/good", nil)
	r.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

// --- regression: #3 Content-Type non-JSON with json-tagged fields ---

func TestHandleRequest_MissingContentType_ErrBind(t *testing.T) {
	r := mux("POST /users", HandleRequest(func(_ context.Context, req *jsonReq) (*jsonReq, error) {
		return req, nil
	}))

	body := `{"name":"alice","email":"alice@example.com"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/users", strings.NewReader(body))
	// No Content-Type header at all.
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400 for missing Content-Type, got %d: %s", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Reason != "BindError" {
		t.Fatalf("expected BindError, got %s", resp.Reason)
	}
}

func TestHandleRequest_NonJSONContentType_ErrBind(t *testing.T) {
	r := mux("POST /users", HandleRequest(func(_ context.Context, req *jsonReq) (*jsonReq, error) {
		return req, nil
	}))

	body := `{"name":"alice","email":"alice@example.com"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/users", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400 for non-JSON Content-Type, got %d: %s", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Reason != "BindError" {
		t.Fatalf("expected BindError, got %s", resp.Reason)
	}
}

func TestHandleRequest_ValidationErrors_FieldMetadata(t *testing.T) {
	r := mux("POST /users", HandleRequest(func(_ context.Context, req *jsonReq) (*jsonReq, error) {
		return req, nil
	}))

	// Missing required fields.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/users", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Reason != "BindError" {
		t.Fatalf("expected BindError, got %s", resp.Reason)
	}
	// Should have field metadata.
	if resp.Metadata == nil {
		t.Fatal("expected metadata with field errors")
	}
}

// --- P0a: Default hook tests ---

type defaultQueryReq struct {
	Page int `form:"page" binding:"omitempty,min=1"`
	Size int `form:"size" binding:"omitempty,min=1,max=100"`
}

func (r *defaultQueryReq) Default() {
	if r.Page == 0 {
		r.Page = 1
	}
	if r.Size == 0 {
		r.Size = 20
	}
}

func TestHandleRequest_DefaultHook_SetsDefaults(t *testing.T) {
	r := mux("GET /items", HandleRequest(func(_ context.Context, req *defaultQueryReq) (*defaultQueryReq, error) {
		return req, nil
	}))

	// Request with no query params — Default() should fill Page=1, Size=20.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/items", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp defaultQueryReq
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page != 1 {
		t.Fatalf("expected Page=1, got %d", resp.Page)
	}
	if resp.Size != 20 {
		t.Fatalf("expected Size=20, got %d", resp.Size)
	}
}

func TestHandleRequest_DefaultHook_ExplicitValuesPreserved(t *testing.T) {
	r := mux("GET /items", HandleRequest(func(_ context.Context, req *defaultQueryReq) (*defaultQueryReq, error) {
		return req, nil
	}))

	// Request with explicit values — Default() should not overwrite.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/items?page=3&size=50", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp defaultQueryReq
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page != 3 {
		t.Fatalf("expected Page=3, got %d", resp.Page)
	}
	if resp.Size != 50 {
		t.Fatalf("expected Size=50, got %d", resp.Size)
	}
}

func TestHandleRequest_NoDefaulter_StillWorks(t *testing.T) {
	// queryReq does not implement Defaulter — should work without error.
	r := mux("GET /items", HandleRequest(func(_ context.Context, req *queryReq) (*queryReq, error) {
		return req, nil
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/items?page=2", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- P1: RegisterMapper integration ---

func TestResolveError_UsesRegisteredMapper(t *testing.T) {
	// Clean up global mapper state after test to avoid polluting other tests.
	t.Cleanup(apierr.ResetMappersForTest)

	// Register a mapper that turns a custom sentinel into 404.
	customNotFound := errors.New("custom: not found")
	apierr.RegisterMapper(func(err error) *apierr.Error {
		if errors.Is(err, customNotFound) {
			return apierr.ErrNotFound.WithMessage("mapped by custom mapper")
		}
		return nil
	})

	r := mux("GET /item", HandleRequest(func(_ context.Context, _ *queryReq) (*queryReq, error) {
		return nil, customNotFound // return raw sentinel — mapper should kick in
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/item", nil)
	r.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404 (mapped from custom sentinel), got %d: %s", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Reason != "NotFound" {
		t.Fatalf("reason = %q, want NotFound", resp.Reason)
	}
	if resp.Message != "mapped by custom mapper" {
		t.Fatalf("message = %q, want 'mapped by custom mapper'", resp.Message)
	}
}

// TestHandleRequest_NonStructTBindsJSON covers #6: when the request
// type is not a struct (e.g. map[string]any), activeBinders previously
// returned an empty slice and the body silently arrived as a zero
// value. The fix routes non-struct T through the JSON binder so the
// payload is decoded; validation is skipped because validator.v10
// requires a struct target.
func TestHandleRequest_NonStructTBindsJSON(t *testing.T) {
	r := mux("POST /echo", HandleRequest(func(_ context.Context, req *map[string]any) (map[string]any, error) {
		return *req, nil
	}))

	body := bytes.NewReader([]byte(`{"hello":"world","n":7}`))
	req, _ := http.NewRequest("POST", "/echo", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["hello"] != "world" {
		t.Fatalf("expected hello=world, got %v", resp)
	}
}

// --- M2: metadata rides the handler; render hooks; written guard ---

func TestHandlerMeta_AttachedToConstructedHandlers(t *testing.T) {
	h := HandleRequest(func(_ context.Context, req *jsonReq) (*jsonReq, error) {
		return req, nil
	}, WithSuccessCode(201), WithSummary("Create user"), WithTags("users"), WithPublic())

	mh, ok := h.(interface{ Meta() Meta })
	if !ok {
		t.Fatal("HandleRequest result must expose Meta()")
	}
	m := mh.Meta()
	if m.Code != 201 || m.Summary != "Create user" || len(m.Tags) != 1 || !m.Public {
		t.Fatalf("unexpected meta: %+v", m)
	}
	if m.ReqType == nil || m.ReqType.Name() != "jsonReq" {
		t.Fatalf("ReqType not captured: %v", m.ReqType)
	}
	if m.RespType == nil {
		t.Fatal("RespType not captured")
	}

	if _, ok := HandleAction(func(context.Context, *uriReq) error { return nil }).(interface{ Meta() Meta }); !ok {
		t.Fatal("HandleAction result must expose Meta()")
	}
}

func TestWriteError_RenderHookLocalizesCopy(t *testing.T) {
	reg := apierr.NewMapperRegistry()
	reg.RegisterRenderHook(func(_ context.Context, ae *apierr.Error) {
		ae.Message = "localized message"
	})

	h := HandleRequest(func(_ context.Context, _ *struct{}) (any, error) {
		return nil, apierr.ErrNotFound
	})
	r := mux("GET /fail", withCtx(h, func(ctx context.Context) context.Context {
		return apierr.WithMapperRegistry(ctx, reg)
	}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/fail", nil)
	r.ServeHTTP(w, req)

	var resp ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Message != "localized message" {
		t.Fatalf("render hook did not apply, got %q", resp.Message)
	}
	// The shared sentinel must NOT have been mutated (hooks get a copy).
	if apierr.ErrNotFound.Message != "resource not found" {
		t.Fatalf("sentinel polluted by render hook: %q", apierr.ErrNotFound.Message)
	}
}

// writtenRecorder simulates the web layer's written-tracking writer.
type writtenRecorder struct {
	*httptest.ResponseRecorder
	written bool
}

func (w *writtenRecorder) Written() bool { return w.written }

func TestWriteResponse_NoopWhenAlreadyWritten(t *testing.T) {
	w := &writtenRecorder{ResponseRecorder: httptest.NewRecorder(), written: true}
	req, _ := http.NewRequest("GET", "/", nil)
	WriteResponse(w, req, 200, map[string]string{"x": "y"}, nil)
	if w.Body.Len() != 0 {
		t.Fatalf("WriteResponse must no-op after a response was written, got %q", w.Body.String())
	}
	WriteError(w, req, apierr.ErrInternal)
	if w.Body.Len() != 0 {
		t.Fatalf("WriteError must no-op after a response was written, got %q", w.Body.String())
	}
}

func TestWriteError_EmitsApierrHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/", nil)
	WriteError(w, req, apierr.ErrTooManyRequests.WithHeader("Retry-After", "30"))
	if w.Code != 429 {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After header missing, got %q", got)
	}
}
