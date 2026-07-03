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

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok"
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

// NewApp creates the blog application. The framework auto-registers
// every Component from the Config struct fields — all the user provides
// is table definitions and business routes.
func NewApp() *chok.App {
	return chok.New("blog",
		chok.WithConfig(&cfg),
		chok.WithErrorMapper(chokstore.MapError),
		chok.WithTables(blogTables...),
		chok.WithRoutes(blogRoutes()),
	)
}

// NewTestRouter returns a gin.Engine wired with in-memory SQLite for
// tests. Uses explicit Component registration because the test DB
// connection must outlive the App lifecycle.
func NewTestRouter() *gin.Engine {
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

	// Ephemeral port: the delayed cancel below lets the server reach
	// its listen phase, and the zero-value Addr would mean ":http".
	httpComp := parts.NewHTTPComponent(
		func(any) *config.HTTPOptions { return &config.HTTPOptions{Addr: "127.0.0.1:0"} },
	).WithoutAccessLog()

	a := chok.New("blog-test",
		chok.WithErrorMapper(chokstore.MapError),
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

	// Readiness barrier: a user AfterStart hook is NOT safe here — user
	// hooks run before the framework's internal route-mount hook, so
	// cancelling on such a signal races route mounting (the loser sees
	// "hook timeout exceeded for after_start" and requests 404). Servers
	// start strictly after every AfterStart hook completes, so a blocking
	// ServerFunc is a race-free "fully started" signal.
	started := make(chan struct{})
	a.AddServer(chok.ServerFunc(func(ctx context.Context, ready func()) error {
		close(started)
		ready()
		<-ctx.Done()
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	select {
	case <-started:
	case err := <-done:
		cancel()
		panic(fmt.Sprintf("blog test setup failed: %v", err))
	}
	cancel()
	<-done
	return httpComp.Engine()
}
