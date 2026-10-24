# blog — the chok v3 quickstart

> **Interim authentication (认证为 WP6 前的过渡方案).** The identity
> battery — register, login, JWT — arrives in WP6. Until then
> `devauthn.go` stands in for it: the `X-User-Id` header names the
> caller, so anyone can claim any id. It exists to exercise the owner
> fence and is never meant to ship.

A blog API where every post belongs to the user who wrote it, in under a
hundred lines of application code. `main.go` assembles it with plain Go:
open the database, register it with the app, declare five routes.
Nothing is generated.

## The five-minute path

Requires Go 1.27. From the repository root:

```sh
cd examples/blog
go run .
```

The app logs the address it listens on (`:8080` by default; set
`BLOG_HTTP_ADDR` to move it). In a second terminal:

```sh
# 1. Health — every component reports in:
curl -s localhost:8080/healthz
# {"status":"up","components":[{"component":"db","status":"up",...}]}

# 2. Without a user, a write is a 401 envelope and reads find nothing:
curl -s -X POST localhost:8080/api/v1/posts -H 'Content-Type: application/json' \
  -d '{"title":"Hello chok","content":"my first post"}'
# {"code":401,"reason":"Unauthorized","message":"unauthorized","request_id":"..."}

# 3. Create a post as alice:
curl -s -X POST localhost:8080/api/v1/posts -H 'X-User-Id: alice' \
  -H 'Content-Type: application/json' -d '{"title":"Hello chok","content":"my first post"}'
# {"id":"pst_...","version":1,...,"title":"Hello chok","content":"my first post","status":"draft"}
RID=pst_...   # the id from the response

# 4. List your posts — owner-scoped, filterable by the query fields:
curl -s "localhost:8080/api/v1/posts?status=draft&order=created_at:desc" -H 'X-User-Id: alice'
# {"items":[...],"total":1,"page":1,"size":20,"has_more":false}

# 5. Read one — bob gets a 404, as if it did not exist:
curl -s localhost:8080/api/v1/posts/$RID -H 'X-User-Id: alice'
curl -s localhost:8080/api/v1/posts/$RID -H 'X-User-Id: bob'

# 6. Publish it, asserting the version you saw; send it again and the
#    now-stale version is a 409 Conflict:
curl -s -X PATCH localhost:8080/api/v1/posts/$RID -H 'X-User-Id: alice' \
  -H 'Content-Type: application/json' -d '{"version":1,"status":"published"}'
curl -s -X PATCH localhost:8080/api/v1/posts/$RID -H 'X-User-Id: alice' \
  -H 'Content-Type: application/json' -d '{"version":1,"status":"published"}'

# 7. Delete it (204), then reading it is a 404:
curl -s -X DELETE localhost:8080/api/v1/posts/$RID -H 'X-User-Id: alice' -w '%{http_code}\n'
curl -s localhost:8080/api/v1/posts/$RID -H 'X-User-Id: alice'

# 8. The OpenAPI 3.1 document the route table produced (UI at /docs):
curl -s localhost:8080/openapi.json
```

Stop the server with Ctrl-C: `/readyz` turns 503, in-flight requests
drain, components close in reverse order and the process exits 0.
`go run . --check-config` validates the configuration and prints it,
without starting anything.

## What to look at

| File | Lesson |
|---|---|
| `chok.yaml` | One strict config. A typo such as `htpp:` fails startup with "did you mean http?"; every key is `BLOG_*` overridable. |
| `main.go` | `Config` embeds `chok.Base` and adds a section per battery; `setup` is the whole assembly — values passed to constructors, lifecycle registered with `app.Use`. |
| `posts.go` | Typed handlers: request structs bind `path` / `json` and validate; `store.Patch` turns a partial update into a locked change set. |
| `devauthn.go` | The interim stand-in for login; WP6 replaces it with `identity`'s `Authn`. |
| `blog_test.go` | The acceptance test that walks this path in process with `choktest.App`. |

Without writing them you get: owner isolation (`Post` embeds
`db.OwnedSoftDeleteModel`), 404 / 409 / 401 error envelopes (store errors
describe themselves), request IDs on every log line, `/readyz` and
`/livez`, and the OpenAPI document.
