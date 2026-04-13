// Package account provides a ready-to-use user module with registration,
// login, token refresh, and password management.
package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/auth"
	"github.com/zynthara/chok/auth/jwt"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/handler"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/middleware"
	"github.com/zynthara/chok/store"
)

// Sender delivers a password-reset code to the user (email, SMS, etc.).
// The module does not impose a delivery mechanism — callers provide their own.
type Sender interface {
	Send(ctx context.Context, to string, code string) error
}

// Module manages user accounts.
type Module struct {
	jwt      *jwt.Manager
	resetJWT *jwt.Manager // short-lived tokens for password reset
	store    *store.Store[User]
	sender   Sender // nil → forgot/reset-password routes are not registered
	logger   log.Logger
}

// Option configures a Module.
type Option func(*moduleConfig)

type moduleConfig struct {
	signingKey     string
	expiration     time.Duration
	resetExpiration time.Duration
	sender         Sender
}

// WithSigningKey sets the JWT signing key (required, >= 32 bytes).
func WithSigningKey(key string) Option {
	return func(c *moduleConfig) { c.signingKey = key }
}

// WithExpiration sets the access token lifetime. Defaults to 2 hours.
func WithExpiration(d time.Duration) Option {
	return func(c *moduleConfig) { c.expiration = d }
}

// WithResetExpiration sets the password-reset token lifetime. Defaults to 15 minutes.
func WithResetExpiration(d time.Duration) Option {
	return func(c *moduleConfig) { c.resetExpiration = d }
}

// WithSender enables the forgot/reset-password flow.
func WithSender(s Sender) Option {
	return func(c *moduleConfig) { c.sender = s }
}

// New creates an account module.
func New(gdb *gorm.DB, logger log.Logger, opts ...Option) (*Module, error) {
	cfg := &moduleConfig{
		expiration:      2 * time.Hour,
		resetExpiration: 15 * time.Minute,
	}
	for _, o := range opts {
		o(cfg)
	}

	jwtMgr, err := jwt.NewManager(jwt.Options{
		SigningKey:  cfg.signingKey,
		Expiration: cfg.expiration,
	})
	if err != nil {
		return nil, err
	}

	resetMgr, err := jwt.NewManager(jwt.Options{
		SigningKey:  cfg.signingKey,
		Expiration: cfg.resetExpiration,
	})
	if err != nil {
		return nil, err
	}

	s := store.New[User](gdb, logger,
		store.WithQueryFields("id", "email", "name", "created_at"),
		store.WithUpdateFields("name", "email", "password_hash", "roles", "active"),
	)

	return &Module{
		jwt:      jwtMgr,
		resetJWT: resetMgr,
		store:    s,
		sender:   cfg.sender,
		logger:   logger,
	}, nil
}

// TokenParser returns the JWT manager for use with middleware.Authn.
func (m *Module) TokenParser() middleware.TokenParser {
	return m.jwt
}

// PrincipalResolver returns a resolver that builds a Principal from JWT claims.
// Roles are read from the "roles" claim in the token — no DB lookup required.
func (m *Module) PrincipalResolver() middleware.PrincipalResolver {
	return func(_ context.Context, subject string, claims map[string]any) (auth.Principal, error) {
		p := auth.Principal{Subject: subject, Claims: claims}
		if name, ok := claims["name"].(string); ok {
			p.Name = name
		}
		if roles, ok := claims["roles"].([]any); ok {
			for _, r := range roles {
				if s, ok := r.(string); ok {
					p.Roles = append(p.Roles, s)
				}
			}
		}
		return p, nil
	}
}

// ActiveCheck returns a gin middleware that verifies the authenticated user
// still exists and is active by hitting the database on every request.
//
// PrincipalResolver is stateless by design (no DB per request). Apply
// ActiveCheck to routes where real-time revocation matters:
//
//	api := srv.Group("/api/v1")
//	api.Use(middleware.Authn(acct.TokenParser(), acct.PrincipalResolver()))
//	api.Use(acct.ActiveCheck())
func (m *Module) ActiveCheck() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := auth.PrincipalFrom(c.Request.Context())
		if !ok {
			handler.WriteResponse(c, 0, nil, apierr.ErrUnauthenticated)
			c.Abort()
			return
		}
		user, err := m.store.GetOne(c.Request.Context(), p.Subject)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				handler.WriteResponse(c, 0, nil, apierr.ErrUnauthenticated.WithMessage("account not found"))
			} else {
				handler.WriteResponse(c, 0, nil, apierr.ErrInternal)
			}
			c.Abort()
			return
		}
		if !user.Active {
			handler.WriteResponse(c, 0, nil, apierr.ErrUnauthenticated.WithMessage("account is disabled"))
			c.Abort()
			return
		}
		c.Next()
	}
}

// RouteGroup is the minimal interface for registering routes.
// *gin.RouterGroup satisfies this (use srv.Group("/path") to obtain one).
type RouteGroup interface {
	POST(string, ...gin.HandlerFunc) gin.IRoutes
	PUT(string, ...gin.HandlerFunc) gin.IRoutes
	Group(string, ...gin.HandlerFunc) *gin.RouterGroup
}

// Setup creates the account module from config, migrates the User table,
// and registers routes — all in one call.
//
// Returns (nil, nil) if opts is nil or opts.Enabled is false.
// Extra modOpts (e.g. WithSender) are applied on top of config values.
//
//	acct, err := account.Setup(gdb, logger, &cfg.Account, srv.Group("/auth"))
func Setup(gdb *gorm.DB, logger log.Logger, opts *config.AccountOptions, r RouteGroup, modOpts ...Option) (*Module, error) {
	if opts == nil || !opts.Enabled {
		return nil, nil
	}

	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("account: %w", err)
	}

	if err := db.Migrate(gdb, Table()); err != nil {
		return nil, fmt.Errorf("account: migrate: %w", err)
	}

	combined := []Option{
		WithSigningKey(opts.SigningKey),
	}
	if opts.Expiration > 0 {
		combined = append(combined, WithExpiration(opts.Expiration))
	}
	if opts.ResetExpiration > 0 {
		combined = append(combined, WithResetExpiration(opts.ResetExpiration))
	}
	combined = append(combined, modOpts...)

	m, err := New(gdb, logger, combined...)
	if err != nil {
		return nil, fmt.Errorf("account: %w", err)
	}

	m.RegisterRoutes(r)
	return m, nil
}

// RegisterRoutes registers account API routes on the given router.
//
// Public routes (no auth required):
//
//	POST /register
//	POST /login
//	POST /forgot-password   (only if WithSender is configured)
//	POST /reset-password    (only if WithSender is configured)
//
// Authenticated routes:
//
//	POST /refresh-token
//	PUT  /change-password
func (m *Module) RegisterRoutes(r RouteGroup) {
	r.POST("/register", handler.HandleRequest(m.register, handler.WithSuccessCode(201)))
	r.POST("/login", handler.HandleRequest(m.login))

	if m.sender != nil {
		r.POST("/forgot-password", handler.HandleAction(m.forgotPassword))
		r.POST("/reset-password", handler.HandleAction(m.resetPassword))
	}

	authed := r.Group("", middleware.Authn(m.jwt, m.PrincipalResolver()))
	authed.POST("/refresh-token", handler.HandleRequest(m.refreshToken))
	authed.PUT("/change-password", handler.HandleAction(m.changePassword))
}
