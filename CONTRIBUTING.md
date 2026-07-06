# Contributing to chok

Thanks for looking under the hood. This document is the canonical
runbook for building, testing, and releasing chok — written so that a
competent stranger could cut a release without asking anyone.

## Philosophy (read before proposing features)

chok is **config-driven**, with a **single blessed implementation** per
capability, **internally complex / externally trivial**. That third
adjective is a budget: complexity is spent inside the framework so the
app developer doesn't have to. Practical consequences:

- PRs that add a *parallel choice* for an existing capability (a second
  router, a switch to another ORM, an alternative cache backend) will
  be declined by design — see `docs/design.md` for the architecture
  rationale.
- New behaviour should usually be reachable through `chok.yaml`
  configuration, not new Go API surface.

## Prerequisites

- Go — minimum version: see the `go`/`toolchain` directives in
  [`go.mod`](go.mod)
- `make`
- optional: [`golangci-lint`](https://golangci-lint.run) (the `lint`
  target skips gracefully without it), a Postgres for the second test
  lane, `goreleaser` + `syft` for local release dry-runs

## Development loop

```sh
make all       # tidy + lint + test + build
make test      # full unit suite, race detector, no test cache
make test-pg   # store/db against Postgres (needs CHOK_TEST_PG_DSN)
make smoke     # boots examples/blog, waits for /healthz, SIGINTs
make snapshot  # goreleaser dry-run into dist/ (needs goreleaser + syft)
```

The Postgres lane mirrors CI:

```sh
CHOK_TEST_PG_DSN="postgres://chok:chok@localhost:5432/choktest?sslmode=disable" make test-pg
```

## Testing discipline

- **Every bug fix ships with a regression test in the same commit.**
  No exceptions — including security fixes.
- Store/db tests run against a **real database** (`db/dbtest.Open`:
  SQLite by default, Postgres via the env vars above). Don't mock the
  database; mocked tests miss real schema interactions.
- Before pushing: `go test ./... && go vet ./...` must pass, and after
  framework-level changes `make smoke` must boot `examples/blog`
  cleanly.

## Commit style

- Prefix with `fix:` / `feat:` / `docs:` / `refactor:` / `chore:`.
- One coherent change per commit; never batch unrelated fixes.
- Never `--no-verify`, never amend a published commit, never
  force-push `main`.

## Public API and changelog discipline

CI enforces three gates beyond the test suite:

- **apidiff** (`hack/apidiff.sh`): the public API is diffed against the
  release baseline named in `.apidiff-baseline`. An incompatible change
  without a `CHANGELOG.md` update is a red build.
- **Generated surfaces**: `go run ./cmd/chok docs gen --check` and
  `go run ./cmd/chok sync --check ...` fail on drift — regenerate and
  commit rather than hand-editing generated files.
- **govulncheck** (`.github/workflows/govulncheck.yml`): known
  vulnerabilities reachable from chok's code are a red build. It also
  runs weekly against `main` to catch new disclosures on unchanged
  dependencies.

`CHANGELOG.md` keeps an `## [Unreleased]` section at the top — put
user-facing changes there under Added/Changed/Fixed as part of the PR.
`docs/changelog.md` carries the design-level *why* notes, written at
release time.

## Releasing (runbook)

Releases are manual since v2 — release-please was retired because its
commit-counting versioning couldn't express the human-paced
`v2.0.0-beta.N` series (see the header of
`.github/workflows/goreleaser.yml` for the full story).

1. **Green check** — `make test && go vet ./...` locally, CI green on
   `main`.
2. **One release commit**, message `chore(release): vX.Y.Z — <punchline>`:
   - `CHANGELOG.md`: promote `## [Unreleased]` to
     `## [X.Y.Z](compare-URL) (YYYY-MM-DD)`
   - `docs/changelog.md`: add the design-level note (the *why* behind
     the version)
   - `.apidiff-baseline`: bump to `vX.Y.Z`. The apidiff gate reports
     "armed, skipping" until the tag actually exists, then bites on
     every subsequent commit.
3. **Tag and push**:
   ```sh
   git tag vX.Y.Z          # or vX.Y.Z-pre.N
   git push origin main vX.Y.Z
   ```
4. The tag push triggers `.github/workflows/goreleaser.yml`:
   multi-platform binaries (`-trimpath`, `CGO_ENABLED=0`, version
   stamped via ldflags), one SPDX SBOM per archive (syft), and
   `checksums.txt`, published as a GitHub Release with
   CHANGELOG-derived notes. goreleaser runs with `mode: replace`, so
   re-running a release tag from the Actions UI is safe and
   idempotent.
5. **Verify the release assets**: binaries for each platform,
   `*.sbom.json` files, `checksums.txt`.

`make tag` prints the short version of this list.

## Security

Please report vulnerabilities privately — see [SECURITY.md](SECURITY.md).
Public issues describing exploitable bugs disclose them before a fix
exists.
