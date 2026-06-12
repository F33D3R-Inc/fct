# API Stability & Versioning Policy

This is FA's compatibility contract: what is frozen, what semver means here,
how deprecations work, and how the wire protocol negotiates versions.

## Versioning

FA follows [semver](https://semver.org). Concretely:

- **Patch (x.y.Z)** — bug fixes only. Always safe to upgrade.
- **Minor (x.Y.0)** — additive features and deprecation *warnings*. Existing
  code keeps compiling and behaving the same.
- **Major (X.0.0)** — the only place a frozen surface may break, with a
  migration guide in the CHANGELOG.

**Pre-1.0 (current):** breaking changes may still land, but only at minor
bumps, never at patch bumps, and always called out in the CHANGELOG with
migration notes. The surfaces below are the freeze *candidates* — they harden
into the contract at v1.0.0.

## The frozen surface (as of 1.0)

| Surface | What freezes |
|---|---|
| `fa` (Go) | every exported identifier: `App`, `Ctx`, `Event`, `Hub`, `Broker`, `Tracer`, `Metrics`, options, signatures and documented behavior |
| `std` | facet names, their props, and their documented semantics (rendered markup may improve; props/names never break) |
| `fatest` | every exported identifier |
| Manifest schema | the JSON shape served at `/manifest.json` and consumed by the web/native runtimes: existing fields never change meaning or disappear; new fields are additive (consumers must ignore unknown fields — all three runtimes do) |
| SSE wire format | the Event frame (`op`, `facet_id`, `fragment`, `hmac`), the hello frame (`_conn`, `conn`, `key`, `v`), the HMAC layout (`op\0facet_id\0fragment`), and the `/events` request body (`type`, `payload`, `conn`) |

Anything unexported, anything under `internal/`, the `fct` CLI's *output text*,
and the bespoke JSON at `/debug/metrics` are **not** part of the contract.
(`/metrics` Prometheus names *are*: metric names and types won't break.)

## Deprecation policy

Nothing frozen is ever removed or changed without **at least one full minor
version of warning**:

1. Version `x.Y` deprecates the API: the doc comment gains `Deprecated:` (so
   `go vet`/staticcheck and editors flag it), the CHANGELOG names the
   replacement, and the old API keeps working unchanged.
2. Version `x.(Y+1)` or later — and post-1.0, only the next **major** — may
   remove it. Removal never lands in the same release as the deprecation.

## Wire-format version negotiation

The SSE wire format is versioned independently of the library
(`fa.WireVersion`, currently `"1"`), and both directions fail **loud**:

- **Client declares its version at connect** — native clients send
  `FA-Wire-Version: 1`; the web runtime appends `?v=1` (EventSource cannot set
  headers). A server that speaks a different version rejects the connect with
  **426 Upgrade Required** plus an explicit message and its own version in the
  `FA-Wire-Version` response header. An outdated native app fails at the
  handshake with a clear error — it never half-renders garbage. Clients that
  predate negotiation (no header, no `?v=`) are treated as v1.
- **Server announces its version in the hello frame** — the first SSE frame
  (`op: "_conn"`) carries `"v"`. Every runtime (web, FacetKit, Compose) checks
  it on each (re)connect and surfaces a fatal, human-readable error on
  mismatch (`wireError` on native clients; a console error on web), stopping
  the reconnect loop.

`fa.WireVersion` only bumps with a breaking wire change, which is a **major**
release post-1.0.

## Supported versions

Security fixes target the latest minor release (see `SECURITY.md`). Post-1.0,
the latest minor of the current major is supported; the previous major
receives security fixes for 6 months after a new major ships.
