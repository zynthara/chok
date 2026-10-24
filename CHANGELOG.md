# Changelog

All notable changes to chok v3 (`github.com/zynthara/chok/v3`) are
documented in this file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Releases are cut by hand: a maintainer writes the entry here, runs
`make verify`, tags and pushes the tag; goreleaser publishes the `chok`
command for each platform. While v3 is in alpha, `hack/apidiff.sh` reports
incompatible API changes against the previous v3 release without blocking.
The v1 and v2 histories are in the `CHANGELOG.md` of the
[`v2` branch](https://github.com/zynthara/chok/blob/v2/CHANGELOG.md).

<!-- When tagging: replace 未发布 (unreleased) with the release date, YYYY-MM-DD. -->

## [v3.0.0-alpha.1] - 未发布

The first release of v3, a rewrite: chok becomes a toolkit an application
imports and assembles with plain Go code. There is no kernel, no registry
and no generated file. Upgrading from v2 is a port, not a dependency bump:
[docs/migration-v2-to-v3.md](docs/migration-v2-to-v3.md) maps every v2
package, configuration key, API, table and metric to its v3 form.

### Added

- **`chok`**: `Load[T]` / `MustLoad[T]` load one typed configuration —
  defaults, `chok.yaml`, environment variables, flags — strictly: an
  unknown key fails startup with a suggestion, `validate` tags and
  `Validate` methods run, secrets are masked, and `--check-config`
  validates and prints it from the application binary. `chok.Base` holds
  the `app`, `http` and `log` sections.
- **`chok`**: the App and its lifecycle — `New`, `Use` (Start, Close,
  Health, Mount and the telemetry slot by interface), `Run` / `Execute`
  with a start phase gated on health, drain, and SIGINT / SIGTERM /
  SIGQUIT / SIGHUP (which applies `log.level` only).
- **`chok`**: typed handlers `GET` / `POST` / `PUT` / `PATCH` / `DELETE`
  and `List`, binding `path` / `query` / `header` / `json` tags with
  `encoding/json/v2` and `validate` rules; errors that describe their HTTP
  envelope through `chok.Coder`; the default middleware stack (recover,
  request id, client ip, access log, timeout, CORS, CSRF); an OpenAPI 3.1
  document at `/openapi.json` with its UI at `/docs`; `/healthz`,
  `/readyz`, `/livez` and optional pprof.
- **`db`**: SQLite (pure Go), MySQL and Postgres as peers, UTC
  throughout; transactions on the context; model bases with prefixed
  public ids; soft-unique indexes; `migrate: auto | versioned | off` with
  one `chok_migrations` ledger shared by the application and the
  batteries, statement-level resume on MySQL, and `MarkApplied`.
- **`store`** and **`store/where`**: a generic `Store[T]` with owner
  scoping that fails closed, patches with optimistic locking, cursor
  pages, aggregates and projections as generic methods, and error types
  that map to 400 / 401 / 404 / 409.
- **`identity`**: accounts, JWT access tokens checked against the user on
  every request, sessions bounded by `refresh_ttl`, login rate limiting,
  OAuth through **`identity/google`**, **`identity/github`**,
  **`identity/apple`** and **`identity/facebook`**, and an audit log with
  `/audit/logs`; **`identity/rbac`** adds casbin role-based access control
  with a Redis watcher.
- **`jobs`**: cron-scheduled periodic jobs with skip, queue or allow
  overlap (a robfig/cron parser built in; see NOTICE), and transactional
  outbox workers that deliver at least once in order.
- **`telemetry`**: Prometheus metrics named after the OpenTelemetry
  semantic conventions (`http_server_request_duration_seconds`,
  `db_client_operation_duration_seconds`, …) on `/metrics`, OpenTelemetry
  traces for requests and SQL statements, and a slog bridge to
  OpenTelemetry logs.
- **`cache`**: an in-memory and a Redis layer behind one `Config`, with a
  circuit breaker and coalesced loads.
- **`choktest`**: a throwaway database per test on the lane's dialect and
  an in-process App client.
- **The `chok` command**: `init` scaffolds a project that serves on
  `go run .` and names the command in a `tool` directive; `migrate create
  | up | status | mark-applied` works the `chok_migrations` ledger from
  the application's own configuration; `openapi export` saves a running
  application's document as JSON or YAML; `version` reports the build.
- **Documentation**: [quickstart](docs/quickstart.md), whose programs are
  compiled by a test; a generated [configuration reference](docs/config.md)
  and [API map](docs/api.md), both kept current by tests;
  [design](docs/design.md) and the [v2 migration guide](docs/migration-v2-to-v3.md),
  in Chinese; English and Chinese READMEs.
- **Budgets in CI**: at most ten public packages, 220 exported
  declarations (with per-package caps), 40 option constructors, 30 direct
  dependencies, 150 configuration keys, and an `examples/blog` of 120
  application lines — `make budget`, part of `make verify`.

### Changed

- Module path `github.com/zynthara/chok/v3`; Go 1.27.
- Assembly is code: components are values constructed by the application
  and registered with `app.Use`; dependencies are passed to constructors.
- Configuration is one strict struct; only `log.level` reloads.
- Functional options drop the `With` prefix; yaml structs are named
  `Config`.
- Errors map to responses through `chok.Coder`; the response envelope
  (`code`, `reason`, `message`, `request_id`, `metadata`) is unchanged.
- Versioned migrations use 14-digit UTC versions on the single
  `chok_migrations` ledger.
- Metric and span names follow the OpenTelemetry semantic conventions.
- `identity` merges v2's account, auth, authz and audit; `jobs` merges
  scheduler and outbox; `telemetry` merges metrics and tracing; `cache`
  absorbs the redis module.

### Removed

- The kernel: the component registry, `Descriptor`, `k.Get` lookups, the
  event bus and `/componentz`.
- `chok sync` and generated assembly, `chok gen fields`, `chok update`,
  and the JSON Schema of `chok.yaml`.
- Configuration hot reload beyond `log.level`, file watching, RCU
  snapshots and named instances.
- Read-only database handles, multiple instances, dbresolver, migration
  manifests, repair history, fences and baseline adoption.
- The error mapper registry; viper; fsnotify; the robfig/cron dependency.

[v3.0.0-alpha.1]: https://github.com/zynthara/chok/releases/tag/v3.0.0-alpha.1
