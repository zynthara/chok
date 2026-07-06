# Security Policy

## Supported versions

| Line | Module path | Status |
|---|---|---|
| v2 (`main`, current `v2.0.0-beta.N` series) | `github.com/zynthara/chok/v2` | Actively developed — security fixes land here first |
| v1 (sealed at `v0.1.4`) | `github.com/zynthara/chok` | **Security fixes only** |
| Anything older | — | Not supported |

## Reporting a vulnerability

Please **do not open a public issue or pull request** for anything
exploitable — that discloses the bug before a fix exists.

Instead, use GitHub's private vulnerability reporting:

> **<https://github.com/zynthara/chok/security/advisories/new>**
> (repository → Security tab → "Report a vulnerability")

A useful report includes:

- the affected version, tag, or commit
- a minimal reproduction (config + request, or a failing test)
- your assessment of the impact, if you have one

Response times are best-effort — this is currently a single-maintainer
project — but every report is read and acknowledged.

## What happens after a report

When a report is confirmed:

1. A fix is developed with a regression test in the same commit
   (standing project discipline — security bugs are not an exception).
2. A GitHub Security Advisory (GHSA) is published with the affected
   version range, crediting the reporter unless they prefer otherwise.
3. The fix is marked as a security fix in `CHANGELOG.md` and shipped in
   a new release.

## Scope

Everything in this repository: the framework modules, the `chok` CLI,
and the shipped examples. Vulnerabilities in upstream dependencies
should be reported upstream; if it is chok's *usage* of a dependency
that makes it exploitable, report it here as well.
