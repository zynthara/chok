package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/internal/clientip"
	"github.com/zynthara/chok/v2/internal/ctxval"
	"github.com/zynthara/chok/v2/log"
)

// chain composes middleware outermost-first around h — the test-side
// equivalent of the web server's stack builder.
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// trackingRecorder simulates the web layer's written-tracking writer:
// middleware read Status()/Written() through structural assertions.
type trackingRecorder struct {
	*httptest.ResponseRecorder
	wrote  bool
	status int
}

func newTrackingRecorder() *trackingRecorder {
	return &trackingRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (w *trackingRecorder) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		w.status = code
	}
	w.ResponseRecorder.WriteHeader(code)
}

func (w *trackingRecorder) Write(b []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
		w.status = http.StatusOK
	}
	return w.ResponseRecorder.Write(b)
}

func (w *trackingRecorder) Written() bool { return w.wrote }
func (w *trackingRecorder) Status() int {
	if !w.wrote {
		return http.StatusOK
	}
	return w.status
}

func TestRecovery_CatchesPanic(t *testing.T) {
	h := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}), Recovery(log.Empty()))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/panic", nil)
	h.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestRecovery_UnifiedErrorFormat(t *testing.T) {
	h := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	}), Recovery(log.Empty()), RequestID())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/panic", nil)
	h.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	var resp struct {
		Code      int    `json:"code"`
		Reason    string `json:"reason"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("panic response is not the uniform envelope: %v (%s)", err, w.Body.String())
	}
	if resp.Code != 500 || resp.Reason != "InternalError" {
		t.Fatalf("unexpected envelope: %+v", resp)
	}
	// v1 behaviour preserved: panic envelopes carry the request id even
	// though Recovery sits outside RequestID (header round-trip).
	if resp.RequestID == "" {
		t.Fatal("panic envelope must carry request_id")
	}
	if resp.RequestID != w.Header().Get("X-Request-ID") {
		t.Fatalf("request_id mismatch: body=%q header=%q", resp.RequestID, w.Header().Get("X-Request-ID"))
	}
}

// TestRecovery_DoesNotDoubleWrite pins one third of the §4.2
// written-tracking contract: a panic after a partially-written
// response must not append a second envelope.
func TestRecovery_DoesNotDoubleWrite(t *testing.T) {
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"partial":true}`)) //nolint:errcheck
		panic("after write")
	}), Recovery(log.Empty()))

	w := newTrackingRecorder()
	req, _ := http.NewRequest("GET", "/panic", nil)
	h.ServeHTTP(w, req)

	if w.Status() != 200 {
		t.Fatalf("original status must survive, got %d", w.Status())
	}
	if strings.Contains(w.Body.String(), "InternalError") {
		t.Fatalf("recovery must not append an envelope to a written response: %s", w.Body.String())
	}
}

func TestRequestID_GeneratesNew(t *testing.T) {
	var seen string
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = ctxval.RequestIDFrom(r.Context())
		w.WriteHeader(200)
	}), RequestID())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/", nil)
	h.ServeHTTP(w, req)

	if seen == "" {
		t.Fatal("request id missing from context")
	}
	if got := w.Header().Get("X-Request-ID"); got != seen {
		t.Fatalf("response header %q != context id %q", got, seen)
	}
	if len(seen) != 32 {
		t.Fatalf("generated id should be 32 hex chars, got %q", seen)
	}
}

func TestRequestID_PropagatesExisting(t *testing.T) {
	var seen string
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = ctxval.RequestIDFrom(r.Context())
		w.WriteHeader(200)
	}), RequestID())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "client-supplied-id")
	h.ServeHTTP(w, req)

	if seen != "client-supplied-id" {
		t.Fatalf("client id not propagated, got %q", seen)
	}
}

func TestLogger_InjectsToContext(t *testing.T) {
	var got log.Logger
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = LoggerFrom(r.Context())
		w.WriteHeader(200)
	}), RequestID(), Logger(log.Empty()))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/", nil)
	h.ServeHTTP(w, req)

	if got == nil {
		t.Fatal("logger missing from context")
	}
}

func TestRequestIDFrom(t *testing.T) {
	ctx := ctxval.WithRequestID(context.Background(), "abc")
	if RequestIDFrom(ctx) != "abc" {
		t.Fatal("RequestIDFrom mismatch")
	}
	if got := RequestIDFrom(context.Background()); got != "" {
		t.Fatalf("expected empty for bare context, got %s", got)
	}
}

