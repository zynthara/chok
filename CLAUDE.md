# CLAUDE.md — Working on the chok framework

> Loaded automatically by Claude Code on every session in this repo.
> Keep under 200 lines or it gets truncated.

## What chok is

A Go web framework that bundles HTTP + DB + cache + auth + scheduler +
observability into one repository. Think "Rails for Go": single
blessed implementation per capability, configuration over code.

- Module path: `github.com/zynthara/chok/v2` — **main is the v2
  rewrite (in progress)**. v1 is sealed at tag `v0.1.4` (v1 module
  path, security fixes only). Implementation SPEC lives in
  `.private/docs/specs/v2-claude.md`; milestones M0-M5 in its §10.
- Three immutable adjectives: **config-driven**, **single blessed
  implementation**, **internally complex / externally trivial**

## Three rules that matter most

1. **Config drives behaviour.** `enabled: true/false` in `chok.yaml`
   is the primary on/off switch — prefer it over Go code changes.
2. **Every subsystem is a Component.** `Init` / `Close` mandatory;
   `Reload` / `Health` / `Router` / `Migrate` optional via type
   assertion. New built-in subsystems must follow this shape.
3. **One official implementation per capability.** Do not add parallel
   choices (no "use mux instead of gin", no "switch to sqlc"). The
   blessed stack is gin / gorm / otter+badger+redis / robfig-cron /
   golang-jwt / Prometheus / OpenTelemetry.

## Architecture invariants — do not violate

> Since M1 the control plane is the v2 kernel (`kernel/` + `conf/`):
> single-actor lifecycle, RCU config snapshots, Descriptor-declared
> components. The lock-order / TryLock / lazy-scrub bullets below
> survive ONLY inside the v1 transition residue (`component/` +
> `parts/`, used by the 9 not-yet-migrated batteries, M3-M4).

- `App` is single-use: `Run` / `Execute` may be called at most once
- v2 kernel: all lifecycle transitions go through the control
  goroutine; reads (`Lookup`/`Health`/`Ready`) use the atomic view —
  never add locks or a second lifecycle writer
- Reload coalesces: overlapping triggers get `ErrReloadInProgress`
  (CAS gate in v2) — do not queue
- [v1 residue] lock order `reloadMu → mu`; `registry.Get` during
  `phaseStopping` returns only still-`available` entries
- Reload does **NOT** trigger `Migrate`. Schema changes require a
  restart. Document this if you add migration-adjacent features.
- Shutdown contexts use `context.WithoutCancel(parent)` so trace_id /
  request_id stay correlated; never `context.Background()` directly
