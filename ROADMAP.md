# Roadmap

Feature and language direction, in priority order. Production/organizational
maturity (load validation, API stability, supply chain, SSO, …) is tracked
separately in [ENTERPRISE.md](ENTERPRISE.md); release history is in
[CHANGELOG.md](CHANGELOG.md).

## Shipped (the foundation)

- Full compiler pipeline for all 8 primitives (`facet`/`feed`/`stream`/
  `lifecycle`/`pipe`/`vault`/`media`/`signal`) with per-primitive runtime
  semantics on the server, the web runtime, **and** both native runtimes
  (FacetKit SwiftUI + Compose).
- Composition (child facets + `slot:`), rich expressions, typed codegen.
- Security suite: scoped SSE, `who:` structural authz, CSRF, rate limits,
  CSP, HMAC-signed events (see `SECURITY_AUDIT.md`).
- App building blocks: router with reload-free navigation, sessions, forms,
  admin panel, Redis broker for multi-instance.
- Tooling: `fct new/dev/build/check/fmt/audit/lsp`, `fatest`, the std library
  (229 facets), VS Code extension, community package registry loop.

## Near term

1. **Named slots** — `slot name:` + `fill name:` (today: one default slot).
2. **Scoped styles** — a `style:` block, auto-scoped per facet.
3. **Computed fields** — derived values in `what:` (the remaining typed-data
   gap).
4. **Client-side `if`/`for` in vault/media bodies** — round 1 is field
   interpolation only.

## Medium term

5. **Hosted registry + docs portal** — the package catalog and this wiki,
   served publicly (see `PUBLISHING.md`). The docs site will be built **with
   FA itself**.
6. **Community facet intake** — curated submissions into `std/` via GitHub
   (see the wiki's Community Packages page).

## Long term

7. **Non-Go backend targets** — codegen for Node / Python / Rust; FA's pitch
   is language-agnostic via the compiler.
8. **Expanded native surface** — richer input kinds and platform components
   over the same neutral-tree protocol.