// TestSanitizeRequestID_StripsLogInjectionVectors covers the M1 fix:
// the sanitizer must drop bytes outside the safe ASCII subset so log
// injection vectors (Unicode line separators, ANSI escapes, raw
// control characters) cannot ride through the X-Request-ID header.
func TestSanitizeRequestID_StripsLogInjectionVectors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// ANSI escape sequence: \x1b and `[` are stripped, so residual
		// "31mDEF" is plain text and no longer an ANSI directive — the
		// terminal will not interpret it as a color change.
		{"ansi_escape", "abc\x1b[31mDEF", "abc31mDEF"},
		{"newline", "abc\ndef", "abcdef"},
		{"unicode_line_sep", "abc\u2028def", "abcdef"},
		{"unicode_para_sep", "abc\u2029def", "abcdef"},
		{"high_byte", "abc\xff\xfe", "abc"},
		{"slash", "id/with/slash", "idwithslash"},
		{"plus", "abc+def", "abcdef"},
		{"safe_dash_dot_underscore", "req-id_v.1", "req-id_v.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeRequestID(tc.in); got != tc.want {
				t.Fatalf("sanitizeRequestID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeRequestID_AllInvalidGeneratesNew verifies fall-back when
// every input byte gets stripped — we must hand back a fresh random ID
// rather than empty string so log correlation never breaks.
func TestSanitizeRequestID_AllInvalidGeneratesNew(t *testing.T) {
	got := sanitizeRequestID("\x00\x01\xff\u2028")
	if got == "" {
		t.Fatal("expected fallback random ID for fully-stripped input")
	}
	if len(got) != 32 { // hex(16 bytes) = 32 chars
		t.Fatalf("expected 32-char generated id, got %d", len(got))
	}
}

// captureLogger records Info lines for access-log assertions.
type captureLogger struct {
	log.Logger
	mu    sync.Mutex
	lines []map[string]any
}

func newCaptureLogger() *captureLogger { return &captureLogger{Logger: log.Empty()} }

func (c *captureLogger) Info(msg string, kv ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	line := map[string]any{"msg": msg}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			line[k] = kv[i+1]
		}
	}
	c.lines = append(c.lines, line)
}

func (c *captureLogger) last() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.lines) == 0 {
		return nil
	}
	return c.lines[len(c.lines)-1]
}

func TestAccessLog_RecordsPatternStatusAndClientIP(t *testing.T) {
	cl := newCaptureLogger()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate the web router filling the pattern slot at dispatch.
		ctxval.RoutePatternHolder(r.Context()).Set("/users/{rid}")
		w.WriteHeader(http.StatusCreated)
	})
	h := chain(inner, AccessLog(cl))

	w := newTrackingRecorder()
	req, _ := http.NewRequest("POST", "/users/usr_1", nil)
	ctx, _ := ctxval.WithRoutePattern(req.Context())
	ctx = ctxval.WithClientIP(ctx, "203.0.113.7")
	h.ServeHTTP(w, req.WithContext(ctx))

	line := cl.last()
	if line == nil {
		t.Fatal("no access line recorded")
	}
	if line["path"] != "/users/{rid}" {
		t.Fatalf("path = %v, want route pattern", line["path"])
	}
	if line["status"] != http.StatusCreated {
		t.Fatalf("status = %v, want 201", line["status"])
	}
	if line["client_ip"] != "203.0.113.7" {
		t.Fatalf("client_ip = %v", line["client_ip"])
	}
}

func TestAccessLog_UnmatchedPath(t *testing.T) {
	cl := newCaptureLogger()
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
	}), AccessLog(cl))

	w := newTrackingRecorder()
	req, _ := http.NewRequest("GET", "/nope", nil)
	ctx, _ := ctxval.WithRoutePattern(req.Context())
	h.ServeHTTP(w, req.WithContext(ctx))

	if line := cl.last(); line["path"] != "unmatched" {
		t.Fatalf("path = %v, want unmatched", line["path"])
	}
}