- Components close in reverse-topo order, same-level in parallel
- `AddCleanup` callbacks run AFTER `registry.Stop` — they must not
  access components (they're already torn down)
- web `Serve` wind-down bounds `Shutdown` by `http.shutdown_timeout`
  then force-`Close`s so hung handlers can't outlive registry teardown

## Coding conventions when modifying chok itself

- New `*Options` types implement `config.Validatable`; discriminator
  types (one of N branches selected by a field) also implement
  `config.SelfValidating` so the recursive walker stops descending
- New `Component` declares `Dependencies` / `OptionalDependencies`;
  never call `k.Get(peer)` inside `Close` (peer may already be closed)
- Optional capabilities are exposed via narrow interfaces (`Reloadable`,
  `Healther`, `Router`, `Migratable`, `ReadyChecker`, ...) — Registry
  uses type assertion. Add new ones in `component/component.go` only
  when there's a clear need; prefer composition.
- Use `rid.New(prefix)` for any externally-exposed ID; never leak the
  internal `uint` primary key in API responses
- Errors: wrap with `fmt.Errorf("...: %w", err)` or build via
  `apierr.New(...)` — never return plain strings
- Logging on the request path: `middleware.LoggerFrom(ctx)`; reserve
  `app.Logger()` for startup / shutdown / cron contexts
- Tests open a real database via `db/dbtest.Open(t)` — SQLite by
  default, Postgres when `CHOK_TEST_DRIVER=postgres` +
  `CHOK_TEST_PG_DSN` are set (M3 dual-run; store/db test setups all
  ride it). `choktest` is the exported harness for downstream apps
  (`NewTestDB` returns `*db.DB`); in-repo it also backs db module
  tests. In-package registry tests use the `mkReg()` helper

## Common pitfalls — observed in 14 rounds of review

- **DON'T** call `k.Get` inside `Component.Close` — peers in the same
  topo level may already have `markUnavailable`'d themselves
- **DON'T** bypass the store with raw `*gorm.DB` unless going through
  the `s.Unsafe(ctx)` escape hatch (tx-aware, scopes applied,
  fail-closed on scope errors) or the handle-level `h.Unsafe(ctx)`
- **DON'T** rely on store auto-discovery in production — always declare
  `WithQueryFields` / `WithUpdateFields` explicitly. Auto-discovery
  emits a warn log because the implicit set is fragile.
- **DON'T** use `store.Set(map)` when optimistic locking matters —
  prefer `store.Fields(&obj)`, which extracts `obj.Version` automatically
- **DON'T** use `store.Where(...)` without at least one filter — the
  framework returns `ErrMissingConditions` to prevent
  `Delete(Where())` from clearing the table
- **DON'T** use `context.Background()` in shutdown paths — lose trace
  correlation. Use `context.WithoutCancel(ctx)` instead.
- **DON'T** put pointer fields in `*Options` types — Reload's `Set()`
  copies values onto the live config and pointer fields end up stale
- **DON'T** mock the database in store tests — use a real in-memory
  SQLite; mocked tests miss real schema interactions

## Testing requirements

- Every bug fix ships with a regression test in the same commit
- Run `go test ./... && go vet ./...` before committing
- For review-driven fixes, name tests after the issue:
  `TestX_RoundNDescription` or `TestFix_RoundNX_<issue>`
- The full suite must pass; during the v2 transition (M1-M4) the
  **current milestone's fixture app** must start cleanly instead of
  the blog smoke test (`examples/blog` is archived as
  `examples/_v1_blog`, build-ignored; blog is rebuilt on the v2 API
  in M5 and the smoke discipline returns then).
  Current fixture: `go run ./internal/fixture/m3` (from the repo
  root) and Ctrl-C; store/db changes also run the Postgres lane
  (`make test-pg` with `CHOK_TEST_PG_DSN`, or let CI's service run it).

## Where things live

| Path | Purpose |
|---|---|
| `chok.go` | v2 App thin shell: New / Use / Routes / Section / Run |
| `options.go` | `WithXxx` constructors |
| `config.go` | config loading + validation (`SelfValidating` recursion stop) |
| `kernel/` + `conf/` | v2 control plane: Descriptor / actor Registry / RCU config |
| `component/` | v1-residue Component core (used by `parts/` until M4) |
| `parts/` | v1-residue battery glue (db, cache, redis, account, ... — 9 left) |
| `db/` | v2 data module: Module/From + `*db.DB` thin handle + versioned migration engine (+ v1-residue constructors for `parts/`) |
| `store/` | generic CRUD; locator + changes + scopes; opt-in `WithBus` events |
| `store/where/` | query DSL (`resolveField` does identifier validation) |
| `handler/` | generic `HandleRequest[T,R]` — stdlib http.Handler + Meta |
| `middleware/` | stdlib middleware set (`func(http.Handler) http.Handler`) |
| `web/` | v2 HTTP module: Server + Router + default middleware stack |
| `swagger/` `tracing/` `health/` `metrics/` `debug/` | v2 observability / docs modules |
| `account/` | ready-to-use user module + login rate limiter |
| `cache/` | memory + file + redis Chain + circuit Breaker |
| `examples/_v1_blog/` | archived v1 example (build-ignored); blog returns on the v2 API in M5 |
| `examples/tasker/` | (planned) full-coverage example |
| `docs/design.md` | architecture source of truth |

## Documentation conventions

- `design.md` is the canonical architecture doc — keep it in
  sync when changing public contracts
- This project uses **plain filenames for public docs** (no
  `-claude.md` suffix). Tool-protocol files like `CLAUDE.md` and
  `AGENTS.md` keep their fixed names because their loaders demand
  the exact spelling.
- Internal planning / scratch / agent SPEC drafts that should not
  ship to the public repo live under `.private/` (gitignored).
  When in doubt about whether something belongs in `docs/`, ask:
  *"Does an external user reading this learn something they can
  act on?"* If no, it goes in `.private/`.
- Code comments are English; design docs are Chinese (team preference)
- New public APIs must have godoc; example-app coverage
  (`examples/blog` quickstart / `examples/tasker` advanced) resumes
  when the examples are rebuilt on the v2 API in M5 — during M1-M4,
  cover new surface in the milestone fixture app instead

## Commit message style

- Prefix with `fix:` / `feat:` / `docs:` / `refactor:` / `chore:`
- Review-driven fixes use `fix: round-N review — <punchline>` and
  group the body by issue ID (`#1`, `#2`, ...) so reviewers can map
  commit lines back to the original report
- One coherent change per commit; never batch unrelated fixes
- Never use `--no-verify`, never amend a published commit, never
  force-push to `main`

## Workflow when reviewing chok code

- Spawn parallel `Explore` agents for big-picture sweeps across
  unrelated subsystems (lifecycle / data / middleware)
- **Verify every agent finding against current code** — agent reports
  carry ~50% false positives (race claims that are actually under a
  lock, "missing recover" where one already exists, etc.)
- Categorise by impact: Critical (breaks quickstart / corrupts data /
  leaks resources) vs High (request DoS / security bypass) vs Medium
  (defence-in-depth) vs Low (doc drift / cosmetic)
- In the final report, document false positives explicitly so the
  user understands what was rejected and why — silent rejection looks
  like missed analysis
