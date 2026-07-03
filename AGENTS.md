# AGENTS.md - Codex instructions for chok

This file is the repo-level instruction entry point for Codex and other
coding agents. Claude Code has its own loader file, `CLAUDE.md`; treat that
file as shared project context, but follow this file when agent workflow
differs.

## Read First

- Read `CLAUDE.md` before non-trivial changes. It contains the compact
  architecture context and repo-specific invariants.
- `docs/design.md` is the architecture source of truth. Keep it in
  sync when changing public contracts.
- Do not blindly copy Claude-specific workflow advice, such as spawning
  "Explore" agents. Use Codex's own tool and delegation rules.

## What chok Is

chok is a Go web framework that bundles HTTP, DB, cache, auth, scheduler, and
observability behind one config-driven application model. `main` is the v2
line (module path `github.com/zynthara/chok/v2`); v1 is sealed at `v0.1.4`.

The three product constraints are:

- Config-driven behavior.
- One blessed implementation per capability.
- Internally complex, externally trivial APIs.

The blessed stack is stdlib `http.ServeMux`, gorm, otter+redis, robfig-cron,
golang-jwt, casbin, Prometheus, and OpenTelemetry.

## Hard Invariants

- `App` is single-use: `Run` / `Execute` may be called at most once.
- All lifecycle transitions go through the kernel's single control
  goroutine; reads (`Get` / `Health` / `Ready`) use the atomic view.
  Never add locks or a second lifecycle writer.
- Reload coalesces: overlapping triggers get `ErrReloadInProgress`.
  Do not queue reloads.
- Reload does not run migrations. Schema changes require restart and
  documentation.
- Shutdown paths must preserve correlation context. Prefer
  `context.WithoutCancel(parent)` over `context.Background()`.
- Components close in reverse topological order. Same-level components may
  close in parallel.
- Never call `k.Get(peer)` inside `Component.Close`; the kernel view has
  already shrunk by then.

## Component Rules

- Every subsystem is a Component: `Describe() Descriptor` + mandatory
  `Init` / `Close`.
- Optional capabilities are the kernel behavior interfaces —
  `Reloader`, `Healther`, `Mounter`, `Migrator`, `Readier`, `Server`,
  `Drainer` — discovered by type assertion. Add new ones in
  `kernel/component.go` only for a clear, repeated need.
- Declare dependencies in `Descriptor.Needs` (soft edges use
  `Optional: true`); discover peers via `kernel.Get[T]` during `Init`.

## Config Rules

- New `*Options` types implement `conf.Validatable`; discriminator
  types (one of N branches selected by a field) also implement
  `conf.SelfValidating` so the recursive walker stops descending.
- Mark secrets `sensitive:"true"`; mark hot-reloadable fields
  `reload:"hot"` (default is restart-only).
- Avoid pointer fields in `*Options` types.
- Prefer config switches over Go-code switches for feature enablement.
- After changing a module's yaml section shape, regenerate docs and
  schema (`chok docs gen`) — CI fails on drift.

## Store And API Rules

- Use `rid.New(prefix)` for externally exposed IDs. Do not leak internal
  numeric primary keys in API responses.
- Raw gorm has exactly two doors, both named for the risk:
  `Store.Unsafe(ctx)` (tx-aware, scopes applied, fail-closed) and
  `(*db.DB).Unsafe(ctx)` (tx-aware, no scopes). `db.InTx(ctx)` answers
  "am I in a transaction" without the handle.
- Declare `WithQueryFields` / `WithUpdateFields` explicitly for production
  Stores; do not rely on fragile field auto-discovery.
- Use `store.Fields(&obj)` when optimistic locking matters.
- Do not use `store.Where(...)` without at least one filter.
- Wrap errors with `fmt.Errorf("...: %w", err)` or return structured
  `apierr` values.

## Codex Editing Workflow

- Prefer `rg` / `rg --files` for search.
- Use `apply_patch` for manual edits.
- Do not revert user changes in a dirty worktree unless explicitly asked.
- Keep edits scoped to the requested behavior and nearby ownership boundary.
- Follow existing package patterns before introducing new abstractions.
- Public APIs need godoc; quickstart-surface changes get coverage in
  `examples/blog` (advanced demonstrations go to `examples/tasker`
  once it exists).

## Testing

- Every bug fix should include a regression test.
- Run focused tests for the changed package before finishing.
- For broad or public-contract changes, run:

```sh
go test ./...
go vet ./...
```

- The full suite should pass, and `examples/blog` must start cleanly
  after framework-level changes (`make smoke`). The blog acceptance
  test (`examples/blog/blog_test.go`) walks the README quickstart over
  real HTTP. `internal/fixture/m1-m4` remain as milestone regression
  tests, not smoke vehicles.

## Documentation

- Code comments are English.
- Design docs are Chinese by team preference.
- Public docs use plain filenames (no `-claude.md` suffix). Tool-
  protocol files like `CLAUDE.md` and `AGENTS.md` keep their fixed
  names because their loaders require the exact spelling.
- Internal planning / agent SPEC drafts that should not ship to the
  public repo live under `.private/` (gitignored).
- Generated blocks (`<!-- gen:components -->` in the READMEs and
  design.md, `docs/config.md`, `docs/chok.schema.json`) come from
  `chok docs gen` — edit the Descriptor/Options source, not the output.

## Review Reports

When asked to review, use code-review posture:

- Lead with findings, ordered by severity.
- Include exact file and line references.
- Focus on bugs, regressions, security issues, data corruption, resource
  leaks, and missing tests.
- Call out false positives or rejected concerns when that context matters.
- Keep summaries secondary and brief.
