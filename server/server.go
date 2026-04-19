package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/handler"
)

// HTTPServer implements the chok.Server interface using Gin.
type HTTPServer struct {
	engine *gin.Engine
	opts   *config.HTTPOptions
	srv    *http.Server
	ln     net.Listener

	mu      sync.Mutex
	stopped bool
	stopCh  chan struct{} // closed by Stop; Start selects on this to detect pre-ready abort
}

// NewHTTPServer creates a new HTTP server.
// Default values are provided by the config layer; the constructor does not second-guess zeroes.
var errMethodNotAllowed = apierr.New(405, "MethodNotAllowed", "method not allowed")

func NewHTTPServer(opts *config.HTTPOptions) *HTTPServer {
	e := gin.New()

	// Unified error responses for unmatched routes/methods.
	e.NoRoute(func(c *gin.Context) {
		handler.WriteResponse(c, 0, nil, apierr.ErrNotFound.WithMessage("route not found"))
	})
	e.NoMethod(func(c *gin.Context) {
		handler.WriteResponse(c, 0, nil, errMethodNotAllowed)
	})
	// Gin requires HandleMethodNotAllowed for NoMethod to fire.
	e.HandleMethodNotAllowed = true

	// Trusted-proxies policy. gin defaults to trusting every proxy,
	// which makes c.ClientIP() honour any X-Forwarded-For the client
	// sends — enough to bypass IP-keyed rate limiting. Fail-closed
	// here: pass the user-supplied list verbatim; empty slice means
	// "trust nobody", so c.ClientIP() returns the direct socket peer.
	//
	// SetTrustedProxies only errors on malformed CIDRs, which would
	// already be a misconfiguration; surface via panic so it surfaces
	// at construction rather than creating a silently-too-permissive
	// server.
	if err := e.SetTrustedProxies(opts.TrustedProxies); err != nil {
		panic("server: invalid TrustedProxies: " + err.Error())
	}

	return &HTTPServer{
		engine: e,
		opts:   opts,
		stopCh: make(chan struct{}),
	}
}

// Route registration (proxy to Gin).

func (s *HTTPServer) Use(middleware ...gin.HandlerFunc) {
	s.engine.Use(middleware...)
}

func (s *HTTPServer) GET(path string, handler ...gin.HandlerFunc)   { s.engine.GET(path, handler...) }
func (s *HTTPServer) POST(path string, handler ...gin.HandlerFunc)  { s.engine.POST(path, handler...) }
func (s *HTTPServer) PUT(path string, handler ...gin.HandlerFunc)   { s.engine.PUT(path, handler...) }
func (s *HTTPServer) PATCH(path string, handler ...gin.HandlerFunc) { s.engine.PATCH(path, handler...) }
func (s *HTTPServer) DELETE(path string, handler ...gin.HandlerFunc) {
	s.engine.DELETE(path, handler...)
}

func (s *HTTPServer) Group(path string, middleware ...gin.HandlerFunc) *gin.RouterGroup {
	return s.engine.Group(path, middleware...)
}

func (s *HTTPServer) Engine() *gin.Engine {
	return s.engine
}

// Start binds the port, calls ready(), then blocks serving until Stop is called.
//
// Stop can be called at any point during Start (including before ready).
// Start uses stopCh to detect Stop between listen and Serve, closing the race window.
func (s *HTTPServer) Start(ctx context.Context, ready func()) error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return err
	}

	s.mu.Lock()
	// Abort if Stop was called or ctx was cancelled before we got here.
	select {
	case <-s.stopCh:
		s.mu.Unlock()
		ln.Close()
		return errors.New("server: stopped before ready")
	case <-ctx.Done():
		s.mu.Unlock()
		ln.Close()
		return ctx.Err()
	default:
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler:           s.engine,
		ReadTimeout:       s.opts.ReadTimeout,
		WriteTimeout:      s.opts.WriteTimeout,
		ReadHeaderTimeout: s.opts.ReadHeaderTimeout,
		IdleTimeout:       s.opts.IdleTimeout,
	}
	// Call ready() while holding mu. Stop() acquires the same mu, so it
	// cannot intervene between this check and the ready() call.
	ready()
	s.mu.Unlock()

	err = s.srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Stop gracefully shuts down the server. It is idempotent and safe to call at any stage.
//
// Closing stopCh ensures Start detects Stop even if called between listen and Serve.
func (s *HTTPServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	close(s.stopCh)
	srv := s.srv
	ln := s.ln
	s.mu.Unlock()

	if srv != nil {
		// Fast shutdown: if ctx is already done, close immediately without
		// draining active connections (e.g. SIGQUIT). Close errors are
		// swallowed — skipping drain on cancelled ctx is normal behavior.
		if ctx.Err() != nil {
			srv.Close()
			return nil
		}
		// Graceful path: ask Shutdown to drain. If the deadline expires
		// before in-flight handlers return, http.Server.Shutdown gives
		// up but does NOT force-close hijacked or long-running
		// connections — they would keep accessing DB / cache / etc that
		// the App is about to tear down a moment later. Force a Close()
		// so those connections actually drop, and return both errors
		// joined so callers see what happened.
		if err := srv.Shutdown(ctx); err != nil {
			closeErr := srv.Close()
			if closeErr != nil {
				return errors.Join(err, closeErr)
			}
			return err
		}
		return nil
	}
	if ln != nil {
		return ln.Close()
	}
	return nil
}
