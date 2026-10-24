package chok

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func listeningApp(t *testing.T, mutate func(*Base)) *App {
	t.Helper()
	base := testBase(t)
	base.HTTP.Enabled = true
	base.HTTP.Addr = "127.0.0.1:0"
	if mutate != nil {
		mutate(&base)
	}
	return New(base, Logger(quiet()))
}

func TestServe_StartServeStop(t *testing.T) {
	app := listeningApp(t, nil)
	GET(app.Router(), "/hi", func(context.Context, *None) (string, error) { return "hi", nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(app, ctx)
	waitFor(t, app.ready.Load, "ready")
	resp, err := http.Get("http://" + app.Addr() + "/hi")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.TrimSpace(string(b)) != `"hi"` {
		t.Fatalf("got %s", b)
	}
	cancel()
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get("http://" + app.Addr() + "/hi"); err == nil {
		t.Fatal("the server must be closed after Run returns")
	}
}

func TestServe_InFlightRequestFinishesBeforeReturn(t *testing.T) {
	app := listeningApp(t, nil)
	started, release := make(chan struct{}), make(chan struct{})
	GET(app.Router(), "/slow", func(context.Context, *None) (string, error) {
		close(started)
		<-release
		return "done", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(app, ctx)
	waitFor(t, app.ready.Load, "ready")
	result := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + app.Addr() + "/slow")
		if err != nil {
			result <- err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		result <- strings.TrimSpace(string(b))
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Run returned before the in-flight request finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if got := <-result; got != `"done"` {
		t.Fatalf("the in-flight request completes: %s", got)
	}
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestServe_ForceClosesAfterShutdownTimeout(t *testing.T) {
	app := listeningApp(t, func(b *Base) { b.HTTP.ShutdownTimeout = 50 * time.Millisecond })
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	GET(app.Router(), "/stuck", func(context.Context, *None) (None, error) {
		close(started)
		<-release
		return None{}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(app, ctx)
	waitFor(t, app.ready.Load, "ready")
	go func() { _, _ = http.Get("http://" + app.Addr() + "/stuck") }()
	<-started
	cancel()
	err := await(t, done)
	if err == nil || !strings.Contains(err.Error(), "http shutdown") {
		t.Fatalf("a stuck request is cut off and reported: %v", err)
	}
}

func TestServe_PreCanceledContext(t *testing.T) {
	app := listeningApp(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestServe_ListenFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	app := listeningApp(t, func(b *Base) { b.HTTP.Addr = ln.Addr().String() })
	if err := app.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "chok: http") {
		t.Fatalf("a taken address fails Run: %v", err)
	}
}

func TestServe_H2CServesHTTP2Cleartext(t *testing.T) {
	app := listeningApp(t, func(b *Base) { b.HTTP.H2C = true })
	GET(app.Router(), "/proto", func(_ context.Context, _ *None) (string, error) { return "ok", nil })
	app.Router().Handle(http.MethodGet, "/raw", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(app, ctx)
	waitFor(t, app.ready.Load, "ready")
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	client := &http.Client{Transport: &http.Transport{Protocols: &protocols}}
	resp, err := client.Get("http://" + app.Addr() + "/raw")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "HTTP/2.0" {
		t.Fatalf("got %s", b)
	}
	cancel()
	_ = await(t, done)
}
