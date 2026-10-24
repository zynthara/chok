package chok

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoggerFrom_RequestCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	app := New(testBase(t), Logger(slog.New(slog.NewJSONHandler(&buf, nil))))
	app.Router().Handle(http.MethodGet, "/x", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		LoggerFrom(r.Context()).Info("inside")
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", "abc-123")
	app.Handler().ServeHTTP(httptest.NewRecorder(), req)
	var line string
	for l := range strings.Lines(buf.String()) {
		if strings.Contains(l, `"msg":"inside"`) {
			line = l
		}
	}
	if !strings.Contains(line, `"request_id":"abc-123"`) {
		t.Fatalf("the request logger carries the request id: %s", buf.String())
	}
}

func TestLoggerFrom_DefaultOutsideRequest(t *testing.T) {
	if LoggerFrom(context.Background()) != slog.Default() {
		t.Fatal("outside a request LoggerFrom is slog.Default")
	}
}

func TestBuildLoggers_FormatAndAccessFiles(t *testing.T) {
	dir := t.TempDir()
	l := buildLoggers(LogConfig{
		Level: "debug", Format: "text", Output: []string{dir + "/app.log"},
		Access: true, AccessFiles: []FileConfig{{Path: dir + "/access.log", MaxSizeMB: 1}},
	})
	l.root.Debug("app line")
	l.access.Info("access line")
	for _, c := range l.closers {
		_ = c.Close()
	}
	app, access := readLog(t, dir+"/app.log"), readLog(t, dir+"/access.log")
	if !strings.Contains(app, "level=DEBUG msg=\"app line\"") || strings.Contains(app, "access line") {
		t.Fatalf("app.log: %s", app)
	}
	if !strings.Contains(access, "access line") {
		t.Fatalf("access.log: %s", access)
	}
	if l := buildLoggers(LogConfig{Access: false}); l.access != nil {
		t.Fatal("log.access false disables the access logger")
	}
}
