# CLAUDE.md — Working on the chok framework

> Loaded automatically by Claude Code on every session in this repo.
> Keep under 200 lines or it gets truncated.

## What chok is

A Go web framework that bundles HTTP + DB + cache + auth + scheduler +
observability into one repository. Think "Rails for Go": single
blessed implementation per capability, configuration over code.

- Module path: `github.com/zynthara/chok`
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

- `App` is single-use: `Run` / `Execute` may be called at most once
- Lock order is `reloadMu → mu` everywhere; never acquire `mu` then
  `reloadMu` (would deadlock the Stop / Reload paths)
- `registry.Get` during `phaseStopping` returns only entries still in
  `available`; `Stop` scrubs lazily as each `Close` completes
- `App.Reload` uses `TryLock`; concurrent triggers coalesce via
  `ErrReloadInProgress` — do not queue
- Reload does **NOT** trigger `Migrate`. Schema changes require a
  restart. Document this if you add migration-adjacent features.
- Shutdown contexts use `context.WithoutCancel(parent)` so trace_id /
  request_id stay correlated; never `context.Background()` directly
- Components close in reverse-topo order, same-level in parallel
- `AddCleanup` callbacks run AFTER `registry.Stop` — they must not
  access components (they're already torn down)
- HTTP server `Stop` now follows `Shutdown` with `srv.Close()` so
  hung handlers can't outlive registry teardown

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
- Tests use `choktest.NewTestDB` / `choktest.NewTestStore`; in-package
  registry tests use the `mkReg()` helper

## Common pitfalls — observed in 14 rounds of review

- **DON'T** call `k.Get` inside `Component.Close` — peers in the same
  topo level may already have `markUnavailable`'d themselves
- **DON'T** bypass the store with raw `*gorm.DB` unless going through
  `s.DB()` (no scopes) or `s.ScopedDB(ctx)` (with scopes) escape hatches
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
- **DON'T** mock the database in store tests — use SQLite via
  `choktest.NewTestDB`; mocked tests miss real schema interactions

## Testing requirements

- Every bug fix ships with a regression test in the same commit
- Run `go test ./... && go vet ./...` before committing
- For review-driven fixes, name tests after the issue:
  `TestX_RoundNDescription` or `TestFix_RoundNX_<issue>`
- The full suite must pass; `examples/blog` must start cleanly
  (`cd examples/blog && go run ./cmd/blog` and Ctrl-C)

## Where things live

| Path | Purpose |
|---|---|
| `chok.go` | App lifecycle (the spine, ~1200 LOC) |
| `options.go` | `WithXxx` constructors |
| `config.go` | config loading + validation (`SelfValidating` recursion stop) |
| `component/` | Component / Kernel / Registry — the abstraction core |
| `parts/` | 15 built-in Components (log, db, cache, redis, http, ...) |
| `store/` | generic CRUD; locator + changes + scopes |
| `store/where/` | query DSL (`resolveField` does identifier validation) |
| `handler/` | generic `HandleRequest[T,R]` |
| `middleware/` | gin middleware (Authn / Authz / Recovery / Timeout / ...) |
| `server/` | HTTP server impl (Stop force-closes after Shutdown timeout) |
| `account/` | ready-to-use user module + login rate limiter |
| `cache/` | memory + file + redis Chain + circuit Breaker |
| `db/` | gorm wrapper + Model mixins + Migrate |
| `examples/blog/` | **quickstart-grade** example — do NOT bloat |
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
- New public APIs must have godoc + an example in `examples/blog`
  (if quickstart-relevant) or `examples/tasker` (if advanced)

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
