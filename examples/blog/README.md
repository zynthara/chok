# blog — the chok quickstart

A JWT-guarded blog API — register, log in, own your posts — in under a
hundred lines of application code. `main.go` assembles it with plain Go:
open the database, construct the identity and telemetry batteries,
register them with the app, declare five routes. Nothing is generated.

## The five-minute path

Requires Go 1.27. From the repository root:

```sh
cd examples/blog
export BLOG_AUTH_SIGNING_KEY=$(openssl rand -hex 32)   # ≥ 32 bytes, never in chok.yaml
go run .
```

The app logs the address it listens on (`:8080` by default; set
`BLOG_HTTP_ADDR` to move it). In a second terminal:

```sh
# 1. Health — every component reports in:
curl -s localhost:8080/healthz
# {"status":"up","components":[{"component":"db","status":"up",...},{"component":"identity",...},{"component":"telemetry",...}]}

# 2. Register, then log in (the identity battery mounts /auth/*). Without
#    the token every /api route is a 401 envelope — try it:
curl -s -X POST localhost:8080/auth/register -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"password123","name":"Alice"}'
TOKEN=$(curl -s -X POST localhost:8080/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"password123"}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
curl -s localhost:8080/api/v1/posts
# {"code":401,"reason":"Unauthorized","message":"a bearer token is required","request_id":"..."}

# 3. Create a post as alice:
curl -s -X POST localhost:8080/api/v1/posts -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"title":"Hello chok","content":"my first post"}'
# {"id":"pst_...","version":1,...,"title":"Hello chok","content":"my first post","status":"draft"}
RID=pst_...   # the id from the response

# 4. List your posts — owner-scoped, filterable by the query fields:
curl -s "localhost:8080/api/v1/posts?status=draft&order=created_at:desc" -H "Authorization: Bearer $TOKEN"
# {"items":[...],"total":1,"page":1,"size":20,"has_more":false}

# 5. Read one — bob, with a token of his own, gets a 404, as if it did
#    not exist:
curl -s localhost:8080/api/v1/posts/$RID -H "Authorization: Bearer $TOKEN"
curl -s -X POST localhost:8080/auth/register -H 'Content-Type: application/json' \
  -d '{"email":"bob@example.com","password":"password123","name":"Bob"}'
BOB=$(curl -s -X POST localhost:8080/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"bob@example.com","password":"password123"}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
curl -s localhost:8080/api/v1/posts/$RID -H "Authorization: Bearer $BOB"

# 6. Publish it, asserting the version you saw; send it again and the
#    now-stale version is a 409 Conflict:
curl -s -X PATCH localhost:8080/api/v1/posts/$RID -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"version":1,"status":"published"}'
curl -s -X PATCH localhost:8080/api/v1/posts/$RID -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"version":1,"status":"published"}'

# 7. Delete it (204), then reading it is a 404:
curl -s -X DELETE localhost:8080/api/v1/posts/$RID -H "Authorization: Bearer $TOKEN" -w '%{http_code}\n'
curl -s localhost:8080/api/v1/posts/$RID -H "Authorization: Bearer $TOKEN"

# 8. The OpenAPI 3.1 document the route table produced (UI at /docs):
curl -s localhost:8080/openapi.json

# 9. Prometheus metrics: every request above by method, route and status,
#    every SQL statement by operation and table. They have a port of
#    their own that only this host reaches (telemetry.metrics.addr);
#    localhost:8080/metrics is a 404:
curl -s localhost:9464/metrics | grep -E '^(http_server_request|db_client_operation)_duration_seconds_count'
```

Stop the server with Ctrl-C. Since `/healthz` has been requested, it
and `/readyz` first answer 503 for `http.drain_delay` (5s by default),
the pause a load balancer needs to move traffic away, and the log says
so; a second Ctrl-C ends it. Then in-flight requests finish, components
close in reverse order and the process exits 0.
`go run . --check-config` validates the configuration and prints it with
secrets masked, without starting anything.

## What to look at

| File | Lesson |
|---|---|
| `chok.yaml` | One strict config. A typo such as `htpp:` fails startup with "did you mean http?"; every key is `BLOG_*` overridable, and the signing key only comes from the environment. |
| `main.go` | `Config` embeds `chok.Base` and adds a section per battery; `setup` is the whole assembly — values passed to constructors, `tel.Instrument(d)` adding the SQL metrics, lifecycle registered with `app.Use`, `auth.Authn()` guarding `/api/v1`. |
| `posts.go` | Typed handlers: request structs bind `path` / `json` and validate; `store.Patch` turns a partial update into a locked change set. |
| `blog_test.go` | The acceptance test that walks this path in process with `choktest.App`. |

Without writing them you get: owner isolation (`Post` embeds
`db.OwnedSoftDeleteModel`, and the owner is the token's user id), 404 /
409 / 401 error envelopes (store and identity errors describe
themselves), request IDs on every log line, `/readyz` and `/livez`,
login rate limiting, the OpenAPI document and the metrics on
`127.0.0.1:9464/metrics`. Set
`telemetry.tracing.enabled: true` (with `exporter: stdout` to watch in
the terminal) for a span per request and per SQL statement.
