<p align="center">
  <a href="README_zh.md">中文</a> | English
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.27+" />
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green" alt="MIT" /></a>
  <a href="https://pkg.go.dev/github.com/zynthara/chok/v3"><img src="https://pkg.go.dev/badge/github.com/zynthara/chok/v3.svg" alt="Go Reference" /></a>
  <a href="https://github.com/zynthara/chok/actions/workflows/ci.yml"><img src="https://github.com/zynthara/chok/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
</p>

<h1 align="center"><code>chok</code></h1>
<p align="center"><b>An opinionated Go application toolkit, assembled with plain Go code.</b></p>

---

> [!NOTE]
> This is **chok v3** (`github.com/zynthara/chok/v3`), released as
> `v3.0.0-alpha.N`: the API may still change before v3.0.0. v2 is sealed
> on the [`v2` branch](https://github.com/zynthara/chok/tree/v2) (security
> fixes only; `go get github.com/zynthara/chok/v2@v2.0.0-beta.6` keeps
> working). Moving from v2? Read the
> [migration guide](docs/migration-v2-to-v3.md).

---

chok gives a Go service one strict typed configuration, typed HTTP handlers
that publish their own OpenAPI document, and a gorm-backed store that fences
every query to the signed-in user — an authenticated CRUD resource takes four
chok imports. Identity, jobs, telemetry, cache, realtime and mail are
batteries: values you construct and hand to `app.Use`. There is no kernel, no registry and no
generated file; `main.go` is the whole assembly:

```go
package main

import (
	"context"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/store"
)

type Config struct {
	chok.Base                    // the app, http and log sections
	DB   db.Config       `yaml:"db"`
	Auth identity.Config `yaml:"auth"`
}

// Note belongs to whoever creates it: the store stamps the owner and
// fences every query to the signed-in user.
type Note struct {
	db.OwnedModel
	Text string `json:"text" store:"query,update" gorm:"size:500;not null"`
}

type newNote struct {
	Text string `json:"text" validate:"required,max=500"`
}

func main() {
	cfg := chok.MustLoad[Config]("notes") // chok.yaml, then NOTES_* variables
	app := chok.New(cfg.Base)
	d := db.MustOpen(cfg.DB, db.Tables(db.Table(&Note{})))
	auth := identity.MustNew(cfg.Auth, d) // /auth/register, /auth/login, ...
	app.Use(d, auth)

	notes := store.New[Note](d)
	api := app.Group("/api", auth.Authn())
	chok.POST(api, "/notes", func(ctx context.Context, req *newNote) (*Note, error) {
		n := &Note{Text: req.Text}
		return n, notes.Create(ctx, n)
	}, chok.Created())
	chok.List(api, "/notes", notes) // ?page=&size=&order=text:desc&text=
	app.Execute()
}
```

```yaml
# chok.yaml — a misspelled key fails startup with a suggestion
db:
  driver: sqlite
  sqlite:
    path: notes.db
auth:
  token_ttl: 2h # signing_key comes from NOTES_AUTH_SIGNING_KEY
```

That serves JSON with request IDs and an access log, `/healthz`, `/readyz`
and `/livez`, the OpenAPI 3.1 document at `/openapi.json` with its UI at
`/docs`, registration and login with rate limiting, 401 / 404 / 409 / 400
error envelopes, and a graceful stop on Ctrl-C. SQLite, MySQL and
Postgres are equally supported; the tables are created on start, or by
versioned SQL migrations.

## 30-second hello

```sh
go run github.com/zynthara/chok/v3/cmd/chok@latest init hello
cd hello && go mod tidy && go run .
```

In a second terminal:

```sh
curl -s localhost:8080/healthz
# {"status":"up","components":[{"component":"db","status":"up","duration_ms":0}]}
curl -s -X POST localhost:8080/api/v1/notes -H 'Content-Type: application/json' -d '{"title":"hello"}'
# {"id":"note_…","version":1,"created_at":"…","updated_at":"…","title":"hello","body":""}
curl -s localhost:8080/api/v1/notes
# {"items":[…],"total":1,"page":1,"size":20,"has_more":false}
```

