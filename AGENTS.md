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
observability behind one config-driven application model.

The three product constraints are:

- Config-driven behavior.
- One blessed implementation per capability.
- Internally complex, externally trivial APIs.

The blessed stack is gin, gorm, otter+badger+redis, robfig-cron, golang-jwt,
Prometheus, and OpenTelemetry.

## Hard Invariants

- `App` is single-use: `Run` / `Execute` may be called at most once.
- Lock order is always `reloadMu -> mu`; never acquire `mu` before
  `reloadMu`.
- Reload does not run migrations. Schema changes require restart and
  documentation.
- Shutdown paths must preserve correlation context. Prefer
  `context.WithoutCancel(parent)` over `context.Background()`.
- Components close in reverse topological order. Same-level components may
  close in parallel.
- `AddCleanup` callbacks run after `registry.Stop`; they must not access
  components.
- Never call `k.Get(peer)` inside `Component.Close`; peer components may
  already be unavailable.

## Component Rules

- Every subsystem is a Component with mandatory `Init` / `Close`.
- Optional capabilities are narrow interfaces such as `Reloadable`,
  `Healther`, `Router`, `Migratable`, and `ReadyChecker`.
- New Components must declare `Dependencies` / `OptionalDependencies` when
  they read peers during `Init`.
- Avoid new framework-wide optional interfaces unless there is a clear,
  repeated need.

## Config Rules

- New `*Options` types must implement `config.Validatable`.
- Discriminator config types must implement `config.SelfValidating` so the
  recursive validator stops descending into inactive branches.
- Avoid pointer fields in `*Options`; reload copies values onto the live
  config and pointer fields can become stale.
- Prefer config switches over Go-code switches for feature enablement.

## Store And API Rules

- Use `rid.New(prefix)` for externally exposed IDs. Do not leak internal
  numeric primary keys in API responses.
- Do not bypass Store with raw `*gorm.DB` unless intentionally using
  `s.DB()` for unscoped access or `s.ScopedDB(ctx)` for scoped access.
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
- Public APIs need godoc and an example in `examples/blog` when quickstart
  relevant, or `examples/tasker` when advanced.

## Testing

- Every bug fix should include a regression test.
- Run focused tests for the changed package before finishing.
- For broad or public-contract changes, run:

```sh
go test ./...
go vet ./...
```

- The full suite should pass, and `examples/blog` should start cleanly after
  framework-level changes.

## Documentation

- Code comments are English.
- Design docs are Chinese by team preference.
- Public docs use plain filenames (no `-claude.md` suffix). Tool-
  protocol files like `CLAUDE.md` and `AGENTS.md` keep their fixed
  names because their loaders require the exact spelling.
- Internal planning / agent SPEC drafts that should not ship to the
  public repo live under `.private/` (gitignored).
- Keep `examples/blog` quickstart-grade; put advanced demonstrations in
  `examples/tasker`.

## Review Reports

When asked to review, use code-review posture:

- Lead with findings, ordered by severity.
- Include exact file and line references.
- Focus on bugs, regressions, security issues, data corruption, resource
  leaks, and missing tests.
- Call out false positives or rejected concerns when that context matters.
- Keep summaries secondary and brief.
