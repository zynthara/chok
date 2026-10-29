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
  SIGQUIT / SIGHUP (which applies `log.level` only). The drain waits only
  once `/readyz` has been probed, and a second SIGINT or SIGTERM during
  the stop stops fast. `Load`'s warnings reach the App's logger, as JSON
  under `log.format: json`.
- **`chok`**: typed handlers `GET` / `POST` / `PUT` / `PATCH` / `DELETE`
  and `List`, binding `path` / `query` / `header` / `json` tags with
  `encoding/json/v2` and `validate` rules; errors that describe their HTTP
  envelope through `chok.Coder`; the default middleware stack (recover,
  request id, client ip, access log, timeout, CORS, and CSRF protection,
  on by default); an OpenAPI 3.1 document at `/openapi.json` with its UI
  at `/docs`, each route's security taken from its authentication
  middleware; `/healthz`, `/readyz`, `/livez` and optional pprof.
- **`chok`**: `SPA(r, prefix, fsys)` serves a single-page application
  from an `fs.FS` such as an embedded `dist`: files as they are, with
  content-hash ETags and immutable caching under `assets/`, `index.html`
  for the page paths a browser asks for, and the 404 envelope for the
  rest; the API's routes keep precedence.
- **`db`**: SQLite (pure Go), MySQL and Postgres as peers, UTC
  throughout; transactions on the context; model bases with prefixed
  public ids; soft-unique indexes; `migrate: auto | versioned | off` with
  one `chok_migrations` ledger shared by the application and the
  batteries, statement-level resume on MySQL, and `MarkApplied`.
