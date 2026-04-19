package parts

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/zynthara/chok/account"
	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/db"
)

// AccountBuilder constructs the *account.Module given access to the
// live DB, the shared logger, and the Kernel (for additional deps).
// Using a builder (rather than a plain resolver) lets the user inject
// account.WithSender and other options without the component needing
// to model them.
type AccountBuilder func(k component.Kernel, gdb *gorm.DB) (*account.Module, error)

// AccountComponent wires account.Module into the Registry. It's the
// deepest node in chok's built-in dependency graph:
//
//	account → db → (none)
//	account → log → (none)
//
// Migratable installs the User table; Router installs the /auth routes
// onto a gin router group supplied by the caller via Mount.
type AccountComponent struct {
	build  AccountBuilder
	group  string // e.g. "/auth"; empty → mount on root
	module *account.Module
	db     *gorm.DB
}

// NewAccountComponent constructs the component. group is the path
// prefix for the account routes (commonly "/auth"); an empty string
// mounts routes on the router passed to Mount directly.
func NewAccountComponent(build AccountBuilder, group string) *AccountComponent {
	return &AccountComponent{build: build, group: group}
}

// Name implements component.Component.
func (a *AccountComponent) Name() string { return "account" }

// ConfigKey implements component.Component.
func (a *AccountComponent) ConfigKey() string { return "account" }

// Dependencies implements component.Dependent.
func (a *AccountComponent) Dependencies() []string {
	return []string{"db", "log"}
}

// Init retrieves the DBComponent via the Kernel, asserts the gorm.DB is
// available, and hands it to the builder along with the kernel.
func (a *AccountComponent) Init(ctx context.Context, k component.Kernel) error {
	dbc, ok := k.Get("db").(*DBComponent)
	if !ok || dbc == nil {
		return fmt.Errorf("account init: DBComponent not registered")
	}
	gdb := dbc.DB()
	if gdb == nil {
		return fmt.Errorf("account init: gorm.DB not ready")
	}
	a.db = gdb

	mod, err := a.build(k, gdb)
	if err != nil {
		return fmt.Errorf("account init: %w", err)
	}
	// nil module = disabled; Mount, Migrate, Module become no-ops.
	a.module = mod
	return nil
}

// Migrate creates the User table. Account drives its own schema
// (separate from whatever tables the app declared on DBComponent).
func (a *AccountComponent) Migrate(ctx context.Context) error {
	if a.module == nil {
		return nil // disabled
	}
	return db.Migrate(ctx, a.db, account.Table())
}

// Close releases account.Module resources — currently the login rate
// limiter's background cleanup goroutine. The DB connection itself is
// owned by DBComponent, not this component, so no SQL teardown is done
// here.
func (a *AccountComponent) Close(ctx context.Context) error {
	if a.module == nil {
		return nil
	}
	return a.module.Close()
}

// Mount implements component.Router. It registers /register, /login,
// and friends on the provided gin router (typically obtained after
// the app's HTTP server is built). When a non-empty group was set in
// NewAccountComponent, routes mount under that prefix.
//
// Mount is a no-op when the Account module is disabled via config
// (module == nil) — returning nil keeps the ordering contract simple
// for auto-register callers.
func (a *AccountComponent) Mount(router any) error {
	if a.module == nil {
		return nil
	}
	rg, ok := router.(interface {
		Group(string, ...gin.HandlerFunc) *gin.RouterGroup
	})
	if !ok {
		return fmt.Errorf("account: Mount expected a gin router, got %T", router)
	}
	group := rg.Group(a.group)
	a.module.RegisterRoutes(group)
	return nil
}

// Module returns the underlying *account.Module. nil before Init.
func (a *AccountComponent) Module() *account.Module { return a.module }
