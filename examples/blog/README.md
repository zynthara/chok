# blog — the chok v3 quickstart

A JWT-guarded blog API — register, log in, own your posts — in about a
hundred lines of application code. `main.go` assembles it with plain Go:
open the database, construct the identity battery, register both with
the app, declare five routes. Nothing is generated.

## The five-minute path

Requires Go 1.27. From the repository root:

```sh
cd examples/blog
export BLOG_AUTH_SIGNING_KEY=$(openssl rand -hex 32)   # ≥ 32 bytes, never in chok.yaml
go run .
```

The app logs the address it listens on (`:8080` by default). In a
second terminal:

```sh
# 1. Health — every component reports in:
curl -s localhost:8080/healthz
# {"status":"up","components":[{"component":"db","status":"up",...},...]}

# 2. Register, then log in (the identity battery mounts /auth/*):
curl -s -X POST localhost:8080/auth/register -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"password123","name":"Alice"}'
TOKEN=$(curl -s -X POST localhost:8080/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"password123"}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')

# 3. Create a post (401 without the token — try it):
curl -s -X POST localhost:8080/api/v1/posts -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"title":"Hello chok","content":"my first post"}'
# {"id":"pst_...","version":1,"title":"Hello chok","status":"draft",...}
RID=pst_...   # the id from the response

# 4. List your posts — owner-scoped, filterable by the query fields:
curl -s "localhost:8080/api/v1/posts?status=draft&order=created_at:desc" -H "Authorization: Bearer $TOKEN"

# 5. Read one:
curl -s localhost:8080/api/v1/posts/$RID -H "Authorization: Bearer $TOKEN"

# 6. Publish it, asserting the version you saw; send it again and the
#    now-stale version is a 409 Conflict:
curl -s -X PATCH localhost:8080/api/v1/posts/$RID -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"version":1,"status":"published"}'

# 7. Delete it (204), then reading it is a 404:
curl -s -X DELETE localhost:8080/api/v1/posts/$RID -H "Authorization: Bearer $TOKEN" -w '%{http_code}\n'

# 8. The OpenAPI 3.1 document the route table produced (UI at /docs):
curl -s localhost:8080/openapi.json
```

Stop the server with Ctrl-C: `/readyz` turns 503, in-flight requests
drain, components close in reverse order and the process exits 0.
`go run . --check-config` validates the configuration and prints it with
secrets masked, without starting anything.

## What to look at

| File | Lesson |
|---|---|
| `chok.yaml` | One strict config. A typo such as `htpp:` fails startup with "did you mean http?"; every key is `BLOG_*` overridable. |
| `main.go` | `Config` embeds `chok.Base` and adds a section per battery; `setup` is the whole assembly — values passed to constructors, lifecycle registered with `app.Use`. |
| `posts.go` | Typed handlers: request structs bind `path` / `json` and validate; `store.Patch` turns a partial update into a locked change set. |
| `blog_test.go` | The acceptance test that walks this path in process with `choktest.App`. |

Without writing them you get: owner isolation (`Post` embeds
`db.OwnedSoftDeleteModel`), 404 / 409 / 401 error envelopes (store and
identity errors describe themselves), request IDs on every log line,
`/readyz` and `/livez`, login rate limiting and the OpenAPI document.