`chok init` writes a 76-line `main.go` (one resource, four routes),
`chok.yaml`, a `Makefile`, `migrations/` and a `go.mod` that requires chok
and names the command in a `tool` directive, so `go tool chok` runs the
version the project uses. Open <http://localhost:8080/docs> for the API.

## Five-minute blog

[`examples/blog`](examples/blog) is a JWT-guarded blog in under a hundred
lines: register, log in, write posts only you can see, update them under
optimistic locking, delete them, read `/metrics`.

```sh
git clone https://github.com/zynthara/chok && cd chok/examples/blog
export BLOG_AUTH_SIGNING_KEY=$(openssl rand -hex 32)
go run .
```

```sh
curl -s -X POST localhost:8080/auth/register -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"password123","name":"Alice"}'
TOKEN=$(curl -s -X POST localhost:8080/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"password123"}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
curl -s -X POST localhost:8080/api/v1/posts -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"title":"Hello chok","content":"my first post"}'
# {"id":"pst_…","version":1,…,"title":"Hello chok","content":"my first post","status":"draft"}
curl -s "localhost:8080/api/v1/posts?status=draft" -H "Authorization: Bearer $TOKEN"
```

Its [README](examples/blog/README.md) walks all nine steps — another user's
404, the 409 of a stale update, the delete, the OpenAPI document and the
metrics — and its acceptance test walks the same path in CI. For a guided
build-up from an empty directory, read the [quickstart](docs/quickstart.md).

## Realtime

The `realtime` battery pushes events to browsers as they happen — an
updated order, a chat message, a finished background job — over a
WebSocket or server-sent events, from the same topics. The page trades
its token for a one-time ticket, so no long-lived token travels in a
URL; `Authorize` decides who follows what; a reconnecting page resumes
where it left off; with Redis, every replica shares the events.

```go
package main

import (
	"context"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/realtime"
)

type Config struct {
	chok.Base
	Realtime realtime.Config `yaml:"realtime"`
}

type story struct {
	Title string `json:"title" validate:"required,max=200"`
}

func main() {
	cfg := chok.MustLoad[Config]("news")
	app := chok.New(cfg.Base)
	// Anonymous tickets (no realtime.Authn), and anyone may follow "news".
	hub := realtime.MustNew(cfg.Realtime, realtime.Authorize(func(_ context.Context, topic string) error {
		if topic == "news" {
			return nil
		}
		return chok.ErrForbidden.Msgf("no topic %s", topic)
	}))
	app.Use(hub) // POST /realtime/ticket, GET /realtime/ws, GET /realtime/sse

	chok.POST(app.Router(), "/stories", func(ctx context.Context, s *story) (chok.None, error) {
		return chok.None{}, hub.Publish(ctx, "news", "story.posted", s)
	})
	app.Execute()
}
```

```js
// In the page: a one-time ticket, then the stream.
const {ticket} = await (await fetch("/realtime/ticket", {method: "POST"})).json();
const news = new EventSource(`/realtime/sse?topics=news&ticket=${ticket}`);
news.addEventListener("story.posted", (e) => console.log(JSON.parse(e.data).data.title));
```

Reconnecting and resuming, the WebSocket protocol, several replicas and
live background jobs: [docs/realtime.md](docs/realtime.md).
[`examples/chatter`](examples/chatter) is a chat with a bot and a jobs
panel built on it.

## Mail and passwordless sign-in

