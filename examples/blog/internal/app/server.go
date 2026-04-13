package app

import (
	"context"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/account"
	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/config"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/examples/blog/internal/handler"
	"github.com/zynthara/chok/examples/blog/internal/model"
	blogStore "github.com/zynthara/chok/examples/blog/internal/store"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/middleware"
	"github.com/zynthara/chok/server"
	chokstore "github.com/zynthara/chok/store"
	"github.com/zynthara/chok/swagger"
)

var cfg Config

// NewApp creates the blog application.
func NewApp() *chok.App {
	return chok.New("blog",
		chok.WithConfig(&cfg),
		chok.WithSetup(func(ctx context.Context, a *chok.App) error {
			return setup(ctx, a, &cfg)
		}),
	)
}

// NewTestRouter returns a gin.Engine wired with in-memory SQLite for testing.
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

	r := gin.New()
	if err := wireRoutes(gdb, log.Empty(), testCfg, r); err != nil {
		panic(err)
	}
	return r
}

func setup(_ context.Context, a *chok.App, c *Config) error {
	gdb, err := db.NewSQLite(&c.SQLite)
	if err != nil {
		return err
	}
	a.SetDB(gdb)
	a.AddCleanup(func(_ context.Context) error { return db.Close(gdb) })

	srv := server.NewHTTPServer(&c.HTTP)
	srv.Use(
		middleware.Recovery(),
		middleware.RequestID(),
		middleware.Logger(a.Logger()),
	)

	if err := wireRoutes(gdb, a.Logger(), c, srv.Engine()); err != nil {
		return err
	}

	a.AddServer(srv)
	return nil
}

// wireRoutes sets up migrations, account, and routes on the given gin engine.
func wireRoutes(gdb *gorm.DB, logger log.Logger, c *Config, r *gin.Engine) error {
	// --- Migrate ---

	if err := db.Migrate(gdb,
		account.Table(),
		db.Table(&model.Post{}, db.SoftUnique("uk_post_title_owner", "title", "owner_id")),
	); err != nil {
		return err
	}

	// --- Error mapper ---

	apierr.RegisterMapper(chokstore.MapError)

	// --- Account ---

	acct, err := account.Setup(gdb, logger, &c.Account, r.Group("/auth"))
	if err != nil {
		return err
	}

	// --- Protected API ---

	api := r.Group("/api/v1")
	if acct != nil {
		api.Use(middleware.Authn(acct.TokenParser(), acct.PrincipalResolver()))
	}

	postStore := blogStore.NewPostStore(
		chokstore.New[model.Post](gdb, logger),
	)
	handler.RegisterPostRoutes(api, postStore)

	// --- Swagger (auto-generated from registered routes) ---

	swagger.Generate(&c.Swagger, r)

	return nil
}
