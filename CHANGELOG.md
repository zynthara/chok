# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

This file is maintained automatically by
[release-please](https://github.com/googleapis/release-please) once the
release-PR workflow is in motion. For design-level release notes (the
*why* behind each version), see [`docs/changelog.md`](docs/changelog.md).

## [0.1.2](https://github.com/zynthara/chok/compare/v0.1.1...v0.1.2) (2026-04-19)


### Bug Fixes

* **release:** ignore vcs.modified for ldflags-injected builds + inline goreleaser ([971bdcb](https://github.com/zynthara/chok/commit/971bdcb49e455b0152616689856cd5e5f5f9c4ec))

## [0.1.1](https://github.com/zynthara/chok/compare/v0.1.0...v0.1.1) (2026-04-19)


### Bug Fixes

* **release:** drop redundant go mod tidy from goreleaser before-hooks ([7351568](https://github.com/zynthara/chok/commit/7351568a38e651caf2cceb8564cd604d8cc4b739))

## [0.1.0] - 2026-04-19

### Added

- Initial public release of chok. Configuration-driven Go web framework
  bundling HTTP, database, cache, JWT auth, scheduler, and observability
  behind a single `chok.yaml` + `Config` struct.
- 15 built-in Components (log / http / db / redis / cache / account /
  swagger / health / metrics / debug / tracing / scheduler / pool /
  jwt / authz).
- Generic Store API with Locator + Changes, optimistic locking, owner
  scope, cursor pagination, and identifier validation.
- `chok` CLI: `init`, `version`, `update`.
- Release pipeline: release-please + goreleaser via GitHub Actions.

See [`docs/changelog.md`](docs/changelog.md) for the full feature list
and the contract guarantees that ship with 0.1.0.
