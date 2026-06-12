# Enterprise Readiness — status & remaining work

The single tracker for "can a company bet a product on FA?" Each item is either
**✅ shipped** (verified in this repo, with where), or **⬜ open** (with what
"done" means). The README roadmap tracks *language/DX* gaps; this file tracks
*production/organizational* gaps. Update it in the same PR that closes an item.

Last updated: 2026-06-11 (v0.13.0).

## Scorecard

| Area | State |
|---|---|
| Security (app-level) | ✅ shipped — full suite, audited (`SECURITY_AUDIT.md`); disclosure policy in `SECURITY.md` |
| Horizontal scale-out | ✅ shipped (Redis broker, shared key) — ⬜ unvalidated under real load |
| Operations | ✅ shipped (health, drain, Docker, logs) |
| Observability | ✅ shipped — Prometheus `/metrics` (counters + latency histograms), `fa.Tracer` OTel hooks traceable across the broker |
| Data / persistence | ✅ documented (BYO database — see `wiki/Working-with-Databases.md`) |
| AuthN / SSO | 🟡 partial — sessions + identity built in; no OIDC/SAML story |
| API stability | ✅ shipped — `STABILITY.md` (semver, 1.0 surface freeze, deprecation policy) + wire-version negotiation in all three runtimes |
| Supply chain | ✅ shipped — cosign-signed releases, SLSA provenance, SBOM, govulncheck + dependency review in CI, reproducible builds (`docs/REPRODUCIBLE_BUILDS.md`) |
| Native client parity | ✅ shipped — wire, HMAC, surgical updates, and per-primitive semantics (window/ttl/vault/media) in both runtimes, unit-tested |
| Docs | ✅ shipped — user wiki (`wiki/`), guide, ADRs, security audit |
| Accessibility / i18n | ⬜ open |
| Support / governance | ✅ shipped — `GOVERNANCE.md` + disclosure/support policy in `SECURITY.md` |

---

## ✅ Shipped (don't re-litigate; verified in code)

- **Security suite** — scoped SSE delivery (no leak by default, deny-by-default
  `channel_auth`), `who:` structural authz (`Guard` + `RenderFor`, fail-closed
  `redact`), CSRF (conn-id + Origin), per-IP rate limit + SSE cap, CSP/secure
  headers, HMAC-signed events verified on web **and** native, compile-time
  cycle/unknown-child/unknown-prop rejection. Audit + status: `SECURITY_AUDIT.md`.
- **Multi-instance** — pluggable `Broker` with a built-in zero-dependency Redis
  adapter (`fa/redisbroker.go`), stable shared signing key (`FA_SIGNING_KEY`),
  sticky-LB deployment shape documented (README + `wiki/Deployment.md`).
- **Operations** — `/healthz`, `/readyz` (drains on `Shutdown`), graceful
  SIGTERM handling in the scaffold, structured request logs (`fa.LogRequests`),
  distroless static Dockerfile shipped by `fct new`.
- **App building blocks** — router (SPA-style nav, SSE survives page changes),
  signed-cookie sessions + `Identify`, form validation + uploads, auth-gated
  admin panel with live metrics.
- **DX / tooling** — typed codegen, `fct check/fmt/lsp/audit`, `fatest`, std
  (229 facets, responsive), VS Code extension, package registry loop.
- **Performance baseline** — microbenchmarks in-repo (`go test ./fa -bench .`):
  ~17 µs/render, ~18 µs/dispatch, ~200 µs signed fan-out to 1k subscribers.
- **User documentation** — the `wiki/` (getting started → first website →
  databases → deployment → testing → native), mkdocs site config.

---

## ⬜ Remaining (priority order)

### P0 — blockers for calling FA "enterprise ready"

1. **Load validation under real concurrency.** The benchmarks are per-op and
   in-process. Done = k6/vegeta against a deployed multi-instance setup
   (Redis broker, sticky LB), published numbers for: concurrent SSE
   connections per instance, event dispatch p99 under fan-out, memory per
   connection, broker saturation point, and reconnect-storm behavior after a
   deploy. The README performance table gains a "validated at scale" section.
