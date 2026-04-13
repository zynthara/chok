package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type corsConfig struct {
	allowOrigins []string
	allowMethods []string
	allowHeaders []string
	maxAge       int
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

func WithMaxAge(seconds int) CORSOption {
	return func(c *corsConfig) { c.maxAge = seconds }
}

// CORS returns a CORS middleware.
func CORS(opts ...CORSOption) gin.HandlerFunc {
	cfg := &corsConfig{
		allowOrigins: []string{"*"},
		allowMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		allowHeaders: []string{"Origin", "Content-Type", "Accept", "Authorization", "X-Request-ID"},
		maxAge:       86400,
	}
	for _, o := range opts {
		o(cfg)
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			c.Next()
			return
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

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
