# Quickstart

chok v3 is a Go toolkit you import and wire with plain code. This page
goes from an empty directory to an authenticated CRUD resource in three
steps. Requires Go 1.27.

## 1. A first endpoint

```sh
mkdir hello && cd hello
go mod init example.com/hello
go get github.com/zynthara/chok/v3
```

```go
// main.go
package main

import (
	"context"

	"github.com/zynthara/chok/v3"
)

type greeting struct {
	Name string `query:"name" validate:"required"`
}

func main() {
	app := chok.New(chok.MustLoad[chok.Base]("hello"))
	chok.GET(app.Router(), "/hello", func(ctx context.Context, req *greeting) (map[string]string, error) {
		return map[string]string{"hello": req.Name}, nil
	})
	app.Execute()
}
```

`go run .` serves `:8080`: `curl 'localhost:8080/hello?name=chok'`.
Without `name` you get a 400 envelope naming the field. You also get
`/healthz`, `/readyz`, `/livez`, `/openapi.json` and `/docs`. Settings come
from `chok.yaml` if present, then `HELLO_*` variables (`HELLO_HTTP_ADDR=:9090`).

## 2. Add login

Give the app a config type with a section per battery, open a database and
let `identity` mount `/auth/*`:

```go
type Config struct {
	chok.Base
	DB   db.Config       `yaml:"db"`
	Auth identity.Config `yaml:"auth"`
}

func main() {
	cfg := chok.MustLoad[Config]("hello")
	app := chok.New(cfg.Base)
	d := db.MustOpen(cfg.DB)
	auth := identity.MustNew(cfg.Auth, d)
	app.Use(d, auth) // start, health and close follow the Use order
	api := app.Group("/api", auth.Authn())
	chok.GET(api, "/me", func(ctx context.Context, _ *chok.None) (chok.Principal, error) {
		p, _ := chok.PrincipalFrom(ctx)
		return p, nil
	})
	app.Execute()
}
```

```yaml
# chok.yaml — the signing key comes from HELLO_AUTH_SIGNING_KEY
db: { driver: sqlite, sqlite: { path: hello.db } }
```

`POST /auth/register` and `POST /auth/login` return a token; `/api/me` without
it is a 401.

## 3. Add a resource

A model embeds a base from `db`; `store:"…"` tags say which fields the API
may filter on and update. Declare the table, build a store, add routes:

```go
type Note struct {
	db.OwnedModel // owner-scoped: users only ever see their own notes
	Text string `json:"text" store:"query,update" gorm:"size:500;not null"`
}

func (Note) RIDPrefix() string { return "nte" }

type newNote struct {
	Text string `json:"text" validate:"required,max=500"`
}

// in main, replacing db.MustOpen(cfg.DB):
d := db.MustOpen(cfg.DB, db.Tables(db.Table(&Note{})))
notes := store.New[Note](d)
chok.List(api, "/notes", notes) // ?page=&size=&order=text:asc&text=
chok.POST(api, "/notes", func(ctx context.Context, req *newNote) (*Note, error) {
	n := &Note{Text: req.Text}
	return n, notes.Create(ctx, n)
}, chok.Created())
```

The table is created on start (`db.migrate: auto`); a missing note is a 404,
a stale update a 409. The full version of this app is `examples/blog`.