func TestCORS_SetsNumericMaxAge(t *testing.T) {
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}), CORS(WithMaxAge(600)))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodOptions, "/test", nil)
	req.Header.Set("Origin", "https://example.com")
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight expected 204, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got != "600" {
		t.Fatalf("expected Access-Control-Max-Age=600, got %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Fatalf("Allow-Origin = %q", got)
	}
}

func TestCORS_CredentialsWithWildcardPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected construction panic")
		}
	}()
	CORS(WithAllowCredentials(true)) // default origins include "*"
}

func TestCORS_DisallowedOriginPassesThrough(t *testing.T) {
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}), CORS(WithAllowOrigins("https://ok.example")))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://evil.example")
	h.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("disallowed origin must not receive ACAO")
	}
	if w.Code != 200 {
		t.Fatalf("request should still reach the handler, got %d", w.Code)
	}
}

func TestTimeout_Writes504WhenHandlerRespectsCtx(t *testing.T) {
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // cooperative handler: bail without writing
	}), Timeout(10*time.Millisecond))

	w := newTrackingRecorder()
	req, _ := http.NewRequest("GET", "/slow", nil)
	h.ServeHTTP(w, req)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", w.Code)
	}
	var resp struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Reason != "GatewayTimeout" {
		t.Fatalf("expected GatewayTimeout envelope, got %s", w.Body.String())
	}
}

// TestTimeout_NoDoubleWriteWhenHandlerWrote pins the second third of
// the §4.2 written-tracking contract.
func TestTimeout_NoDoubleWriteWhenHandlerWrote(t *testing.T) {
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		// Handler wrote a response despite the deadline.
		w.WriteHeader(200)
		w.Write([]byte(`{"late":true}`)) //nolint:errcheck
	}), Timeout(10*time.Millisecond))

	w := newTrackingRecorder()
	req, _ := http.NewRequest("GET", "/slow", nil)
	h.ServeHTTP(w, req)

	if w.Status() != 200 {
		t.Fatalf("handler response must win, got %d", w.Status())
	}
	if strings.Contains(w.Body.String(), "GatewayTimeout") {
		t.Fatalf("timeout must not double-write: %s", w.Body.String())
	}
}

func TestTimeout_ZeroDisables(t *testing.T) {
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, has := r.Context().Deadline(); has {
			t.Error("zero timeout must not set a deadline")
		}
		w.WriteHeader(200)
	}), Timeout(0))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/", nil)
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
}

func mustResolver(t *testing.T, trusted ...string) *clientip.Resolver {
	t.Helper()
	res, err := clientip.NewResolver(trusted)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestClientIP_StoresResolvedAddress(t *testing.T) {
	res := mustResolver(t)
	var seen string
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = ctxval.ClientIPFrom(r.Context())
		w.WriteHeader(200)
	}), ClientIP(res))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Forwarded-For", "10.1.1.1") // spoof attempt — no trusted proxies
	h.ServeHTTP(w, req)

	if seen != "203.0.113.9" {
		t.Fatalf("client ip = %q, want socket peer", seen)
	}
}

func TestTimeout_504EnvelopeCarriesRequestID(t *testing.T) {
	// End-to-end shape of the v1 timeoutBodyFor contract: the 504
	// envelope carries the request id stamped by RequestID.
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}), RequestID(), Timeout(5*time.Millisecond))

	w := newTrackingRecorder()
	req, _ := http.NewRequest("GET", "/slow", nil)
	req.Header.Set("X-Request-ID", "rid-504")
	h.ServeHTTP(w, req)

	var resp struct {
		RequestID string `json:"request_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.RequestID != "rid-504" {
		t.Fatalf("504 envelope lost request_id: %s", w.Body.String())
	}
}

// TestChainOrder_PostProcessingRunsInReverse pins the onion semantics
// that replace gin's c.Next: post-logic after next.ServeHTTP runs in
// reverse registration order (SPEC §4.2 item 2).
func TestChainOrder_PostProcessingRunsInReverse(t *testing.T) {
	var order []string
	mk := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+"-pre")
				next.ServeHTTP(w, r)
				order = append(order, name+"-post")
			})
		}
	}
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		order = append(order, "handler")
		w.WriteHeader(200)
	}), mk("outer"), mk("inner"))

	req, _ := http.NewRequest("GET", "/", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	want := []string{"outer-pre", "inner-pre", "handler", "inner-post", "outer-post"}
	if len(order) != len(want) {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}
