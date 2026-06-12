# Enterprise Readiness — status & remaining work

The single tracker for "can a company bet a product on FA?" Each item is either
**✅ shipped** (verified in this repo, with where), or **⬜ open** (with what
"done" means). The README roadmap tracks *language/DX* gaps; this file tracks
*production/organizational* gaps. Update it in the same PR that closes an item.

Last updated: 2026-06-11 (v0.12.0).

## Scorecard

| Area | State |
|---|---|
| Security (app-level) | ✅ shipped — full suite, audited (`SECURITY.md`) |
| Horizontal scale-out | ✅ shipped (Redis broker, shared key) — ⬜ unvalidated under real load |
| Operations | ✅ shipped (health, drain, Docker, logs) |
| Observability | 🟡 partial — JSON metrics only; no Prometheus/OTel |
| Data / persistence | ✅ documented (BYO database — see `wiki/Working-with-Databases.md`) |
| AuthN / SSO | 🟡 partial — sessions + identity built in; no OIDC/SAML story |
| API stability | ⬜ open — pre-1.0, minor versions may break |
| Supply chain | ⬜ open — no signed releases / SBOM / vuln scanning in CI |
| Native client parity | 🟡 partial — wire + HMAC + surgical updates done; primitive semantics staged |
| Docs | ✅ shipped — user wiki (`wiki/`), guide, ADRs, security audit |
| Accessibility / i18n | ⬜ open |
| Support / governance | 🟡 partial — `GOVERNANCE.md` exists; no LTS or CVE process |

---

## ✅ Shipped (don't re-litigate; verified in code)

- **Security suite** — scoped SSE delivery (no leak by default, deny-by-default
  `channel_auth`), `who:` structural authz (`Guard` + `RenderFor`, fail-closed
  `redact`), CSRF (conn-id + Origin), per-IP rate limit + SSE cap, CSP/secure
  headers, HMAC-signed events verified on web **and** native, compile-time
  cycle/unknown-child/unknown-prop rejection. Audit + status: `SECURITY.md`.
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
2. **API stability commitment.** Today "pre-1.0: minor versions may break"
   (CHANGELOG). Done = a 1.0 surface freeze for `fa`, `std`, `fatest`, the
   manifest schema, and the SSE wire format; semver + a written deprecation
   policy (one minor of warning before removal); wire-format version
   negotiation so old native clients fail loud, not weird.
3. **Native primitive parity.** FacetKit (SwiftUI) and Compose share the
   signed SSE wire and surgical updates, but `window:` trimming, signal
   `ttl:` revert, vault decrypt, and media mounting are web-only. Done =
   per-primitive enforcement in both native runtimes with unit tests (the
   staged "next round" from v0.12.0; modified files are already on this branch).
4. **Observability in standard formats.** `/debug/metrics` is bespoke JSON.
   Done = Prometheus exposition format on `/metrics` (counters it already
   tracks: events in/out, conns, rate-limited, forbidden — plus dispatch
   latency histograms), and OpenTelemetry trace hooks around
   dispatch→render→emit so a request is traceable across the broker.
5. **Supply-chain hardening.** Done = signed release artifacts (cosign) +
   SLSA provenance in the release workflow, SBOM per release, `govulncheck`
   + dependency scanning in CI, and a pinned reproducible-build doc. (The
   framework is dependency-free, which makes this cheap — do it while that's
   still true.)

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
10. **CVE / disclosure process + support policy.** `SECURITY.md` is an audit,
    not a policy. Done = a security-reporting contact + embargo process, a
    stated supported-versions window, and an LTS statement (even if it's
    "latest minor only" — say it).

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
