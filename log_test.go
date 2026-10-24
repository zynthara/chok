package chok

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zynthara/chok/v3/internal/logtap"
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

// TestBuildLoggers_TappedForTelemetry: the loggers New builds pass their
// records to the handler logtap installs (telemetry's logs.otel bridge),
// the access log and request-scoped attributes included.
func TestBuildLoggers_TappedForTelemetry(t *testing.T) {
	dir := t.TempDir()
	l := buildLoggers(LogConfig{
		Level: "info", Format: "json", Output: []string{dir + "/app.log"},
		Access: true, AccessFiles: []FileConfig{{Path: dir + "/access.log"}},
	})
	t.Cleanup(func() {
		for _, c := range l.closers {
			_ = c.Close()
		}
	})
	var tapped bytes.Buffer
	uninstall := logtap.Install(slog.NewJSONHandler(&tapped, nil))
	defer uninstall()
	l.root.With("request_id", "r-1").Info("handled")
	l.root.Debug("below log.level")
	l.access.Info("access", "status", 204)
	got := tapped.String()
	if !strings.Contains(got, `"msg":"handled","request_id":"r-1"`) || !strings.Contains(got, `"msg":"access","status":204`) {
		t.Fatalf("the tap sees the app and access records: %s", got)
	}
	if strings.Contains(got, "below log.level") {
		t.Fatalf("log.level applies to the tap too: %s", got)
	}
}
