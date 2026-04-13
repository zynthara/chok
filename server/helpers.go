package server

import (
	"context"
	"net/http/pprof"

	"github.com/gin-gonic/gin"
)

// HealthChecker is a function that returns nil if healthy, error if not.
type HealthChecker func(ctx context.Context) error

// RegisterHealthz registers GET /healthz.
// Should be called before authentication middleware is applied so that
// K8s probes do not receive 401.
func RegisterHealthz(srv *HTTPServer, checks ...HealthChecker) {
	srv.GET("/healthz", func(c *gin.Context) {
		for _, check := range checks {
			if err := check(c.Request.Context()); err != nil {
				c.JSON(503, gin.H{"status": "unhealthy", "error": err.Error()})
				return
			}
		}
		c.JSON(200, gin.H{"status": "healthy"})
	})
}

// RegisterPprof registers /debug/pprof/* routes.
// Not enabled by default — call explicitly in Setup when needed.
func RegisterPprof(srv *HTTPServer) {
	g := srv.engine.Group("/debug/pprof")
	g.GET("/", gin.WrapF(pprof.Index))
	g.GET("/cmdline", gin.WrapF(pprof.Cmdline))
	g.GET("/profile", gin.WrapF(pprof.Profile))
	g.GET("/symbol", gin.WrapF(pprof.Symbol))
	g.GET("/trace", gin.WrapF(pprof.Trace))
	// Catch-all for named profiles (heap, goroutine, allocs, block, mutex,
	// threadcreate, and any custom/future profiles). Uses pprof.Index which
	// dispatches to the correct handler based on the profile name.
	g.GET("/:name", gin.WrapF(pprof.Index))
}