- **`store`** and **`store/where`**: a generic `Store[T]` with owner
  scoping that fails closed, patches with optimistic locking, cursor
  pages, aggregates and projections as generic methods, and error types
  that map to 400 / 401 / 404 / 409. `store:"query"` fences what a URL
  may filter and sort on (values read as the field's type); code names
  any mapped column.
- **`identity`**: accounts, JWT access tokens checked against the user on
  every request, sessions bounded by `refresh_ttl`, login rate limiting,
  OAuth through **`identity/google`**, **`identity/github`**,
  **`identity/apple`** and **`identity/facebook`**, and an audit log with
  `/audit/logs`; **`identity/rbac`** adds casbin permissions over
  identity's roles — `admin` passes every check — with tenant domains and
  a Redis watcher. `auth.admins` makes the listed addresses admins once
  they are verified; `Users()` finds a user by email and lists every user
  for an admin page — to the `admin` role and those `identity/rbac` grants
  `user/read` only, wherever the list is mounted.
- **`identity`**: passwordless sign-in (`auth.passwordless`):
  `POST /auth/passwordless/start` answers 202 whatever the address and
  mails a code and a magic-link token in the background;
  `POST /auth/passwordless/verify` trades either for a session —
  `InvalidCode` with `attempts_left`, `CodeExpired` once a code expired,
  was used or was tried `max_attempts` times — and creates the account of
  a first sign-in unless `auto_register` is off. Codes are stored as
  HMACs in the new table `auth_login_codes` (migration
  `20261025000002`), starts are rate limited per client address, and an
  hourly sweep deletes the expired codes. `auth.passwordless.only` leaves
  the code the only way in: register, login, forgot-password,
  reset-password and change-password are not mounted.
- **`identity`**: cookie mode (`auth.cookie`): register, login,
  refresh-token, passwordless verify and the OAuth exchange set the
  access token in an HttpOnly cookie as well (with `only`, in the cookie
  alone); `Authn` reads it from requests without an `Authorization`
  header; `POST /auth/logout` clears it; the OpenAPI document lists it
  as `cookieAuth`. Pair it with `http.csrf`. `auth.reset_url` gives the
  password-reset mail a link to the reset page.
- **`jobs`**: cron-scheduled periodic jobs with skip, queue or allow
  overlap (a robfig/cron parser built in; see NOTICE), and transactional
  outbox workers that deliver at least once in order, the payload decoded
  into the worker's type. A failing message is retried with a growing
  wait and, after `max_attempts`, parked in the dead letters
  (`Dead`, `Requeue`, `Discard`) while its topic moves on; a graceful
  restart does not deliver again what was delivered. `jobs.Observe`
  streams every run — started, finished, failed, skipped — and every
  parked message as a `jobs.Event`, for a live jobs panel through
  `realtime`.
- **`telemetry`**: Prometheus metrics named after the OpenTelemetry
  semantic conventions (`http_server_request_duration_seconds`,
  `db_client_operation_duration_seconds`, …) on `/metrics`, OpenTelemetry
  traces for requests and SQL statements, and a slog bridge to
  OpenTelemetry logs.
- **`cache`**: an in-memory and a Redis layer behind one `Config`, with a
  circuit breaker and coalesced loads.
- **`realtime`**, a battery that pushes events to browsers as they happen:
  one `Hub`, registered with `app.Use`, serves topics over a WebSocket
  (`github.com/coder/websocket`) and over server-sent events. A page trades
  its token for a one-time ticket at `POST /realtime/ticket`, so no
  long-lived token travels in a URL; `realtime.Authorize` decides every
  subscription — a refused one is a `refused` event, never a closed
  connection; `refused`, `reset` and `error` are the hub's own types, which
  `Publish` refuses — and `user:<UserID>` topics are their user's alone
  (`hub.Send`). `hub.Publish` never waits on a slow connection — one that
  falls `send_buffer` events behind is dropped — and the last `replay`
  events stay in memory, so a reconnecting page resumes after the last
  event it saw (`after`, `Last-Event-ID`) or gets a `reset`. With
  `realtime.Redis`, tickets and events span every replica. See
  [docs/realtime.md](docs/realtime.md).
- **`mail`**, a battery for transactional mail with the standard library
  only: SMTP through a relay (STARTTLS or TLS from the first byte, AUTH
  PLAIN, every recipient Bcc included, within `mail.timeout`), `.eml`
  files in a directory for development, or the log; templates in
  `html/template`, `text/template` or both, with partials and a subject
  define, parsed at startup; attachments and inline images; RFC 5322
  composition with encoded headers and Message-IDs at the sender's domain;
  `mail.Sender` swaps the transport for an HTTP API or a test, and
  `SendTemplate` is what `identity.Sender` takes. See
  [docs/mail.md](docs/mail.md).
- **`choktest`**: a throwaway database per test on the lane's dialect and
  an in-process App client.
- **The `chok` command**: `init` scaffolds a project that serves on
  `go run .` and names the command in a `tool` directive; `migrate create
  | up | status | mark-applied` works the `chok_migrations` ledger from
  the application's own configuration; `openapi export` saves a running
  application's document as JSON or YAML; `version` reports the build.
- **Documentation**: [quickstart](docs/quickstart.md), whose programs are
  compiled by a test; [realtime](docs/realtime.md), the browser's side of
  the realtime battery; [mail](docs/mail.md), from a first mail to a
  passwordless sign-in with a cookie session; a generated [configuration reference](docs/config.md)
  and [API map](docs/api.md), both kept current by tests;
  [design](docs/design.md) and the [v2 migration guide](docs/migration-v2-to-v3.md),
  in Chinese; English and Chinese READMEs.
- **`examples/chatter`**: chat rooms over a WebSocket, a bot answering
  from a jobs worker, and a jobs panel over server-sent events, behind a
  sign-in by mailed code with a cookie session, in about 240 lines of Go,
  two mail templates and one HTML page without a build step.
- **Budgets in CI**: at most twelve public packages, 250 exported
  declarations (with per-package caps), 50 option constructors, 30 direct
  dependencies, 180 configuration keys, and an `examples/blog` of 120
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
- **Breaking:** identity sends mail through a function:
  `identity.Sender(fn)` takes `func(ctx, to, template string, data any) error`,
  which `(*mail.Mailer).SendTemplate` is, so the wiring is
  `identity.Sender(mailer.SendTemplate)`. identity calls it with the
  mail's kind (`password-reset` or `login-code`) as the template name and
  an `identity.Mail` — the `Token`, the `Code`, a `Link` and `ExpiresAt` —
  as the data. It replaces v2's `account.Sender`
  (`Send(ctx, to, code string)`) and the `MailSender` interface of earlier
  v3 snapshots, whose senders no longer compile. identity sends in
  tracked goroutines, `Close` waits for the mails in flight within its
  context, and a failed send is logged as a warning.
- **Breaking:** `chok.Public()` and `http.openapi.bearer_auth` are gone.
  A route's OpenAPI security is what its authentication middleware
  reports: identity's `Authn` reports `bearerAuth`, and `cookieAuth` in
  cookie mode; a route without one is public in the document, as it is
  in fact. A middleware of the application's own is not recognised.
- **Breaking:** `http.csrf` is on by default, a zero `chok.Base`
  included: cross-site POST / PUT / PATCH / DELETE from a browser get a
  403, while same-origin requests, the origins of `http.cors`, and
  clients that send no `Sec-Fetch-Site` or `Origin` (curl, mobile apps,
  services) pass. `chok.AllowCrossSitePost` lets a third party's form
  POST to one route — a payment gateway's return; identity uses it for
  its OAuth callbacks, which Apple posts. Turning it off while a route
  takes a cookie session logs a warning at startup.
- **Breaking:** one set of roles. `identity/rbac` decides on the
  principal's `Roles` (identity's `users.roles`, set with
  `Users().SetRoles`): `admin` passes everything, then the user's own
  grants, then what `GrantRole` gave any of the roles; roles within a
  domain stay in casbin, and the InDomain membership methods refuse the
  empty domain. `AddUserToRole`, `RemoveUserFromRole`, `UserRoles` and
  `BootstrapAdmin` are gone: list the first admin in `auth.admins`.
  A password reset now marks the address verified.
