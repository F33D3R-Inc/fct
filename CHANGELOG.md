# Changelog

All notable changes. Pre-1.0: minor versions may break.

## v0.10.1

### Fixed
- **`who: redact` now applies to typed structs** (and pointers/slices), not just
  `map[string]any`. A declared redaction that names a non-existent field now
  **fails closed** (render errors) instead of silently leaking the field.
- **Release binaries report the real version.** `fct version` now prints the tag
  (e.g. `v0.10.1`) — release builds stamp it via `-ldflags -X main.version`,
  instead of the hardcoded `0.0.0-walking-skeleton`.
- Aligned the Go toolchain version across the repo: README and the release
  workflow now state **Go 1.26** (matching `go.mod`, the scaffold, and the
  Dockerfile).

## [Unreleased] — v0.1.0 (walking skeleton → usable)

### Language & compiler
- FDL `facet` primitive: `who` / `what` / `looks` / `when`.
- **Composition**: child facets `<Avatar/>` and `slot:` (block-form fill).
- **Rich expressions**: paths, method calls, comparisons, boolean, arithmetic,
  literals — lowered to Go-template pipelines.
- Idiomatic Go naming (`avatar_url` → `AvatarURL`); `fct build` emits typed
  `<Facet>Data` structs.
- Compile-time guards: composition cycles / unknown child / unknown prop.

### Server runtime (`fa`)
- Scoped SSE delivery (`EmitConn`/`EmitTo`/`EmitChannel`/`Broadcast`), **no leak
  by default**; deny-by-default `channel_auth`.
- **Multi-instance** via a pluggable `Broker` + a stable shared signing key
  (`FA_SIGNING_KEY` / `WithSigningKey`).
- Structural authz: `App.Guard` (events) + `who:` enforced at render (`RenderFor`).
- CSRF (conn-id + Origin), per-IP rate limit + connection cap, secure headers/CSP,
  HMAC-signed events.
- Observability: `/healthz`, `/readyz`, `/debug/metrics`, `fa.LogRequests`.
- Graceful shutdown (`app.Shutdown`).

### Tooling & ecosystem
- CLI: `new`, `dev`, `build`, **`check`**, **`fmt`**, **`add`**, `audit`, **`lsp`**.
- `github.com/F33D3R-Inc/fct/fatest` — unit-test facets and handlers.
- `github.com/F33D3R-Inc/fct/std` — standard library (44 facets) + default theme.
- VS Code extension (highlighting + LSP diagnostics).

### Docs
- README (source of truth), GUIDE, SECURITY (audit + roadmap), DECISIONS (ADRs),
  REACT_MIGRATION, CONTRIBUTING, GOVERNANCE.