The `mail` battery sends transactional mail with the standard library
alone — through an SMTP relay, into `.eml` files during development, or
into the log — from `html/template` and `text/template` files. Given a
way to mail, `identity` signs people in with a code instead of a
password: `POST /auth/passwordless/start {"email"}` answers 202 and mails
a code and a one-click link; `POST /auth/passwordless/verify` with the
code (or the link's token) signs in, creating the account on first use.
In cookie mode the session is an HttpOnly cookie the page's scripts never
see, guarded by `http.csrf`.

```go
package main

import (
	"context"
	"os"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/mail"
)

type Config struct {
	chok.Base
	DB   db.Config       `yaml:"db"`
	Auth identity.Config `yaml:"auth"`
	Mail mail.Config     `yaml:"mail"`
}

// sender is identity's MailSender: a mail is the template named after
// its kind, templates/login-code.html or templates/password-reset.html.
type sender struct{ m *mail.Mailer }

func (s sender) Send(ctx context.Context, m identity.Mail) error {
	return s.m.Send(ctx, mail.Message{To: []string{m.To}, Template: m.Kind, Data: m})
}

func main() {
	cfg := chok.MustLoad[Config]("shop")
	app := chok.New(cfg.Base)
	d := db.MustOpen(cfg.DB)
	m := mail.MustNew(cfg.Mail, mail.Templates(os.DirFS("templates")))
	auth := identity.MustNew(cfg.Auth, d, identity.Sender(sender{m}))
	app.Use(d, auth) // + POST /auth/passwordless/start, /auth/passwordless/verify, /auth/logout
	app.Execute()
}
```

```yaml
# chok.yaml; the signing key comes from SHOP_AUTH_SIGNING_KEY
http:
  csrf: true   # the session cookie goes with every request
db:
  driver: sqlite
  sqlite:
    path: shop.db
auth:
  passwordless:
    enabled: true
  cookie:
    enabled: true
mail:
  from: Shop <no-reply@shop.example>
  smtp:
    host: smtp.example.com   # the password comes from SHOP_MAIL_SMTP_PASSWORD
    username: apikey
```

Templates, attachments, the three transports, retries through a jobs
worker, the page's side of the sign-in and the cookie:
[docs/mail.md](docs/mail.md). [`examples/chatter`](examples/chatter) signs
in this way.

## Packages

| Package | What it is |
|---|---|
| [`chok`](https://pkg.go.dev/github.com/zynthara/chok/v3) | Configuration loading, the App and its lifecycle, typed handlers and routing, binding and validation, error envelopes, default middleware, OpenAPI, health probes, slog |
| [`db`](https://pkg.go.dev/github.com/zynthara/chok/v3/db) | SQLite, MySQL and Postgres handles, transactions, model bases, RIDs, soft-unique indexes, auto / versioned / off migrations on one ledger |
| [`store`](https://pkg.go.dev/github.com/zynthara/chok/v3/store) | Generic `Store[T]`: CRUD, owner scoping that fails closed, patches with optimistic locking, cursors, aggregates |
| [`store/where`](https://pkg.go.dev/github.com/zynthara/chok/v3/store/where) | The query DSL: `Eq`, `In`, `Like`, `Order`, `Page`, `Cursor`, `FromQuery`, … |
| [`identity`](https://pkg.go.dev/github.com/zynthara/chok/v3/identity) | Accounts, JWTs, login rate limiting, OAuth, passwordless sign-in by mailed code, cookie sessions, the audit log, `Authn()` |
| [`identity/rbac`](https://pkg.go.dev/github.com/zynthara/chok/v3/identity/rbac) | casbin role-based access control: `Require`, policies, a Redis watcher |
| [`identity/google`](https://pkg.go.dev/github.com/zynthara/chok/v3/identity/google), [`github`](https://pkg.go.dev/github.com/zynthara/chok/v3/identity/github), [`apple`](https://pkg.go.dev/github.com/zynthara/chok/v3/identity/apple), [`facebook`](https://pkg.go.dev/github.com/zynthara/chok/v3/identity/facebook) | The OAuth providers |
| [`jobs`](https://pkg.go.dev/github.com/zynthara/chok/v3/jobs) | Cron-scheduled periodic jobs and transactional-outbox workers (at-least-once) |
| [`telemetry`](https://pkg.go.dev/github.com/zynthara/chok/v3/telemetry) | Prometheus metrics, OpenTelemetry traces, slog to OpenTelemetry logs, SQL instrumentation |
| [`cache`](https://pkg.go.dev/github.com/zynthara/chok/v3/cache) | An in-memory and a Redis layer behind one `Config`, with a circuit breaker |
| [`realtime`](https://pkg.go.dev/github.com/zynthara/chok/v3/realtime) | Events pushed to browsers over WebSocket and server-sent events: one-time tickets, authorized topics, resuming after a reconnect, a Redis backplane |
| [`mail`](https://pkg.go.dev/github.com/zynthara/chok/v3/mail) | Transactional mail through an SMTP relay, `.eml` files or the log: templates, attachments, inline images; identity's sign-in codes |
| [`choktest`](https://pkg.go.dev/github.com/zynthara/chok/v3/choktest) | Test helpers: a throwaway database per test on the lane's dialect, an in-process App client |

Every exported symbol, one line each: [docs/api.md](docs/api.md).

## Configuration

One struct, loaded by `chok.Load[T]`: defaults, then `chok.yaml`, then
environment variables (`http.addr` is `<NAME>_HTTP_ADDR`), then flags.
Unknown keys fail startup with a suggestion; secrets are masked;
`--check-config` validates the configuration and prints it without
starting anything; only `log.level` reloads, on SIGHUP. Every key, with its
type, default and rules: [docs/config.md](docs/config.md).

## Command line

```sh
go install github.com/zynthara/chok/v3/cmd/chok@latest   # or go tool chok in a project
```

| Command | What it does |
|---|---|
| `chok init <name>` | A project that serves on `go run .` |
| `chok migrate create <name>` | The next versioned migration's empty SQL files, one per dialect |
| `chok migrate up` / `status` / `mark-applied <version>` | Apply, list or record migrations on the `chok_migrations` ledger, reading the app's own `db` configuration |
| `chok openapi export <url> [--out file]` | Save a running app's OpenAPI document as JSON or YAML |
| `chok version` | The chok version, revision and toolchain the command was built from |

## Design in three sentences

Configuration is strict: one typed struct, unknown keys fail startup, and
only the log level reloads. Errors describe themselves: an error that
implements `chok.Coder` becomes the HTTP envelope, with no mapper registry.
Assembly is code: dependencies are values you pass to constructors, and
`app.Use` only registers lifecycle.

More in [docs/design.md](docs/design.md) (Chinese).

## Documentation

| Topic | Where |
|---|---|
| Quickstart: from an empty directory to an authenticated resource | [docs/quickstart.md](docs/quickstart.md) |
| Realtime: events in the browser, five minutes | [docs/realtime.md](docs/realtime.md) |
| Mail, passwordless sign-in and cookie sessions | [docs/mail.md](docs/mail.md) |
| Configuration reference (generated) | [docs/config.md](docs/config.md) |
| API map (generated) | [docs/api.md](docs/api.md) |
| Design (Chinese) | [docs/design.md](docs/design.md) |
| Migrating from v2 (Chinese) | [docs/migration-v2-to-v3.md](docs/migration-v2-to-v3.md) |
| Changes by release | [CHANGELOG.md](CHANGELOG.md) |
| GoDoc | <https://pkg.go.dev/github.com/zynthara/chok/v3> |
| Guidance for coding agents | [CLAUDE.md](CLAUDE.md), [AGENTS.md](AGENTS.md) |

## Contributing

`make verify` runs every gate CI runs, in order: tidy, gofmt, vet,
golangci-lint, `go fix`, the test suite on SQLite, Postgres and MySQL, the
API budget and a smoke test of `examples/blog`. The database lanes read
`CHOK_TEST_PG_DSN` and `CHOK_TEST_MYSQL_DSN`; [CLAUDE.md](CLAUDE.md) has the
conventions. Security reports: [SECURITY.md](SECURITY.md).

## License

Released under the [MIT License](LICENSE). The cron parser in
`jobs/internal/cron` is adapted from robfig/cron (MIT); see [NOTICE](NOTICE).