- **Breaking:** linking a provider needs a verified address:
  `POST {prefix}/identities/link` answers 403 `EMAIL_NOT_VERIFIED` for an
  account whose address nobody has proven, since whoever registered it
  need not own it. The first proof of an address — reset-password, a
  passwordless code, `Users().MarkEmailVerified` — unlinks the providers
  linked before it; v2 let unverified accounts link.
- **Breaking:** a passwordless code that proves an address for the first
  time takes the account over clean: the password set before it no
  longer logs in (reset-password sets a new one) and the account's tokens
  are revoked, since whoever registered the address need not own it.
  `Users().MarkEmailVerified` keeps both: list in `auth.admins` only
  addresses their owners registered, or turn registration off.
- **Breaking:** `jobs.Worker` is generic: `Worker[T](topic, fn)` with
  `fn func(ctx, jobs.Message[T]) error`, the payload decoded into `T`
  (`Worker[jsontext.Value]` for the JSON as stored); a payload that does
  not decode is parked. A failing message is no longer retried on every
  sweep forever: see `max_attempts`, `retry_backoff` and `max_backoff`.
  The jobs migration `20261025000001` gained the failure columns and two
  tables in place: drop and recreate a development database made by an
  earlier snapshot.
- **Breaking:** `jobs.retention` defaults to `168h` when the
  configuration does not set it; a `jobs.Config` built in code still
  keeps messages forever with a zero `Retention`.
- **Breaking:** `store`: an unknown field named by code — in a `where`
  option, a locator, an aggregate, `Order`, `Set` or an upsert's conflict
  columns — is a 500 that keeps the field's name out of the response
  (`errors.Is(err, where.ErrUnknownField)` still holds); an unknown field
  from the URL is still a 400 that lists the allowed ones.

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
