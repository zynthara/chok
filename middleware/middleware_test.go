package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/internal/ctxval"
	"github.com/zynthara/chok/log"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestRecovery_CatchesPanic(t *testing.T) {
	r := gin.New()
	r.Use(Recovery())
	r.GET("/panic", func(c *gin.Context) {
		panic("test panic")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/panic", nil)
	r.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestRecovery_UnifiedErrorFormat(t *testing.T) {
	r := gin.New()
	r.Use(RequestID(), Logger(log.Empty()), Recovery())
	r.GET("/panic", func(c *gin.Context) {
		panic("boom")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/panic", nil)
	r.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Fatalf("expected 500, got %d", w.Code)
	}

	// Verify response matches handler.ErrorResponse format.
	var resp struct {
		Code      int    `json:"code"`
		Reason    string `json:"reason"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp.Code != 500 {
		t.Fatalf("expected code 500, got %d", resp.Code)
	}
	if resp.Reason != "InternalError" {
		t.Fatalf("expected reason InternalError, got %s", resp.Reason)
	}
	if resp.RequestID == "" {
		t.Fatal("expected request_id in panic response")
	}
}

func TestRequestID_GeneratesNew(t *testing.T) {
	r := gin.New()
	r.Use(RequestID())
	r.GET("/test", func(c *gin.Context) {
		rid := ctxval.RequestIDFrom(c.Request.Context())
		c.String(200, rid)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if body == "" {
		t.Fatal("expected generated request ID")
	}
	if w.Header().Get("X-Request-ID") == "" {
		t.Fatal("expected X-Request-ID header")
	}
	if w.Header().Get("X-Request-ID") != body {
		t.Fatal("header and context request ID should match")
	}
}

func TestRequestID_PropagatesExisting(t *testing.T) {
	r := gin.New()
	r.Use(RequestID())
	r.GET("/test", func(c *gin.Context) {
		rid := ctxval.RequestIDFrom(c.Request.Context())
		c.String(200, rid)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-ID", "existing-id-123")
	r.ServeHTTP(w, req)

	if w.Body.String() != "existing-id-123" {
		t.Fatalf("expected propagated ID, got %s", w.Body.String())
	}
}

func TestLogger_InjectsToContext(t *testing.T) {
	l := log.Empty()
	r := gin.New()
	r.Use(Logger(l))
	r.GET("/test", func(c *gin.Context) {
		got := LoggerFrom(c.Request.Context())
		if got == nil {
			c.String(500, "no logger")
			return
		}
		c.String(200, "ok")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequestIDFrom(t *testing.T) {
	if got := RequestIDFrom(nil); got != "" {
		t.Fatalf("expected empty for nil context, got %s", got)
	}
}

func TestAccessLog_DoesNotPanic(t *testing.T) {
	l := log.Empty()
	r := gin.New()
	r.Use(RequestID(), AccessLog(l))
	r.GET("/test", func(c *gin.Context) {
		c.String(200, "ok")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestCORS_SetsNumericMaxAge(t *testing.T) {
	r := gin.New()
	r.Use(CORS(WithMaxAge(600)))
	r.OPTIONS("/test", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodOptions, "/test", nil)
	req.Header.Set("Origin", "https://example.com")
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Max-Age"); got != "600" {
		t.Fatalf("expected Access-Control-Max-Age=600, got %q", got)
	}
}
