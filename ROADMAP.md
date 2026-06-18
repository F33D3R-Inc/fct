# Roadmap

Feature and language direction, in priority order. Production/organizational
maturity (load validation, API stability, supply chain, SSO, …) is tracked
separately in [ENTERPRISE.md](ENTERPRISE.md); release history is in
[CHANGELOG.md](CHANGELOG.md).

## Shipped (the foundation)

- Full compiler pipeline for all 8 primitives (`facet`/`feed`/`stream`/
  `lifecycle`/`pipe`/`vault`/`media`/`signal`) with per-primitive runtime
  semantics on the server, the web runtime, **and** both native runtimes
  (FacetKit SwiftUI + Compose). Client-rendered bodies (`decrypt:`/`source:`)
  support `{field}` interpolation **plus `{if}`/`{for}`** across all runtimes.
- Composition (child facets, default + **named slots** via `slot name:` /
  `fill name:`), rich expressions, **computed `what:` fields**,
  **cross-platform `style:` tokens**, typed codegen.
- Security suite: scoped SSE, `who:` structural authz, CSRF, rate limits,
  CSP, HMAC-signed events (see `SECURITY_AUDIT.md`).
- App building blocks: router with reload-free navigation, sessions, forms,
  admin panel, Redis broker for multi-instance.
- Tooling: `fct new/dev/build/check/fmt/audit/lsp`, `fatest`, the std library
  (229 facets), VS Code extension, community package registry loop.

## Near term

1. **Web-only style escape hatch** — a separate, explicitly web-scoped block
   for `:hover` / `@media` / animations the cross-platform `style:` tokens
   can't express (keyword reserved; build when a facet needs it). See ADR-0008.

## Medium term

2. **Hosted registry + docs portal** — the package catalog and this wiki,
   served publicly (see `PUBLISHING.md`). The docs site will be built **with
   FA itself**.
3. **Community facet intake** — curated submissions into `std/` via GitHub
   (see the wiki's Community Packages page).

## Long term

4. **Non-Go backend targets** — codegen for Node / Python / Rust; FA's pitch
   is language-agnostic via the compiler. *Compiler track shipped* (pluggable
   `codegen.Backend`, `fct.toml [compiler] target`, typed data + expression
   lowering for all three — DECISIONS.md ADR-0010, docs/BACKENDS.md); the
   per-language server runtimes are the remaining track.
5. **Expanded native surface** — richer input kinds and platform components
   over the same neutral-tree protocol.
