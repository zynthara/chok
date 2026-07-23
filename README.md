<p align="center">
  <a href="README_zh.md">中文</a> | English
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.26+" />
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green" alt="MIT" /></a>
  <a href="https://pkg.go.dev/github.com/zynthara/chok/v2"><img src="https://pkg.go.dev/badge/github.com/zynthara/chok/v2.svg" alt="Go Reference" /></a>
  <a href="https://github.com/zynthara/chok/actions/workflows/ci.yml"><img src="https://github.com/zynthara/chok/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
</p>

<h1 align="center"><code>chok</code></h1>
<p align="center"><b>An opinionated, configuration-driven Go web framework.</b></p>

---

> [!NOTE]
> This is **chok v2** (`github.com/zynthara/chok/v2`), currently in
> beta. The v1 line is sealed at
> [`v0.1.4`](https://github.com/zynthara/chok/releases/tag/v0.1.4)
> (security fixes only) and stays permanently installable:
> `go get github.com/zynthara/chok@v0.1.4`. Migrating? See the
> [v1 → v2 migration guide](docs/migration-v1-to-v2.md).

---

`chok` bundles HTTP, database, cache, JWT auth, RBAC, scheduler, and
observability into a single Go module. One YAML file declares the
modules *and* configures them; the assembly code is generated.

```yaml
# chok.yaml — sections present = modules assembled; enabled = runtime switch
log:     { level: info, format: json }
http:    { addr: ":8080" }
db:      { driver: sqlite, migrate: auto, sqlite: { path: app.db } }
health:  { path: /healthz }
swagger: { title: "My API" }
account: { signing_key: "${MYAPP_ACCOUNT_SIGNING_KEY}" }
```

```go
// main.go — the whole wiring
func main() {
    chok.New("myapp",
        chokModules(), // chok_modules_gen.go — regenerate with `chok sync`
        chok.Override(db.Module(db.WithTables(db.Table(&Note{})))),
        chok.Routes(func(r chok.Router, k chok.Kernel) error {
            notes := store.New[Note](db.From(k), log.From(k),
                store.WithQueryFields("id", "title", "created_at"),
                store.WithUpdateFields("title", "body"))
            api := r.Group("/api/v1", account.Authn(k))
            api.Handle("GET", "/notes/{rid}", handler.HandleRequest(getNote))
            return nil
        }),
    ).Execute()
}
```

That boots an HTTP server with JSON logging, SQLite with
auto-migration, JWT-backed `/auth/register|login|refresh-token|...`,
an OpenAPI 3 spec at `/swagger`, `/healthz` `/livez` `/readyz` for
Kubernetes, and Prometheus `/metrics` — and only the modules you
declared are linked into the binary.

## Design

Three immutable adjectives:

- **Config-driven.** yaml sections declare what runs; `enabled:
  true|false` flips subsystems at runtime; `reload:"hot"` fields apply
  on SIGHUP without a restart. Code is reserved for business logic.
- **One blessed implementation per capability.** HTTP is stdlib
  `ServeMux` (Go 1.22+ patterns). ORM is gorm — invisible behind the
  store. Cache is otter + redis. Cron is robfig. JWT is golang-jwt.
  RBAC is casbin. Observability is Prometheus + OpenTelemetry. No
  parallel choices.
- **Internally complex, externally trivial.** A single-actor kernel
  runs the lifecycle (topological init, mount, serve, drain, reverse
  close); config is immutable RCU snapshots; components declare a
  `Descriptor` and the framework discovers capabilities by type. None
  of that appears in application code.

Guarantees you get structurally: unauthenticated queries on owned
models fail closed, external IDs are prefixed RIDs (numeric keys never
leak), optimistic locking rides a version column, secrets are redacted
in logs, raw SQL has exactly two doors — both named `Unsafe`.

## 30-second hello world

No yaml, one file, `go run .` — and what boots is production-shaped:
structured access logs with request IDs, graceful drain on SIGINT,
env-var config overrides, all on defaults.

```go
package main

import (
	"context"

	"github.com/zynthara/chok/v2"
	"github.com/zynthara/chok/v2/web"
)

func main() {
	chok.New("hello",
		chok.Use(web.Module()),
		chok.Routes(func(r chok.Router, _ chok.Kernel) error {
			web.GET(r, "/ping", func(context.Context, *struct{}) (string, error) {
				return "pong", nil
			})
			return nil
		}),
	).Execute()
}
```

`web.GET/POST/PUT/PATCH/DELETE` fuse routing with the typed binding
layer: one line is route + request binding + response encoding +
error mapping + OpenAPI registration.

## Quick start

```bash
go install github.com/zynthara/chok/v2/cmd/chok@latest

chok init myapp
cd myapp && go mod tidy && go run .
```

`curl localhost:8080/healthz`, then open <http://localhost:8080/swagger>.
The scaffold ships a working Note API (model + three routes), a
`chok.yaml` declaring the standard modules, and a `migrations/`
skeleton for versioned mode. Add a battery = add a yaml section, run
`chok sync`, done.

For a guided tour — auth, owner-scoped CRUD, optimistic locking over
curl — walk [`examples/blog`](examples/blog) (five minutes).

## Built-in modules

Every subsystem is a module: assembled via `chok.Use` (or the
generated `chokModules()`), declared by a `Descriptor`, configured by
its yaml section. Capabilities are discovered by the kernel
(mount/serve/migrate/reload/health/ready/drain), so the table below is
generated from the source of truth — `chok docs gen` keeps it honest.

<!-- gen:components -->
| Module | Section | Needs (`?` = optional) | Capabilities | Enabled by default | What it does |
|---|---|---|---|---|---|
| `log.Module()` | `log` | — | reload | always | Root logger section (level/format/outputs); hot level reload. |
| `web.Module()` | `http` | log?, metrics?, tracing?, authz? | serve, router | true | stdlib HTTP server + router + default middleware stack. |
| `health.Module()` | `health` | — | reload, mount, drain | true | /healthz /livez /readyz probes; drains readiness on shutdown. |
| `metrics.Module()` | `metrics` | — | mount | true | Prometheus registry + /metrics endpoint. |
| `debug.Module()` | `debug` | — | mount | false | /componentz topology and lifecycle-event dump (off by default). |
| `swagger.Module()` | `swagger` | http | mount | true | OpenAPI 3 spec generated from the route table + Swagger UI. |
| `tracing.Module()` | `tracing` | — | — | false | OpenTelemetry tracer provider (stdout/OTLP exporters). |
| `db.Module()` | `db` | log?, tracing?, metrics? | health, migrate | true | Database pool (sqlite/mysql/postgres) + migrations (auto/versioned/off); sqlite runs the pure-Go split-pool production shape. |
| `redis.Module()` | `redis` | log? | health | true | go-redis client with TLS/CA support; health probe. |
| `cache.Module()` | `cache` | redis?, log? | — | true | Layered cache: otter memory + redis + circuit breaker. |
| `scheduler.Module()` | `scheduler` | log? | health, serve | true | robfig cron with panic-safety, overlap policies and stats. |
| `audit.Module()` | `audit` | db, scheduler?, account?, log? | reload, mount, migrate | false | Compliance audit log: async DB sink, purge cron, admin API (opt-in). |
| `outbox.Module()` | `outbox` | db, scheduler?, log? | reload, health, migrate | true | Transactional outbox: same-transaction enqueue + at-least-once relay delivery. |
| `authz.Module()` | `authz` | db, redis?, audit?, log? | migrate, ready | true | casbin RBAC engine: adapter, Redis watcher, bootstrap seeding, decision audit. |
| `account.Module()` | `account` | db, log? | mount, migrate | true | User module: register/login/JWT/reset + login rate limit + OAuth providers. |
<!-- /gen:components -->

Configuration for every module: [`docs/config.md`](docs/config.md) —
and [`docs/chok.schema.json`](docs/chok.schema.json) gives your editor
completion and CI validation for `chok.yaml`.

Custom components implement `kernel.Component` (`Describe` / `Init` /
`Close` plus any behavior interfaces) and join the same lifecycle.

## CLI

```bash
chok init <name>            # scaffold a v2 project (boots immediately)
chok sync [--check]         # chok.yaml → chok_modules_gen.go (CI-gateable)
chok migrate create|up|status|repair   # audited forward-only migrations
chok docs gen [--check]     # components tables + config reference + JSON Schema
chok openapi export         # running app's OpenAPI spec → .json/.yaml
chok version [--json]       # build / VCS / runtime metadata
chok update [--ref vX.Y]    # upgrade the local CLI via go install
```

## Examples

| Path | What it shows |
|---|---|
| [`examples/blog`](examples/blog) | Quickstart-grade: JWT auth + a `Post` resource with owner scope, optimistic locking, soft delete, generated OpenAPI. Its acceptance test walks the README path over real HTTP in CI. |

A full-coverage example (`examples/tasker`) exercising authz, audit,
scheduler and custom components is on the roadmap.

## Documentation

| Topic | Where |
|---|---|
| Architecture & contracts (Chinese) | [`docs/design.md`](docs/design.md) |
| Data-layer usage guide (Chinese) | [`docs/db.md`](docs/db.md) |
| Configuration reference (generated) | [`docs/config.md`](docs/config.md) |
| v1 → v2 migration guide | [`docs/migration-v1-to-v2.md`](docs/migration-v1-to-v2.md) |
| Design changelog | [`docs/changelog.md`](docs/changelog.md) |
| Roadmap | [`docs/roadmap.md`](docs/roadmap.md) |
| GoDoc reference | <https://pkg.go.dev/github.com/zynthara/chok/v2> |
| Agent / AI assistant guidance | [`CLAUDE.md`](CLAUDE.md), [`AGENTS.md`](AGENTS.md) |

## Contributing

Conventional commits (`feat:`, `fix:`, `docs:`, `chore:`). Releases
are cut manually: changelog entry → tag → goreleaser publishes.
Public API changes must land with a changelog entry — CI runs
`apidiff` against the latest release tag, plus `chok docs gen --check`
and `chok sync --check` so generated surfaces can't drift.

```bash
make all      # tidy + lint + test + build
make smoke    # boot examples/blog as a sanity check
make test-pg  # store/db suites against Postgres (set CHOK_TEST_PG_DSN)
```

The architectural invariants are documented in [`CLAUDE.md`](CLAUDE.md)
and [`docs/design.md`](docs/design.md).

## License

Released under the [MIT License](LICENSE).
