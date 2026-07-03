# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

This file is maintained automatically by
[release-please](https://github.com/googleapis/release-please) once the
release-PR workflow is in motion. For design-level release notes (the
*why* behind each version), see [`docs/changelog.md`](docs/changelog.md).

## [0.2.0](https://github.com/zynthara/chok/compare/v0.1.4...v0.2.0) (2026-07-03)


### ⚠ BREAKING CHANGES

* M4 purge — parts/ and component/ deleted, gin and badger out of go.mod
* replace the gin web stack with stdlib implementations
* replace the v1 App face with the v2 thin shell

### Features

* **apierr:** port RenderHook mechanism from toffs ([8a19586](https://github.com/zynthara/chok/commit/8a19586a52703bf5230f42addbeb21f4c6c4b756))
* **audit:** v2 audit.Module — purge cron, admin API, dead config zeroed ([a08709c](https://github.com/zynthara/chok/commit/a08709c75342d33070263d1f9b860a64ffae1a45))
* **authz:** v2 authz.Module — casbin_rule schema in Migrate, 7.E audit truth table wired ([4f273cd](https://github.com/zynthara/chok/commit/4f273cd9ca25ce7eb311e80474ea798020f8b8b4))
* **cache:** v2 cache.Module — memory+redis Chain with Breaker, badger layer removed ([2c3fcc6](https://github.com/zynthara/chok/commit/2c3fcc66d043e68a7c7db64932e063874637d90b))
* **cmd:** chok migrate create/up/status subcommand group ([ad26e7c](https://github.com/zynthara/chok/commit/ad26e7cd4d6627234f8f4b96783df2d2ac689af5))
* **conf:** immutable config RCU — loader, snapshots, reload tag diff ([deeebdf](https://github.com/zynthara/chok/commit/deeebdfdad48ca1fde2dda9644b2526740e07387))
* **conf:** sensitive-tag redaction — §12.9 first piece ([908d673](https://github.com/zynthara/chok/commit/908d6733670e9f25fea90ff62214bde5a7e31cfd))
* **db:** v2 data-layer foundation — thin handle, Postgres, MySQL TLS, commit staging ([4ffd4d6](https://github.com/zynthara/chok/commit/4ffd4d692a6d3d493a7ab91c53eaaffe72b42774))
* **db:** versioned migration engine — ledger, three-branch lock, splitter; typed PG duplicate mapping ([5b874a9](https://github.com/zynthara/chok/commit/5b874a9335f74901f3699af16e27c96553d5a809))
* **kernel/event:** typed async event bus ([e6c7eb4](https://github.com/zynthara/chok/commit/e6c7eb456d6319c99d175ccd4050bd510da178f7))
* **kernel:** component contract + single-actor control plane ([b61a617](https://github.com/zynthara/chok/commit/b61a617c806770aca55102beb65f21bfc00ef59e))
* M1 fixture app + transition doc alignment ([648bcb3](https://github.com/zynthara/chok/commit/648bcb3fff647ee6b0f4603cb9a171d335929be9))
* M2 fixture app + transition doc alignment ([f1ad4ec](https://github.com/zynthara/chok/commit/f1ad4ec7af6e698948551e7cac39a79fcea94041))
* M3 fixture app + Postgres CI lane + transition doc alignment ([683a153](https://github.com/zynthara/chok/commit/683a1534107f70939e5f167b1697c5d974831620))
* M4 fixture app — the full-battery assembly, acceptance-tested end to end ([8e3fd11](https://github.com/zynthara/chok/commit/8e3fd116e2cb0dbc1ff1067da3ffbecf1c9e76b5))
* M4 purge — parts/ and component/ deleted, gin and badger out of go.mod ([d8bdfa3](https://github.com/zynthara/chok/commit/d8bdfa3816bcb02844aa0614ad394b1d1ba31988))
* migrate log/health/metrics/debug to v2 modules ([cbebcf3](https://github.com/zynthara/chok/commit/cbebcf3435c04cf316a023722cf85326002b2499))
* **redis:** v2 redis.Module — TLS/username/ca_cert back-port, TLSConfigFor export ([4ae7735](https://github.com/zynthara/chok/commit/4ae7735a83886572376fb2a07103935d6b22c778))
* replace the gin web stack with stdlib implementations ([8d9e4df](https://github.com/zynthara/chok/commit/8d9e4df0f2b9126f215963de05eec6dff492f31d))
* replace the v1 App face with the v2 thin shell ([f4704ce](https://github.com/zynthara/chok/commit/f4704ce4c575dd8bbdac39a9ecd500ca229d79e4))
* **scheduler:** v2 scheduler.Module — kernel.Server behaviour, stop_budget, peer Register API ([e24f7d9](https://github.com/zynthara/chok/commit/e24f7d91ab1e029ab494b6fe3625a3cc851bb4e5))
* swagger route-table module + standalone tracing module ([d90ff73](https://github.com/zynthara/chok/commit/d90ff7386be84ca6b342f356d2d9d57856f18ed7))
* **web:** ClientIP/TrustedProxies resolver + route-pattern carrier ([4ab5e7b](https://github.com/zynthara/chok/commit/4ab5e7b25cbdf558570228b95458b0317d284133))
* **web:** stdlib Server + Router + Module — first real kernel-contract consumer ([c10a14b](https://github.com/zynthara/chok/commit/c10a14b28784e72a64a6987f223eba4798555688))


### Code Refactoring

* **db:** move the gorm logger adapter out of the log package ([110d3db](https://github.com/zynthara/chok/commit/110d3dbf894fce09233b309bc3d5c6b3cea7d3bb))


### Documentation

* mark v2 development status; move agent discipline to fixture wording ([0322948](https://github.com/zynthara/chok/commit/03229488ef7b08e46fd3fa6f16b4374a817a3704))

## [0.1.4](https://github.com/zynthara/chok/compare/v0.1.3...v0.1.4) (2026-07-03)

> v1 feature-freeze release. After this tag the v0.1.x line receives
> security fixes only; `main` moves to the v2 rewrite
> (`github.com/zynthara/chok/v2`). See
> [`docs/changelog.md`](docs/changelog.md) for the design notes.

### Features

* **account:** split userStore vs publicStore + add admin APIs + AuthChain ([82e1751](https://github.com/zynthara/chok/commit/82e17519b2b4db8618d44064b6beb522405be0f8))
* **account:** Phase 2 OAuth abstraction layer + fake-provider e2e ([9fb5de6](https://github.com/zynthara/chok/commit/9fb5de67b2d626b95665eafcf48220171f71e316))
* **account:** Phase 3 — config-driven OAuth provider assembly ([bc14b35](https://github.com/zynthara/chok/commit/bc14b35052ae1655e66e2f9371a92a99aafad5fd))
* **account:** bundle blessed OAuth providers by default ([26231fe](https://github.com/zynthara/chok/commit/26231fe9fb58849c1c167831ee49c32e644faebd))
* **account:** Phase 4 — Google OAuth provider ([818a3f6](https://github.com/zynthara/chok/commit/818a3f6a6b2761a08608f25ae1dff57febba1af2))
* **account:** Phase 5a — GitHub OAuth provider ([156e4e2](https://github.com/zynthara/chok/commit/156e4e228039827bff570d5e358e0b4ce995e662))
* **account:** Phase 5b — Facebook OAuth provider ([2ed822f](https://github.com/zynthara/chok/commit/2ed822fbeb23e708ebc40ecd29d7fb055bc3ff3c))
* **account:** Phase 5c — Apple Sign-In provider ([25b870a](https://github.com/zynthara/chok/commit/25b870a40710b5baa2be6c643a08fc87de809ddd))
* **audit:** Phase 7.A — base types + config.AuditOptions ([e63ebf6](https://github.com/zynthara/chok/commit/e63ebf6fdaa2b1ad061f519ad779d71a699d48de))
* **audit:** Phase 7.B — async DB sink + parts.AuditComponent ([1bbc812](https://github.com/zynthara/chok/commit/1bbc8123c4b047a226ea1dd26b6dd7a3462ec07c))
* **authz:** Phase 6 — Casbin authorizer + multi-tenant middleware ([d6cd7ca](https://github.com/zynthara/chok/commit/d6cd7ca4bb1f86568c2d55b231a8ef786333b509))
* **authz/casbin:** Phase 6 follow-up — Redis Watcher ([14e3cf3](https://github.com/zynthara/chok/commit/14e3cf36f49ae79addea3a6ac0b942b5b077159a))


### Bug Fixes

* **examples/blog:** race-free startup barrier in NewTestRouter ([19086e6](https://github.com/zynthara/chok/commit/19086e6d9cf327ee6510ddb9792c1a8d21d2c71d))
* round-1 review — Phase 1 account hardening ([244d1f1](https://github.com/zynthara/chok/commit/244d1f1b670ad15cff5df26afb980318d9ce2ea2))
* round-2 review — Phase 2 OAuth hardening ([4ec383f](https://github.com/zynthara/chok/commit/4ec383f3261027628a99ebb0517e61cc35b1791f))
* round-3 review — Phase 3 hardening ([1539093](https://github.com/zynthara/chok/commit/15390932dba6d5009684719a828f56f637f335bb))
* **authz/casbin:** self-shipped GORM adapter, drop gorm-adapter v3 ([2ebbf88](https://github.com/zynthara/chok/commit/2ebbf886e6d71e20b8dfeac3d61462ab2a7f4f6b))
* **authz/casbin:** round-1 review — adapter wire-compat + bootstrap batch ([8b64d6b](https://github.com/zynthara/chok/commit/8b64d6b5730cc7bc3e413e9e6ee52b13bb5b3b7d))
* **authz/casbin:** round-2 review — UpdatableAdapter + LoadPolicy CSV-safe ([d4f7fc7](https://github.com/zynthara/chok/commit/d4f7fc7226cfe21df20dcdab3e78ebab5a2acafe))
* **authz/casbin:** round-3 review — Update* exact-rule + guard mapped + LoadPolicy fail-fast ([036b7d2](https://github.com/zynthara/chok/commit/036b7d27432a91b07c958a861f9873196654ca6e))
* **authz/casbin:** round-1 review — Watcher contract hardening ([1b7da18](https://github.com/zynthara/chok/commit/1b7da1854515ab06820532bdd94e6c943568ce60))

## [0.1.3](https://github.com/zynthara/chok/compare/v0.1.2...v0.1.3) (2026-04-21)


### Features

* **account:** expose User Store + ActiveCheck verifies pv ([0d40a96](https://github.com/zynthara/chok/commit/0d40a96e4ea3e377d0a1a4941417f0059a7e7109))
* **account:** 可选关闭公开注册 ([93c9ab7](https://github.com/zynthara/chok/commit/93c9ab7cb87ff3c61a37a1056705a6410822c3e8))
* **config:** auto-register discovers Options in nested structs ([be9f979](https://github.com/zynthara/chok/commit/be9f979b2ecf7e407bb1a6cd40faf29240c10e72))


### Code Refactoring

* **apierr:** use per-App WithErrorMapper in scaffold and example ([77c71de](https://github.com/zynthara/chok/commit/77c71de3268c37aa9b96f2786e46946eb7f88108))

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
