// Command chatter is chok's realtime example: chat rooms that update
// without a reload, a bot that answers through a background worker, and
// a live panel of the background jobs — identity, store, jobs and the
// realtime hub in one app.
//
//	cd examples/chatter
//	CHATTER_AUTH_SIGNING_KEY=$(openssl rand -hex 32) go run .
//
// Then open http://localhost:8080 in two browsers. README.md next to this
// file walks through it, browser and curl.
package main

import (
	_ "embed"
	"net/http"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/jobs"
	"github.com/zynthara/chok/v3/realtime"
	"github.com/zynthara/chok/v3/store"
)

// Config is the whole configuration: chok's own sections and one per
// battery. A misspelled key in chok.yaml or a CHATTER_* variable fails
// startup with a suggestion.
type Config struct {
	chok.Base
	DB       db.Config       `yaml:"db"`
	Auth     identity.Config `yaml:"auth"`
	Jobs     jobs.Config     `yaml:"jobs"`
	Realtime realtime.Config `yaml:"realtime"`
}

func main() {
	cfg := chok.MustLoad[Config]("chatter")
	app := chok.New(cfg.Base)
	setup(app, cfg, db.MustOpen(cfg.DB, db.Tables(tables()...)))
	app.Execute()
}

// page is the whole front end: plain HTML and JavaScript, no build step.
//
//go:embed index.html
var page []byte

// setup assembles the app on d; chatter_test.go reuses it.
func setup(app *chok.App, cfg Config, d *db.DB) {
	auth := identity.MustNew(cfg.Auth, d) // /auth/register, /auth/login, ...
	c := &chat{d: d, rooms: store.New[Room](d), members: store.New[Member](d), messages: store.New[Message](d)}
	// The hub: a ticket from /realtime/ticket carries the signed-in user,
	// and mayWatch decides which topics that user may follow.
	c.hub = realtime.MustNew(cfg.Realtime, realtime.Authn(auth.Authn()), realtime.Authorize(c.mayWatch))
	// The jobs: the bot's worker, a periodic job, and every run of both
	// handed to the hub for the jobs panel. The batteries do not know each
	// other; this closure is where they meet.
	c.jobs = jobs.MustNew(cfg.Jobs, d,
		jobs.Worker("bot.reply", c.botReply),
		jobs.Periodic("stats", "@every 30s", c.stats, jobs.OverlapSkip),
		jobs.Observe(c.observe))
	// Close runs in reverse: the hub says goodbye to the browsers first.
	app.Use(d, auth, c.jobs, c.hub)

	api := app.Group("/api", auth.Authn())
	chok.POST(api, "/rooms", c.createRoom, chok.Created(), chok.Summary("Create a room"))
	chok.List(api, "/rooms", c.rooms, chok.Summary("List the rooms"))
	chok.POST(api, "/rooms/{id}/join", c.join, chok.Summary("Join a room"))
	chok.GET(api, "/rooms/{id}/messages", c.history, chok.Summary("List a room's last messages"))
	chok.POST(api, "/rooms/{id}/messages", c.post, chok.Created(), chok.Summary("Post a message"))
	app.Router().Handle(http.MethodGet, "/{$}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	}))
}
