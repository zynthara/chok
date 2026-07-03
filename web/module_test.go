package web

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// startServe runs Serve on a component bound to :0 and returns the
// base URL plus a cancel + wait pair.
func startServe(t *testing.T, c *Component) (base string, cancel context.CancelFunc, wait func() error) {
	t.Helper()
	ctx, cancelFn := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	readyCh := make(chan struct{})
	go func() {
		errCh <- c.Serve(ctx, func() { close(readyCh) })
	}()
	select {
	case <-readyCh:
	case err := <-errCh:
		cancelFn()
		t.Fatalf("Serve exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		cancelFn()
		t.Fatal("Serve never became ready")
	}
	return "http://" + c.BoundAddr(), cancelFn, func() error {
		select {
		case err := <-errCh:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("Serve did not return after cancel")
			return nil
		}
	}
}

func TestServe_StartServeStop(t *testing.T) {
	c := newWebComponent(t, `
http:
  addr: "127.0.0.1:0"
`, nil)
	c.router.Handle("GET", "/ping", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("pong"))
	}))

	base, cancel, wait := startServe(t, c)

	resp, err := http.Get(base + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "pong" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}

	cancel()
	if err := wait(); err != nil {
		t.Fatalf("clean stop expected nil, got %v", err)
	}
}

func TestServe_InFlightRequestFinishesBeforeReturn(t *testing.T) {
	c := newWebComponent(t, `
http:
  addr: "127.0.0.1:0"
  shutdown_timeout: "5s"
`, nil)

	inHandler := make(chan struct{})
	var finished atomic.Bool
	c.router.Handle("GET", "/slow", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(inHandler)
		time.Sleep(300 * time.Millisecond)
		finished.Store(true)
		w.WriteHeader(200)
	}))

	base, cancel, wait := startServe(t, c)

	got := make(chan int, 1)
	go func() {
		resp, err := http.Get(base + "/slow")
		if err != nil {
			got <- -1
			return
		}
		resp.Body.Close()
		got <- resp.StatusCode
	}()

	<-inHandler
	cancel() // stop while the request is in flight

	if err := wait(); err != nil {
		t.Fatalf("graceful stop failed: %v", err)
	}
	// Draining contract: Serve returned ⇒ the in-flight request is done.
	if !finished.Load() {
		t.Fatal("Serve returned before the in-flight handler finished")
	}
	if code := <-got; code != 200 {
		t.Fatalf("in-flight request got %d", code)
	}
}

func TestServe_ForceClosesAfterShutdownTimeout(t *testing.T) {
	c := newWebComponent(t, `
http:
  addr: "127.0.0.1:0"
  shutdown_timeout: "150ms"
`, nil)

	inHandler := make(chan struct{})
	release := make(chan struct{})
	c.router.Handle("GET", "/hang", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inHandler)
		// Ignores ctx cancellation — the hung-handler case Stop's
		// force-Close exists for.
		<-release
	}))

	base, cancel, wait := startServe(t, c)

	go func() {
		resp, err := http.Get(base + "/hang")
		if err == nil {
			resp.Body.Close()
		}
	}()

	<-inHandler
	start := time.Now()
	cancel()

	err := wait()
	close(release)
	if err == nil {
		t.Fatal("expected a shutdown-timeout error from Serve")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error from Shutdown, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("force-close took too long: %s (hung handler outlived teardown)", elapsed)
	}
}

func TestServe_PreCancelledContext(t *testing.T) {
	c := newWebComponent(t, `
http:
  addr: "127.0.0.1:0"
`, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Serve(ctx, func() { t.Fatal("ready must not fire") }); err == nil {
		t.Fatal("pre-cancelled Serve must return an error")
	}
}

func TestServe_ListenFailure(t *testing.T) {
	// Occupy a port, then ask the component to bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	c := newWebComponent(t, fmt.Sprintf(`
http:
  addr: %q
`, ln.Addr().String()), nil)
	if err := c.Serve(context.Background(), func() { t.Fatal("ready must not fire") }); err == nil {
		t.Fatal("expected listen error")
	}
}

func TestServe_H2CServesHTTP2Cleartext(t *testing.T) {
	c := newWebComponent(t, `
http:
  addr: "127.0.0.1:0"
  h2c: true
`, nil)
	c.router.Handle("GET", "/proto", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Proto))
	}))

	base, cancel, wait := startServe(t, c)
	defer func() {
		cancel()
		_ = wait()
	}()

	client := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}
	resp, err := client.Get(base + "/proto")
	if err != nil {
		t.Fatalf("h2c request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.ProtoMajor != 2 || !strings.HasPrefix(string(body), "HTTP/2") {
		t.Fatalf("expected HTTP/2 over cleartext, got proto=%s body=%q", resp.Proto, body)
	}
}

// TestServe_WriteResponseGuardEndToEnd exercises the third no-double-
// write consumer (§4.2 item 2) against the real wrapped writer: a
// handler whose WriteResponse runs after a middleware already wrote.
func TestServe_WrittenGuardEndToEnd(t *testing.T) {
	preempt := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("preempt") == "1" {
				w.WriteHeader(418)
				_, _ = w.Write([]byte(`teapot`))
			}
			next.ServeHTTP(w, r) // handler still runs; its write must no-op
		})
	}
	c := newWebComponent(t, `
http:
  addr: "127.0.0.1:0"
`, nil, WithMiddleware(preempt))
	c.router.Handle("GET", "/g", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ww, ok := w.(interface{ Written() bool }); ok && ww.Written() {
			return // guarded, same check handler.WriteResponse performs
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("normal"))
	}))

	base, cancel, wait := startServe(t, c)
	defer func() {
		cancel()
		_ = wait()
	}()

	resp, err := http.Get(base + "/g?preempt=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 418 || string(body) != "teapot" {
		t.Fatalf("preempted response corrupted: %d %q", resp.StatusCode, body)
	}
}
