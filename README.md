<p align="center">
  <a href="README_zh.md">中文</a> | English
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.26+" />
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green" alt="MIT" /></a>
  <a href="https://pkg.go.dev/github.com/zynthara/chok"><img src="https://pkg.go.dev/badge/github.com/zynthara/chok.svg" alt="Go Reference" /></a>
  <a href="https://github.com/zynthara/chok/actions/workflows/ci.yml"><img src="https://github.com/zynthara/chok/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
</p>

<h1 align="center"><code>chok</code></h1>
<p align="center"><b>An opinionated, configuration-driven Go web framework.</b></p>

---

`chok` bundles HTTP, database, cache, JWT auth, scheduler, and observability
into a single Go module. One YAML file enables or disables every subsystem;
all wiring is generated from your config struct.

```yaml
# chok.yaml
http:      { addr: ":8080" }
log:       { level: info, format: json, output: [stdout] }
database:  { driver: sqlite, sqlite: { path: "app.db" } }
account:   { enabled: true, signing_key: "..." }
swagger:   { enabled: true, title: "My API" }
```

```go
// main.go — three lines wire the entire app
type Config struct {
    HTTP     config.HTTPOptions     `mapstructure:"http"`
    Log      config.SlogOptions     `mapstructure:"log"`
    Database config.DatabaseOptions `mapstructure:"database"`
    Account  config.AccountOptions  `mapstructure:"account"`
    Swagger  config.SwaggerOptions  `mapstructure:"swagger"`
}

var cfg Config

func main() {
    chok.New("myapp",
        chok.WithConfig(&cfg),
        chok.WithRoutes(func(_ context.Context, a *chok.App) error {
            a.API("/api/v1", a.AuthMiddleware()).GET("/me", meHandler)
            return nil
        }),
    ).Execute()
}
```

That's an HTTP server with JSON logging, SQLite + auto-migration, JWT-backed
`/auth/register|login|refresh-token|change-password|...`, an OpenAPI 3.0
spec at `/swagger`, `/healthz` `/livez` `/readyz` for Kubernetes, and a
Prometheus `/metrics` endpoint — without a single explicit `Register` call.

## Design

Three immutable adjectives:

- **Config-driven.** `enabled: true|false` is the primary on/off switch.
  Code changes are reserved for business logic, not subsystem assembly.
- **One blessed implementation per capability.** HTTP is gin. ORM is gorm.
  Cache is Otter + Badger + Redis. Cron is robfig. JWT is golang-jwt.
  Tracing is OpenTelemetry. No parallel choices.
- **Internally complex, externally trivial.** A `Component` abstraction
  with topological lifecycle, hot-reload dispatch, and health aggregation
  hides behind `app.Register(c)` and `app.Logger()`.

## Quick start

```bash
go install github.com/zynthara/chok/cmd/chok@latest

chok init myapp
cd myapp && go mod tidy && make run
```

The scaffold ships with `cmd/`, `internal/{app,handler}`, `configs/`, a
`Makefile` that injects build metadata via ldflags, and a `chok.yaml`
that enables HTTP + logging + SQLite + JWT auth + Swagger. Hit
<http://127.0.0.1:8080/healthz> to confirm the boot.

## Built-in Components

15 subsystems, each registered as a `Component` with `Init` / `Close` and
optional `Reload` / `Health` / `Router` / `Migrate` capabilities. Most
auto-register from your `Config` struct; the rest you opt into in
`WithSetup`.

| Component | Auto-register | Selected by | Capability |
|---|---|---|---|
| `LoggerComponent`    | yes | `SlogOptions`        | logger + access log + reload |
| `HTTPComponent`      | yes | `HTTPOptions`        | gin server + middleware stack |
| `DBComponent`        | yes | `DatabaseOptions`    | gorm + auto-migrate (sqlite/mysql) |
| `RedisComponent`     | yes | `RedisOptions`       | go-redis client + health |
| `CacheComponent`     | yes | `CacheMemory/FileOptions` | memory + file + Redis chain |
| `AccountComponent`   | yes | `AccountOptions`     | register / login / JWT / reset |
| `SwaggerComponent`   | yes | `SwaggerOptions`     | OpenAPI 3.0 + Swagger UI |
| `HealthComponent`    | yes | (whenever HTTP)      | `/healthz` `/livez` `/readyz` |
| `MetricsComponent`   | yes | (whenever HTTP)      | Prometheus `/metrics` |
| `DebugComponent`     | yes | `DebugOptions`       | `/componentz` topology dump |
| `TracingComponent`   | explicit | code            | OTel tracer + OTLP exporter |
| `SchedulerComponent` | explicit | code            | robfig cron with stats |
| `PoolComponent`      | explicit | code            | bounded async worker pool |
| `JWTComponent`       | explicit | code            | extra JWT managers |
| `AuthzComponent`     | explicit | code            | pluggable authorizer |

Custom components implement the same interface and integrate with the
registry's lifecycle, hot-reload, and health checks. See
[`docs/design.md`](docs/design.md) §13.

## CLI

```bash
chok init <name>          # scaffold a new project
chok version [--json]     # build / VCS / runtime metadata
chok update [--ref vX.Y]  # upgrade the local CLI via go install
```

Version metadata resolves in order: ldflags injected by `make build` /
goreleaser → `debug.ReadBuildInfo` (so `go install ...@latest` shows a
real pseudo-version + git hash) → `dev` / `unknown` fallback.

## Examples

| Path | What it shows |
|---|---|
| [`examples/blog`](examples/blog) | Quickstart-grade: HTTP + SQLite + JWT + Swagger + a single `Post` resource with optimistic locking, soft delete, and owner scope. Boot in 5 minutes. |

A full-coverage example (`examples/tasker`) exercising every Component and
custom-component pattern is on the roadmap; see
[`docs/roadmap.md`](docs/roadmap.md).

## Documentation

| Topic | Where |
|---|---|
| Architecture & API contracts | [`docs/design.md`](docs/design.md) |
| Design changelog (per release) | [`docs/changelog.md`](docs/changelog.md) |
| Roadmap | [`docs/roadmap.md`](docs/roadmap.md) |
| GoDoc reference | <https://pkg.go.dev/github.com/zynthara/chok> |
| Agent / AI assistant guidance | [`CLAUDE.md`](CLAUDE.md), [`AGENTS.md`](AGENTS.md) |

## Contributing

Conventional commits (`feat:`, `fix:`, `docs:`, `chore:`) drive
release-please's automatic versioning and CHANGELOG generation. Tag
push triggers a goreleaser build for linux / darwin / windows × amd64
/ arm64.

```bash
make all      # tidy + lint + test + build
make smoke    # boot examples/blog as a sanity check
make snapshot # run goreleaser locally without publishing
```

The hard rules and the architectural invariants the project tries to
preserve are documented in [`CLAUDE.md`](CLAUDE.md).

## License

Released under the [MIT License](LICENSE).