- ✅ **API stability commitment** *(v0.14.0)* — `STABILITY.md` defines semver,
  the 1.0 surface freeze (`fa`, `std`, `fatest`, the manifest schema, the SSE
  wire format), and the deprecation policy (≥ one full minor of `Deprecated:`
  warning before removal; post-1.0 removal only at a major). Wire-format
  version negotiation shipped in all three runtimes: clients declare their
  version at connect (`FA-Wire-Version` header; `?v=` for EventSource) and a
  mismatch is a loud **426** at the handshake; the `_conn` hello frame carries
  the server's version (`fa.WireVersion`) and every runtime fails fatal — no
  reconnect loop, no garbage rendering — on mismatch (`fa/wire.go`,
  `runtime/fa-runtime.js`, `FacetClient.swift`/`.kt` `wireError`; test
  `fa/wire_test.go`).
- ✅ **Native primitive parity** *(v0.13.0)* — `window:` trimming, signal
  `ttl:` apply/revert, vault AES-GCM decrypt (device-held key), and media
  mounting enforced in both FacetKit (SwiftUI) and the Compose client, from
  the same manifest registry as the web runtime, with unit tests
  (`clients/swift`, `clients/android`).
- ✅ **Observability in standard formats** *(v0.14.0)* — `GET /metrics` serves
  Prometheus exposition format, dependency-free (events in/out, conns
  active/total, rate-limited, forbidden, plus `fa_dispatch_duration_seconds`
  and `fa_fanout_duration_seconds` histograms); `/debug/metrics` JSON kept for
  back-compat. `fa.Tracer` + `fa.WithTracer` open spans around
  `fa.dispatch` → `fa.emit` → `fa.deliver`, with the emitter's trace context
  carried inside the broker message (Inject/Extract — one call each on an OTel
  W3C propagator), so a request is traceable across instances. OTel wiring
  example in `wiki/Deployment.md`; tests `fa/observe_test.go`,
  `fa/trace_test.go`.
- ✅ **Supply-chain hardening** *(v0.14.0)* — the release workflow now ships,
  per release: keyless **cosign** signature over `checksums.txt` (Sigstore,
  Rekor-logged), **SLSA build provenance** as GitHub artifact attestations
  (`gh attestation verify`), and an **SPDX SBOM**; builds are reproducible
  (`-trimpath -buildid= -buildvcs=false`, CGO off, zero deps). `ci.yml` runs
  **govulncheck** on every push/PR plus dependency review on PRs; every
  third-party action is pinned to a commit SHA. Recipe + the three
  verification commands: `docs/REPRODUCIBLE_BUILDS.md`.

### P1 — expected by enterprise evaluators

6. **SSO / OIDC guidance or adapter.** Sessions + `Identify` exist, but
   "verify credentials however you like" doesn't pass a security review.
   Done = a documented OIDC login flow (stdlib-only or one blessed library),
   password-hashing guidance (argon2id) in the wiki, and a worked example
   wiring an IdP identity into `Identify`/`who:` policies.
7. **Signing-key rotation.** `FA_SIGNING_KEY` has no rotation story; changing
   it breaks every open page until reload. Done = dual-key verification
   window (`FA_SIGNING_KEY_PREVIOUS`) so keys rotate with zero downtime,
   documented in `wiki/Deployment.md`.
8. **Named slots + scoped styles.** The two language gaps that block large
   design systems (README roadmap #1–2). Enterprise apps hit both in week one.
9. **Accessibility pass on `std`.** 229 facets, no stated a11y contract.
   Done = ARIA roles/labels on interactive stdlib facets, focus preservation
   across fragment swaps in the runtime (focus dies on `replace` today unless
   proven otherwise), `prefers-reduced-motion` respected, axe-core run on the
   demo recorded in CI.
- ✅ **CVE / disclosure process + support policy** — `SECURITY.md` is now the
  policy: private reporting (GitHub advisory or email), response timelines,
  coordinated disclosure, and a stated supported-versions window (latest
  minor, pre-1.0). The audit moved to `SECURITY_AUDIT.md`.

### P2 — competitive completeness

11. **i18n/l10n story** — message catalogs in FDL (`{t "key"}` or similar),
    locale-aware rendering; today it's "interpolate your own strings."
12. **Hosted registry + public docs site** — `PUBLISHING.md` has the plan;
    the wiki + mkdocs config are ready to deploy to GitHub Pages.
13. **Non-Go backend targets** (Node/Python/Rust codegen) — the
    language-agnostic pitch, README roadmap #6.
14. **Multi-region / DR guidance** — broker topology across regions, what
    happens to SSE clients on regional failover.

---

*How to use this file:* pick the lowest-numbered open item, ship it with tests
and docs, flip it to ✅ with a pointer to the code, and update the scorecard.
