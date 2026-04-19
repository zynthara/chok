// Package app wires chok Components into the blog example.
//
// Production: NewApp uses WithConfig + WithTables + WithRoutes.
// The framework auto-registers HTTP server, DB, Account, Swagger,
// Health and Metrics components from the Config struct — zero manual
// Register calls needed.
//
// Tests: NewTestRouter still needs explicit wiring because it uses an
// in-memory SQLite connection that outlives the App lifecycle.
package app

import (
	"context"
	"fmt"
	"sync"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/component"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/examples/blog/internal/handler"
	"github.com/zynthara/chok/examples/blog/internal/model"
	blogStore "github.com/zynthara/chok/examples/blog/internal/store"
	"github.com/zynthara/chok/parts"
	chokstore "github.com/zynthara/chok/store"
)

var cfg Config

// blogTables lists tables the DBComponent migrates at startup.
var blogTables = []db.TableSpec{
	db.Table(&model.Post{}, db.SoftUnique("uk_post_title_owner", "title", "owner_id")),
}

// blogRoutes returns the WithRoutes callback that wires business routes.
// Shared between NewApp (production) and NewTestRouter (tests).
func blogRoutes() func(context.Context, *chok.App) error {
	return func(ctx context.Context, a *chok.App) error {
		api := a.API("/api/v1", a.AuthMiddleware())
		gdb := a.DB().(*gorm.DB)
		postStore := blogStore.NewPostStore(chokstore.New[model.Post](gdb, a.Logger()))
		handler.RegisterPostRoutes(api, postStore)
		return nil
	}
}

var mapOnce sync.Once

// NewApp creates the blog application. The framework auto-registers
// every Component from the Config struct fields — all the user provides
// is table definitions and business routes.
func NewApp() *chok.App {
	mapOnce.Do(func() { apierr.RegisterMapper(chokstore.MapError) })
	return chok.New("blog",
		chok.WithConfig(&cfg),
		chok.WithTables(blogTables...),
		chok.WithRoutes(blogRoutes()),
	)
}

// NewTestRouter returns a gin.Engine wired with in-memory SQLite for
// tests. Uses explicit Component registration because the test DB
// connection must outlive the App lifecycle.
func NewTestRouter() *gin.Engine {
	mapOnce.Do(func() { apierr.RegisterMapper(chokstore.MapError) })

	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		panic(err)
	}

	testCfg := &Config{
		Account: config.AccountOptions{
			Enabled:    true,
			SigningKey: "test-signing-key-at-least-32-bytes!",
		},
		Swagger: config.SwaggerOptions{
			Enabled: true,
			Title:   "Blog API",
		},
	}

	httpComp := parts.NewHTTPComponent(
		func(any) *config.HTTPOptions { return &config.HTTPOptions{} },
	).WithoutAccessLog()

	a := chok.New("blog-test",
		chok.WithSetup(func(ctx context.Context, app *chok.App) error {
			app.Register(httpComp)
			app.Register(parts.NewDBComponent(
				func(_ component.Kernel) (*gorm.DB, error) { return gdb, nil },
				blogTables...,
			).WithoutClose())
			app.Register(parts.NewAccountComponent(
				parts.DefaultAccountBuilder(&testCfg.Account), "/auth"))
			app.Register(parts.NewSwaggerComponent(
				parts.DefaultSwaggerResolver(&testCfg.Swagger)))
			app.Register(parts.NewHealthComponent("/healthz"))
			app.Register(parts.NewMetricsComponent("/metrics"))
			return nil
		}),
		chok.WithRoutes(blogRoutes()),
	)

	ready := make(chan struct{})
	a.On(component.EventAfterStart, func(ctx context.Context) error {
		close(ready)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	select {
	case <-ready:
	case err := <-done:
		cancel()
		panic(fmt.Sprintf("blog test setup failed: %v", err))
	}
	cancel()
	<-done
	return httpComp.Engine()
}
