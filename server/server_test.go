package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/config"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestNewHTTPServer_DoesNotChangeGinMode(t *testing.T) {
	origMode := gin.Mode()
	gin.SetMode(gin.DebugMode)
	defer gin.SetMode(origMode)

	_ = NewHTTPServer(&config.HTTPOptions{
		Addr:         ":0",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})

	if got := gin.Mode(); got != gin.DebugMode {
		t.Fatalf("NewHTTPServer should not change gin mode, got %q", got)
	}
}

func TestHTTPServer_StartStop(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{
		Addr:         ":0", // random port
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})
	srv.GET("/ping", func(c *gin.Context) { c.String(200, "pong") })

	readyCh := make(chan struct{})
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- srv.Start(context.Background(), func() { close(readyCh) })
	}()

	select {
	case <-readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not become ready")
	}

	// Make a request.
	addr := srv.ln.Addr().String()
	resp, err := http.Get("http://" + addr + "/ping")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Stop.
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("stop error: %v", err)
	}

	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Stop")
	}
}

func TestHTTPServer_StopIdempotent(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{
		Addr:         ":0",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})

	readyCh := make(chan struct{})
	go func() {
		srv.Start(context.Background(), func() { close(readyCh) })
	}()
	<-readyCh

	// Double stop should not error.
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

func TestHTTPServer_StopBeforeStart(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{
		Addr:         ":0",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})
	// Stop before Start should not panic.
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("stop before start: %v", err)
	}
}

func TestHTTPServer_CtxCancelPreReady_AbortsStart(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{
		Addr:         ":0",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Start

	readyCalled := false
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- srv.Start(ctx, func() { readyCalled = true })
	}()

	select {
	case err := <-doneCh:
		if err == nil {
			t.Fatal("Start should return error when ctx is cancelled pre-ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}

	if readyCalled {
		t.Fatal("ready() must not be called when ctx is cancelled pre-ready")
	}
}

func TestHTTPServer_StopPreReady_ReadyNeverCalled(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{
		Addr:         ":0",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})

	readyCalled := false
	doneCh := make(chan error, 1)

	// Stop immediately — before Start can call ready().
	srv.Stop(context.Background())

	go func() {
		doneCh <- srv.Start(context.Background(), func() { readyCalled = true })
	}()

	select {
	case err := <-doneCh:
		if err == nil {
			t.Fatal("Start should return error when stopped pre-ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after pre-ready Stop")
	}

	if readyCalled {
		t.Fatal("ready() must not be called when Stop fires before ready")
	}
}

func TestHTTPServer_SIGQUITFastShutdown(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{
		Addr:         ":0",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})

	readyCh := make(chan struct{})
	go func() {
		srv.Start(context.Background(), func() { close(readyCh) })
	}()
	<-readyCh

	// Simulate SIGQUIT: pass already-cancelled ctx.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := srv.Stop(ctx)
	// Should return nil — skipping drain on cancelled ctx is expected.
	if err != nil {
		t.Fatalf("fast stop error: %v", err)
	}
}

// regression: SIGQUIT with an active request must still return nil.
func TestHTTPServer_SIGQUITFastShutdown_ActiveRequest(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{
		Addr:         ":0",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})

	handlerReached := make(chan struct{})
	srv.GET("/slow", func(c *gin.Context) {
		close(handlerReached)
		<-c.Request.Context().Done()
	})

	readyCh := make(chan struct{})
	go func() {
		srv.Start(context.Background(), func() { close(readyCh) })
	}()
	<-readyCh

	// Start an in-flight request.
	addr := srv.ln.Addr().String()
	go http.Get("http://" + addr + "/slow")

	select {
	case <-handlerReached:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach handler")
	}

	// SIGQUIT: pass already-cancelled ctx — should return nil even with active connection.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := srv.Stop(ctx)
	if err != nil {
		t.Fatalf("fast stop with active request should return nil, got: %v", err)
	}
}

// --- P2: 404/405, healthz, pprof tests ---

func TestNoRoute_404(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{Addr: ":0"})
	srv.GET("/exists", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/does-not-exist", nil)
	srv.engine.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["reason"] != "NotFound" {
		t.Fatalf("reason = %v, want NotFound", resp["reason"])
	}
}

func TestNoMethod_405(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{Addr: ":0"})
	srv.GET("/resource", func(c *gin.Context) { c.Status(200) })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/resource", nil)
	srv.engine.ServeHTTP(w, req)

	if w.Code != 405 {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["reason"] != "MethodNotAllowed" {
		t.Fatalf("reason = %v, want MethodNotAllowed", resp["reason"])
	}
}

func TestRegisterHealthz_Healthy(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{Addr: ":0"})
	RegisterHealthz(srv)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/healthz", nil)
	srv.engine.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "healthy" {
		t.Fatalf("status = %v, want healthy", resp["status"])
	}
}

