package parts

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/component"
)

// DebugComponent exposes a /componentz endpoint that returns structured
// information about the component registry: topology, capabilities,
// init timing, and failed optional components. Intended for development
// and staging environments — disabled by default via DebugOptions.
//
// The endpoint is read-only and does not modify any state. Example
// response:
//
//	{
//	  "phase": "started",
//	  "components": [
//	    {"name":"log", "level":0, "capabilities":["reloadable","healther"], "init_duration_ms":2},
//	    {"name":"db",  "level":0, "capabilities":["migratable","healther"], "init_duration_ms":150}
//	  ]
//	}
type DebugComponent struct {
	path   string
	kernel component.Kernel
}

// NewDebugComponent constructs the component. path defaults to "/componentz".
func NewDebugComponent(path string) *DebugComponent {
	if path == "" {
		path = "/componentz"
	}
	return &DebugComponent{path: path}
}

// Name implements component.Component.
func (d *DebugComponent) Name() string { return "debug" }

// ConfigKey implements component.ConfigKeyer.
func (d *DebugComponent) ConfigKey() string { return "debug" }

// Init captures the kernel reference.
func (d *DebugComponent) Init(_ context.Context, k component.Kernel) error {
	d.kernel = k
	return nil
}

// Close is a no-op.
func (d *DebugComponent) Close(_ context.Context) error { return nil }

// Mount implements component.Router. Registers the /componentz endpoint.
func (d *DebugComponent) Mount(router any) error {
	type debugInfoProvider interface {
		DebugInfo() map[string]any
	}
	r, ok := router.(interface {
		GET(string, ...gin.HandlerFunc) gin.IRoutes
	})
	if !ok {
		// Return an explicit error so wiring bugs surface, matching the
		// behaviour of sibling Router components (metrics, health).
		return fmt.Errorf("debug: Mount expected a gin router, got %T", router)
	}
	r.GET(d.path, func(c *gin.Context) {
		// The kernel is a *Registry which implements DebugInfo.
		if di, ok := d.kernel.(debugInfoProvider); ok {
			c.JSON(http.StatusOK, di.DebugInfo())
			return
		}
		c.JSON(http.StatusOK, gin.H{"error": "debug info not available"})
	})
	return nil
}

// Path returns the configured endpoint path.
func (d *DebugComponent) Path() string { return d.path }
