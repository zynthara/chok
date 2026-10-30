# CLAUDE.md — working on chok v3

> Loaded by Claude Code on every session in this repo; AGENTS.md points here.
> Keep it under 120 lines.

## What chok v3 is

chok is an opinionated Go application toolkit that an app imports and wires
with plain Go code — no kernel, no registry, no generated files. One strict
typed config, typed HTTP handlers with runtime OpenAPI and a gorm-backed
store with owner scoping cover a CRUD resource with four imports. Batteries
(identity, jobs, telemetry, cache, realtime, mail) are values you construct
and hand to `app.Use`, never services you look up.

- Module `github.com/zynthara/chok`, Go 1.27 (toolchain go1.27.3), built on
  `main`. v3 is a rewrite: v2 code is a porting source, not a base.
- v2 is sealed on branch `v2` at `65651d4` (tags `v2.0.0-beta.N`); its source
  is checked out read-only at `~/go/src/chok-v2` (a git worktree). Port by
  copy + rewrite, never `git mv` across trees; never commit to `v2` or move a tag.

## Public packages (plan §1 D3) — everything else is internal/

`chok` (root) · `db` · `store` · `store/where` · `identity` · `identity/rbac` ·
`identity/{google,github,apple,facebook}` · `jobs` · `telemetry` · `cache` ·
`choktest` · `realtime` · `mail`; the CLI is `cmd/chok`. Batteries import the root, the
root no battery and no gorm. A new public package is an orchestrator decision.

## Three rules

1. **Config is strict.** One typed struct loaded by `chok.Load[T]`: defaults <
   `chok.yaml` < env < flags. Unknown keys fail startup with a suggestion; only
   `log.level` hot-reloads (SIGHUP); nothing watches files.
2. **Errors describe themselves.** An error implementing `chok.Coder` maps to
   the HTTP envelope (`code / reason / message / request_id / metadata`); there
   is no mapper registry. Wrap with `fmt.Errorf("...: %w", err)`.
3. **Assembly is code.** Dependencies are values: `db.Open` returns a handle
   that `store.New[T](d)` and `identity.New(cfg, d)` take. `app.Use` only
   registers lifecycle (Start / Close / Health / Mount) — no lookups.

## Before every commit: `make verify`

It runs tidy-check, fmt-check (gofmt -s), vet, golangci-lint (blocking),
fix-check (`go fix -diff` empty), test (SQLite, `-race -count=1`), test-pg,
test-mysql, budget and smoke in that order: CI's verify job (its apidiff
job only reports until v3.0.0). All dialect lanes must be green, MySQL too.

### Database lanes

The DSNs live in the git-ignored `.env`, identical to CI; the Makefile never
reads it, so load it per shell (not in `~/.zshrc`):

    set -a; source .env; set +a
    make test-pg test-mysql      # or: make verify

- Postgres: `postgres://chok:chok@localhost:5432/choktest?sslmode=disable` —
  role chok owns choktest; each test works in a throwaway schema.
- MySQL 8.4: `root:root@tcp(localhost:3306)/?parseTime=true` — each test
  creates and drops its own `chok_test_<rand>` database.
- CI provides `postgres:16` and `mysql:8.4` services; the Macs run
  `postgresql@17` and `mysql@8.4` under brew services.
- The lanes rerun every DB-backed package (`DB_PKG_DIRS` in the Makefile)
  in full with `CHOK_TEST_DRIVER` set; never pick tests with `-run`.
  A missing DSN fails its lane; `make test` blanks the driver and both DSNs.

## Docs map (`.private/` is gitignored — never commit it)

| File under `.private/docs/specs/` | Role |
|---|---|
| `v3-plan-claude.md` | the plan: decisions D1–D17, WPs, MySQL rules, budgets, workflow |
| `v3-contract-claude.md` | the API contract, frozen at signature level |
| `v3-prompts-claude.md` | the kick-off prompt of each work package |
| `v3-reports/wp<N>-report.md` | executor close-out reports (plan §7.3 template) |

An executor does one WP on branch `wp<N>-<slug>`, writes its report, pushes
the branch and does not merge. The plan and the contract are never edited by
an executor: a mismatch goes under 契约出入 in the report, and work on it
stops; only points marked 执行者可定 are decided, under 实现级决策.

## Naming and budget (both are gates)

- Functional options carry no `With` prefix and their types are named only
  `Option`, `RouteOption` or `LoadOption`; yaml structs are named `*Config`;
  constructors start with `New`, `MustNew`, `Open` or `MustOpen`.
  `internal/lint/naming_test.go` checks every public package; its exemption
  list holds the frozen contract names that break a rule on purpose, and
  growing it is an orchestrator decision.
- `hack/apibudget.sh`, limits in `hack/budget.txt`: public packages ≤ 12
  (providers excluded), top-level exports ≤ 250 plus per-package caps,
  option constructors ≤ 50 (where predicates excluded), direct deps ≤ 30,
  examples/blog ≤ 120 app lines importing ≤ 5 chok packages. An unlisted
  public package or any count over its limit is red.
- No compatibility shims: no v2 names, no `Deprecated` aliases.

## Code conventions

- New public symbols get godoc; godoc examples compile (`example_test.go`).
- Logging is `*slog.Logger` only; on the request path use `chok.LoggerFrom(ctx)`.
- `*gorm.DB` leaves `db` / `store` only through `Unsafe(ctx)`.
- Shutdown paths derive from `context.WithoutCancel(ctx)`, never
  `context.Background()`, so trace and request IDs stay correlated.
- Code comments are English; design docs are Chinese.
- Commits: `feat:` / `refactor:` / `test:` / `docs:` / `chore:`, one change
  per commit; never `--no-verify`, never force-push.

## Testing

- Every bug fix ships with a regression test in the same commit.
- Real databases, never mocks: `choktest.DB(t)` (WP5) follows `CHOK_TEST_DRIVER`.
- Tests never bind a fixed port (no 8080): use `httptest` / `app.Handler()`,
  or a random port plus `t.Cleanup`. Prefer `testing/synctest` for timing
  logic that does not touch a database.

## Pitfalls seen before

- gofmt turns a bare `''` or `""` pair inside a doc comment into a curly
  quote; describe SQL empty strings or doubled quotes in words instead.
- MySQL reserved words (`key`, `order`, `group`, …) are not quoted by the
  `store/where` identifiers: test models must not use them as column names.
- zsh: `status` is a read-only variable; quote globs such as
  `--include='*.go'`; a word starting with `=` (like `===`) gets expanded.