func TestRegisterHealthz_Unhealthy(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{Addr: ":0"})
	RegisterHealthz(srv, func(_ context.Context) error {
		return fmt.Errorf("db down")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/healthz", nil)
	srv.engine.ServeHTTP(w, req)

	if w.Code != 503 {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestRegisterHealthz_MultipleCheckers(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{Addr: ":0"})
	RegisterHealthz(srv,
		func(_ context.Context) error { return nil },                      // pass
		func(_ context.Context) error { return errors.New("cache down") }, // fail
	)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/healthz", nil)
	srv.engine.ServeHTTP(w, req)

	if w.Code != 503 {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestRegisterPprof_Registered(t *testing.T) {
	srv := NewHTTPServer(&config.HTTPOptions{Addr: ":0"})
	RegisterPprof(srv)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/debug/pprof/", nil)
	srv.engine.ServeHTTP(w, req)

	// pprof index returns 200 with HTML.
	if w.Code != 200 {
		t.Fatalf("expected 200 for pprof index, got %d", w.Code)
	}
}

// TestStop_ForceClosesAfterShutdownTimeout covers the #2 fix: when a
// handler ignores the request context, srv.Shutdown deadlines out and
// http.Server keeps the connection open. Without the post-Shutdown
// Close(), App.runCleanups would tear down DB/cache while that handler
// is still alive. After the fix, Stop must return promptly and Start
// must unblock so the App's shutdown sequence can proceed.
func TestStop_ForceClosesAfterShutdownTimeout(t *testing.T) {
	handlerEnter := make(chan struct{})
	handlerExit := make(chan struct{})
	srv := NewHTTPServer(&config.HTTPOptions{Addr: "127.0.0.1:0", ReadTimeout: time.Second, WriteTimeout: time.Second})
	srv.engine.GET("/slow", func(c *gin.Context) {
		close(handlerEnter)
		// Ignore ctx and block until the test releases us — the pattern
		// net/http.Server.Shutdown cannot interrupt.
		<-handlerExit
		c.String(200, "late")
	})

	startErr := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		startErr <- srv.Start(context.Background(), func() { close(ready) })
	}()
	<-ready

	srv.mu.Lock()
	addr := srv.ln.Addr().String()
	srv.mu.Unlock()

	clientErr := make(chan error, 1)
	go func() {
		_, err := http.Get("http://" + addr + "/slow")
		clientErr <- err
	}()
	<-handlerEnter

	stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	stopStart := time.Now()
	_ = srv.Stop(stopCtx)
	stopDur := time.Since(stopStart)

	if stopDur > 500*time.Millisecond {
		t.Fatalf("Stop should not exceed 500ms with force-close, took %v", stopDur)
	}

	select {
	case <-startErr:
	case <-time.After(time.Second):
		t.Fatal("Start did not return after Stop force-closed the listener")
	}

	close(handlerExit) // unblock the handler goroutine
	<-clientErr        // drain the client (typically EOF / reset)
}
