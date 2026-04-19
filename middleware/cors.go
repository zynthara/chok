package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type corsConfig struct {
	allowOrigins     []string
	allowMethods     []string
	allowHeaders     []string
	exposeHeaders    []string
	allowCredentials bool
	maxAge           int
}

// CORSOption configures the CORS middleware.
type CORSOption func(*corsConfig)

func WithAllowOrigins(origins ...string) CORSOption {
	return func(c *corsConfig) { c.allowOrigins = origins }
}

func WithAllowMethods(methods ...string) CORSOption {
	return func(c *corsConfig) { c.allowMethods = methods }
}

func WithAllowHeaders(headers ...string) CORSOption {
	return func(c *corsConfig) { c.allowHeaders = headers }
}

// WithExposeHeaders lists response headers that browsers should make
// visible to JavaScript. By default only simple headers are accessible
// to cross-origin fetchers; anything else (X-Request-ID, Retry-After,
// custom pagination cursors, etc.) needs explicit listing here or the
// client JS can't read them.
func WithExposeHeaders(headers ...string) CORSOption {
	return func(c *corsConfig) { c.exposeHeaders = headers }
}

// WithAllowCredentials enables Access-Control-Allow-Credentials: true,
// permitting browsers to send cookies / Authorization headers on cross-
// origin requests. Incompatible with a wildcard Origin — CORS() panics
// at setup time if both are configured, because the combination is a
// credential-leak vector (any origin could read authenticated responses).
func WithAllowCredentials(allow bool) CORSOption {
	return func(c *corsConfig) { c.allowCredentials = allow }
}

func WithMaxAge(seconds int) CORSOption {
	return func(c *corsConfig) { c.maxAge = seconds }
}

// CORS returns a CORS middleware.
func CORS(opts ...CORSOption) gin.HandlerFunc {
	cfg := &corsConfig{
		allowOrigins:  []string{"*"},
		allowMethods:  []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		allowHeaders:  []string{"Origin", "Content-Type", "Accept", "Authorization", "X-Request-ID"},
		exposeHeaders: []string{"X-Request-ID", "Retry-After"},
		maxAge:        86400,
	}
	for _, o := range opts {
		o(cfg)
	}

	if cfg.allowCredentials {
		for _, o := range cfg.allowOrigins {
			if o == "*" {
				panic("middleware.CORS: WithAllowCredentials(true) cannot be combined with wildcard '*' Origin — this would leak credentials to any site")
			}
		}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			c.Next()
			return
		}

		// Append Vary: Origin so caches don't serve a response with one
		// origin's ACAO header to a request from a different origin.
		// Use Add (not Set) to preserve any existing Vary values.
		c.Writer.Header().Add("Vary", "Origin")
		// Preflight responses additionally depend on the requested method
		// and headers. Without these Vary entries, a CDN could cache the
		// preflight for POST and serve it to a later PUT (or vice versa),
		// making the browser reject legitimate cross-origin requests.
		if c.Request.Method == http.MethodOptions {
			c.Writer.Header().Add("Vary", "Access-Control-Request-Method")
			c.Writer.Header().Add("Vary", "Access-Control-Request-Headers")
		}

		allowed := false
		for _, o := range cfg.allowOrigins {
			if o == "*" || o == origin {
				allowed = true
				break
			}
		}
		if !allowed {
			c.Next()
			return
		}

		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Methods", strings.Join(cfg.allowMethods, ", "))
		c.Header("Access-Control-Allow-Headers", strings.Join(cfg.allowHeaders, ", "))
		c.Header("Access-Control-Max-Age", strconv.Itoa(cfg.maxAge))
		if len(cfg.exposeHeaders) > 0 {
			c.Header("Access-Control-Expose-Headers", strings.Join(cfg.exposeHeaders, ", "))
		}
		if cfg.allowCredentials {
			c.Header("Access-Control-Allow-Credentials", "true")
		}

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
