package parts

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/swagger"
)

// SwaggerResolver extracts title, version, mount prefix, and a bearer-auth
// flag from the app config. Kept as a struct return so additional knobs
// (e.g. custom UI HTML) can grow without breaking the signature.
type SwaggerResolver func(appConfig any) *SwaggerSettings

// SwaggerSettings is the flat view of swagger configuration that
// SwaggerComponent needs. Maps to a subset of config.SwaggerOptions but
// kept independent so the component can be reused with other config
// schemas.
type SwaggerSettings struct {
	Enabled    bool
	Title      string
	Version    string
	Prefix     string // e.g. "/swagger"; defaults to "/swagger"
	BearerAuth bool   // add JWT Bearer security scheme to the spec
}

// SwaggerComponent owns the OpenAPI *swagger.Spec. It implements Router
// — Mount installs the spec endpoint and Swagger UI on the provided
// gin router group.
//
// Handlers that want to contribute operations to the spec obtain it via
// Kernel.Get("swagger").(*SwaggerComponent).Spec() and call
// swagger.Post / swagger.Get / etc. as usual.
type SwaggerComponent struct {
	resolve  SwaggerResolver
	settings *SwaggerSettings
	spec     *swagger.Spec
}

// NewSwaggerComponent constructs the component.
func NewSwaggerComponent(resolve SwaggerResolver) *SwaggerComponent {
	return &SwaggerComponent{resolve: resolve}
}

// Name implements component.Component.
func (s *SwaggerComponent) Name() string { return "swagger" }

// ConfigKey implements component.Component.
func (s *SwaggerComponent) ConfigKey() string { return "swagger" }

// Init builds the spec. When settings.Enabled is false the spec stays
// nil and Mount becomes a no-op — handlers calling swagger.Post with a
// nil Spec simply skip documentation.
func (s *SwaggerComponent) Init(ctx context.Context, k component.Kernel) error {
	settings := s.resolve(k.ConfigSnapshot())
	if settings == nil || !settings.Enabled {
		return nil
	}
	if settings.Title == "" {
		settings.Title = "API"
	}
	if settings.Version == "" {
		settings.Version = "1.0.0"
	}
	if settings.Prefix == "" {
		settings.Prefix = "/swagger"
	}
	s.settings = settings
	s.spec = swagger.New(settings.Title, settings.Version)
	if settings.BearerAuth {
		s.spec.BearerAuth()
	}
	return nil
}

// Close is a no-op.
func (s *SwaggerComponent) Close(ctx context.Context) error { return nil }

// Mount implements component.Router. It expects an interface capable of
// registering GET routes; *gin.Engine and *gin.RouterGroup both satisfy
// it. Called by application code (or a future HTTPComponent) once the
// gin engine is available.
func (s *SwaggerComponent) Mount(router any) error {
	if s.spec == nil {
		return nil
	}
	// When handed a full *gin.Engine, auto-populate the spec from all
	// routes registered so far (handler metadata from HandleRequest /
	// HandleAction / HandleList). Callers should mount swagger AFTER
	// business routes so Populate sees every handler.
	if engine, ok := router.(*gin.Engine); ok {
		swagger.Populate(s.spec, engine)
	}
	r, ok := router.(interface {
		GET(string, ...gin.HandlerFunc) gin.IRoutes
	})
	if !ok {
		return fmt.Errorf("swagger: Mount expected a gin router, got %T", router)
	}
	s.spec.Mount(r, s.settings.Prefix)
	return nil
}

// Spec returns the underlying OpenAPI Spec. nil when disabled.
func (s *SwaggerComponent) Spec() *swagger.Spec { return s.spec }
