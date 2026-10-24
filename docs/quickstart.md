# Quickstart

chok v3 is a Go toolkit you import and wire with plain code: no generated
files, no registry. This page goes from an empty directory to an
authenticated, owner-scoped CRUD resource in three steps. Requires Go
1.27. Every Go program below is complete — paste it over `main.go` — and
is compiled by the repository's tests, so it builds as printed.

To start from a generated project instead, `chok init` writes one with a
resource, a Makefile and migrations (see the [README](../README.md)).

## 1. A first endpoint

```sh
mkdir hello && cd hello
go mod init example.com/hello
go get github.com/zynthara/chok/v3
```

`main.go`:

```go
package main

import (
	"context"

	"github.com/zynthara/chok/v3"
)

// greeting is the request: name comes from the query string and is
// required.
type greeting struct {
	Name string `query:"name" validate:"required"`
}

func main() {
	app := chok.New(chok.MustLoad[chok.Base]("hello"))
	chok.GET(app.Router(), "/hello", func(_ context.Context, req *greeting) (map[string]string, error) {
		return map[string]string{"hello": req.Name}, nil
	}, chok.Public())
	app.Execute()
}
```

`go run .` serves `:8080`:

```sh
curl 'localhost:8080/hello?name=chok'
# {"hello":"chok"}
curl localhost:8080/hello
# {"code":400,"reason":"Validation","message":"validation failed: name is required","request_id":"…","metadata":{"fields":{"name":"required"}}}
```

You also get `/healthz`, `/readyz` and `/livez`, the OpenAPI 3.1 document
at `/openapi.json` with its UI at `/docs`, a JSON access log line per
request, and a graceful stop on Ctrl-C. Settings come from `chok.yaml`
when there is one, then from `HELLO_*` variables (`HELLO_HTTP_ADDR=:9090`);
`go run . --check-config` prints what is in effect. A misspelled key fails
startup with a suggestion.

## 2. Add login

Give the app a configuration type with a section per battery, open a
database and let `identity` mount `/auth/*`. `main.go`:

```go
package main

import (
	"context"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity"
)

// Config is chok.yaml: chok.Base brings the app, http and log sections,
// and each battery gets a section of its own.
type Config struct {
	chok.Base
	DB   db.Config       `yaml:"db"`
	Auth identity.Config `yaml:"auth"`
}

// whoami is what /api/whoami answers: who the bearer token belongs to.
type whoami struct {
	UserID string   `json:"user_id"`
	Email  string   `json:"email"`
	Roles  []string `json:"roles"`
}

func main() {
	cfg := chok.MustLoad[Config]("hello")
	app := chok.New(cfg.Base)
	d := db.MustOpen(cfg.DB)
	auth := identity.MustNew(cfg.Auth, d) // creates its tables, mounts /auth/*
	app.Use(d, auth)                      // start, health and close follow the Use order

	api := app.Group("/api", auth.Authn()) // a valid bearer token, or 401
	chok.GET(api, "/whoami", func(ctx context.Context, _ *chok.None) (*whoami, error) {
		p, _ := chok.PrincipalFrom(ctx) // the user Authn found
		return &whoami{UserID: p.UserID, Email: p.Email, Roles: p.Roles}, nil
	})
	app.Execute()
}
```

`chok.yaml`:

```yaml
db:
  driver: sqlite
  sqlite:
    path: hello.db
auth:
  token_ttl: 2h # signing_key is a secret: it comes from HELLO_AUTH_SIGNING_KEY
```

```sh
go mod tidy
HELLO_AUTH_SIGNING_KEY=$(openssl rand -hex 32) go run .
```

In a second terminal:

```sh
curl -s -X POST localhost:8080/auth/register -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"password123","name":"Ada"}'
# 201 {"token":"eyJ…","expires_at":"…"}
TOKEN=$(curl -s -X POST localhost:8080/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"password123"}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')   # 200, same body
curl -s localhost:8080/api/whoami -H "Authorization: Bearer $TOKEN"
# {"user_id":"usr_…","email":"ada@example.com","roles":[]}
curl -s localhost:8080/api/whoami
# {"code":401,"reason":"Unauthorized","message":"a bearer token is required","request_id":"…"}
```

`identity` also serves `GET /auth/me` (the account: `id`, `email`, `name`,
`email_verified`, `roles`, `created_at`), `POST /auth/refresh-token` and
`PUT /auth/change-password`; forgot and reset password appear once you
pass `identity.Sender(...)`. Tokens last `auth.token_ttl` (2h), a session
`auth.refresh_ttl` (168h) from its login, and ten failed logins in 15
minutes, per email or per address, answer 429 — all in
[docs/config.md](config.md#identityconfig--section-auth).

## 3. Add a resource

A model embeds a base from `db`; `store:"…"` tags name the fields the API
may filter on (`query`) and change (`update`). Declare the table, build a
store, add routes. `main.go`, with the `/whoami` route of step 2 left out
for brevity:

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
	chok.Base
	DB   db.Config       `yaml:"db"`
	Auth identity.Config `yaml:"auth"`
}

// Note embeds an owned base: the store stamps the signed-in user as its
// owner and fences every query to that user's rows.
type Note struct {
	db.OwnedModel
	Text string `json:"text" store:"query,update" gorm:"size:500;not null"`
}

// RIDPrefix makes the public ids read nte_….
func (Note) RIDPrefix() string { return "nte" }

type newNote struct {
	Text string `json:"text" validate:"required,max=500"`
}

type noteID struct {
	ID string `path:"id" validate:"required"`
}

func main() {
	cfg := chok.MustLoad[Config]("hello")
	app := chok.New(cfg.Base)
	d := db.MustOpen(cfg.DB, db.Tables(db.Table(&Note{})))
	auth := identity.MustNew(cfg.Auth, d)
	app.Use(d, auth)

	notes := store.New[Note](d)
	api := app.Group("/api", auth.Authn())
	chok.POST(api, "/notes", func(ctx context.Context, req *newNote) (*Note, error) {
		n := &Note{Text: req.Text}
		return n, notes.Create(ctx, n)
	}, chok.Created())
	chok.List(api, "/notes", notes) // ?page=&size=&order=text:asc&text=
	chok.GET(api, "/notes/{id}", func(ctx context.Context, req *noteID) (*Note, error) {
		return notes.Get(ctx, store.RID(req.ID))
	})
	app.Execute()
}
```

The `notes` table is created on start (`db.migrate: auto`, the default;
`versioned` applies SQL files instead — see `chok migrate`). With the
`TOKEN` of step 2:

```sh
curl -s -X POST localhost:8080/api/notes -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"text":"first"}'
# 201 {"id":"nte_…","version":1,"created_at":"…","updated_at":"…","text":"first"}
curl -s 'localhost:8080/api/notes?order=created_at:desc' -H "Authorization: Bearer $TOKEN"
# {"items":[…],"total":1,"page":1,"size":20,"has_more":false}
```

Another user's notes do not exist for you: reading one is a 404, as is an
unknown id; an unknown query parameter is a 400 naming the allowed
fields. The full version of this app — updates with optimistic locking
(409 on a stale version), soft delete, a unique title per author and
metrics — is [`examples/blog`](../examples/blog).

## Next

- Every configuration key: [docs/config.md](config.md). Every exported
  symbol: [docs/api.md](api.md).
- How the pieces fit — lifecycle, errors, the store, the batteries:
  [docs/design.md](design.md) (in Chinese).
- Coming from chok v2: [docs/migration-v2-to-v3.md](migration-v2-to-v3.md)
  (in Chinese). Request bodies are decoded with `encoding/json/v2`: field
  names match case-sensitively, unknown and duplicate fields are a 400,
  and a nil slice is written as `[]`.
