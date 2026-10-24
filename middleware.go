package chok

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zynthara/chok/v3/internal/clientip"
	"github.com/zynthara/chok/v3/internal/reqinfo"
)

// Instrumenter is a component that contributes middleware to the default
// stack, between ClientIP and AccessLog — telemetry plugs in this way.
type Instrumenter interface{ Middleware() Middleware }

type ctxKey int

const (
	requestIDKey ctxKey = iota
	clientIPKey
)

// RequestID returns the request's ID: the X-Request-Id it arrived with
// (sanitized) or a generated one. The response echoes it.
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
}

// ClientIP returns the client address, read from forwarding headers only
// when the direct peer is one of http.trusted_proxies.
func ClientIP(ctx context.Context) string {
	s, _ := ctx.Value(clientIPKey).(string)
	return s
}

// buildHandler assembles, outermost first: Recover, RequestID, ClientIP,
// the Instrumenters, AccessLog, Timeout, CORS, CSRF, the Wrap middleware,
// then the routes.
func (a *App) buildHandler() http.Handler {
	var h http.Handler = a.mux
	for _, mw := range slices.Backward(a.wrap) {
		h = mw(h)
	}
	if a.base.HTTP.CSRF {
		h = a.csrf(h)
	}
	if len(a.base.HTTP.CORS.AllowOrigins) > 0 {
		h = cors(a.base.HTTP.CORS, h)
	}
	if d := a.base.HTTP.RequestTimeout; d > 0 {
		h = a.timeout(d, h)
	}
	if a.access != nil {
		h = a.accessLog(h)
	}
	a.mu.Lock()
	instrumenters := slices.Clone(a.instrumenters)
	a.mu.Unlock()
	for _, in := range slices.Backward(instrumenters) {
		h = in.Middleware()(h)
	}
	h = a.clientIP(h)
	h = a.requestID(h)
	return a.recover(h)
}

// statusWriter records the status and size of a response.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach Flush, Hijack and deadlines.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("chok: the response writer cannot hijack")
}

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func trackWriter(w http.ResponseWriter) *statusWriter {
	if sw, ok := w.(*statusWriter); ok {
		return sw
	}
	return &statusWriter{ResponseWriter: w}
}

func (a *App) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := trackWriter(w)
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler {
				panic(p)
			}
			stack := debug.Stack()
			if len(stack) > 8<<10 {
				stack = stack[:8<<10]
			}
			ctx := r.Context()
			LoggerFrom(ctx).ErrorContext(ctx, "chok: panic in handler", "panic", fmt.Sprint(p), "stack", string(stack))
			if sw.status == 0 {
				a.writeError(sw, r, ErrInternal)
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

const requestIDHeader = "X-Request-Id"

func (a *App) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(requestIDHeader))
		w.Header().Set(requestIDHeader, id)
		ctx, _ := reqinfo.New(r.Context())
		ctx = context.WithValue(ctx, requestIDKey, id)
		ctx = withLogger(ctx, a.logger.With("request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sanitizeRequestID keeps an incoming ID that is short and plain, and
// generates one otherwise.
func sanitizeRequestID(id string) string {
	if len(id) > 128 {
		id = id[:128]
	}
	clean := make([]byte, 0, len(id))
	for i := range len(id) {
		switch c := id[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '.', c == '_', c == '-':
			clean = append(clean, c)
		}
	}
	if len(clean) == 0 {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	}
	return string(clean)
}

func (a *App) clientIP(next http.Handler) http.Handler {
	resolver, err := clientip.NewResolver(a.base.HTTP.TrustedProxies)
	if err != nil {
		// Validation should have caught it; never widen trust on a typo.
		a.logger.Error("chok: http.trusted_proxies is invalid; trusting no proxy", "error", err)
		resolver, _ = clientip.NewResolver(nil)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), clientIPKey, resolver.ClientIP(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *App) accessLog(next http.Handler) http.Handler {
	skip := a.probePaths()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(skip, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		begin := time.Now()
		sw := trackWriter(w)
		next.ServeHTTP(sw, r)
		ctx := r.Context()
		route := "unmatched"
		if info := reqinfo.From(ctx); info != nil && info.Route != "" {
			route = info.Route
		}
		a.access.LogAttrs(ctx, slog.LevelInfo, "access",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("route", route),
			slog.Int("status", sw.code()),
			slog.Int64("bytes", sw.bytes),
			slog.Duration("duration", time.Since(begin)),
			slog.String("request_id", RequestID(ctx)),
			slog.String("client_ip", ClientIP(ctx)),
		)
	})
}

var errTimeout = NewError(http.StatusGatewayTimeout, "Timeout", "the request took too long")

func (a *App) timeout(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		sw := trackWriter(w)
		next.ServeHTTP(sw, r.WithContext(ctx))
		if ctx.Err() != nil && sw.status == 0 {
			a.writeError(sw, r, errTimeout)
		}
	})
}

func cors(c CORSConfig, next http.Handler) http.Handler {
	anyOrigin := slices.Contains(c.AllowOrigins, "*")
	methods := strings.Join(c.AllowMethods, ", ")
	headers := strings.Join(c.AllowHeaders, ", ")
	expose := strings.Join(c.ExposeHeaders, ", ")
	maxAge := strconv.Itoa(int(c.MaxAge / time.Second))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if preflight {
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
		}
		if !anyOrigin && !slices.Contains(c.AllowOrigins, origin) {
			next.ServeHTTP(w, r)
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		if c.AllowCredentials {
			h.Set("Access-Control-Allow-Credentials", "true")
		}
		if expose != "" {
			h.Set("Access-Control-Expose-Headers", expose)
		}
		if preflight {
			h.Set("Access-Control-Allow-Methods", methods)
			h.Set("Access-Control-Allow-Headers", headers)
			h.Set("Access-Control-Max-Age", maxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrf rejects cross-origin unsafe requests; the CORS origins are trusted.
func (a *App) csrf(next http.Handler) http.Handler {
	p := http.NewCrossOriginProtection()
	for _, o := range a.base.HTTP.CORS.AllowOrigins {
		if o == "*" {
			continue
		}
		if err := p.AddTrustedOrigin(o); err != nil {
			a.logger.Warn("chok: http.cors origin is not usable for csrf", "origin", o, "error", err)
		}
	}
	p.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.writeError(w, r, ErrForbidden.Msgf("cross-origin request denied"))
	}))
	return p.Handler(next)
}
