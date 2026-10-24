// Command blog is the chok v3 quickstart: a JWT-guarded blog API where
// every post belongs to the user who wrote it.
//
//	cd examples/blog
//	BLOG_AUTH_SIGNING_KEY=$(openssl rand -hex 32) go run .
//
// The walkthrough with curl commands is README.md next to this file.
package main

import (
	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/telemetry"
)

// Config is the whole configuration: the standard app / http / log
// sections plus one section per battery. A misspelled key in chok.yaml
// or in a BLOG_* variable fails startup with a suggestion.
type Config struct {
	chok.Base
	DB        db.Config        `yaml:"db"`
	Auth      identity.Config  `yaml:"auth"`
	Telemetry telemetry.Config `yaml:"telemetry"`
}

// Post is the blog's one resource. OwnedSoftDeleteModel gives it a
// prefixed public ID, optimistic locking, soft delete and an owner
// scope: every store query is fenced to the authenticated user.
type Post struct {
	db.OwnedSoftDeleteModel
	Title   string `json:"title"   store:"query,update" gorm:"size:200;not null"`
	Content string `json:"content" store:"update"       gorm:"type:text;not null"`
	Status  string `json:"status"  store:"query,update" gorm:"size:20;not null"`
}

// RIDPrefix exposes posts as pst_xxx; the numeric key stays internal.
func (Post) RIDPrefix() string { return "pst" }

func main() {
	cfg := chok.MustLoad[Config]("blog")
	app := chok.New(cfg.Base)
	setup(app, cfg)
	app.Execute()
}

// setup wires the batteries and the routes; blog_test.go reuses it.
func setup(app *chok.App, cfg Config) {
	// Request and SQL metrics on /metrics; spans once tracing is on.
	tel := telemetry.MustNew(cfg.Telemetry)
	// Titles are unique per author among live posts; deleting a post
	// frees its title.
	d := db.MustOpen(cfg.DB, db.Tables(
		db.Table(&Post{}, db.SoftUnique("uk_post_title_owner", "title", "owner_id")),
	))
	tel.Instrument(d)
	auth := identity.MustNew(cfg.Auth, d)
	// Close runs in reverse: telemetry, first in, flushes last.
	app.Use(tel, d, auth)

	posts := store.New[Post](d)
	api := app.Group("/api/v1", auth.Authn())
	chok.POST(api, "/posts", createPost(posts), chok.Created(), chok.Summary("Create a post"))
	chok.List(api, "/posts", posts, chok.Summary("List my posts"))
	chok.GET(api, "/posts/{id}", getPost(posts))
	chok.PATCH(api, "/posts/{id}", updatePost(posts))
	chok.DELETE(api, "/posts/{id}", deletePost(posts))
}
