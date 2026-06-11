# Changelog

All notable changes. Pre-1.0: minor versions may break.

## v0.12.0

### Primitive runtime semantics (server + web runtime)

The taxonomy that compiled in v0.11.0 now *behaves*:

- **feed** — `Compiled.SortFeed(facet, items)` orders a slice (structs, struct
  pointers, or maps) by the declared `order:` field in place. Bare field =
  descending (ranked list); `asc` flips. Snake_case fields resolve idiomatic Go
  struct names (`created_at` → `CreatedAt`). Fails closed: a missing field or
  non-comparable type errors and leaves the slice untouched.
- **stream / pipe `throttle:`** — enforced in the hub, per (scope, target,
  facet instance), at the emitting instance. Trailing-edge coalescing: first
  frame immediate, frames inside the interval replace each other, the latest
  flushes when it elapses. The final state is always delivered.
- **stream `window:`** — the web runtime trims the container after
  `append`/`prepend` (from the opposite end), capping DOM growth.
- **lifecycle `states:`** — `Compiled.Lifecycle(facet)` returns the validated
  machine (`Initial`/`Valid`/`Next`/`CanTransition`, forward-by-one).
- **signal** — `App.Signal` / `Ctx.Signal` relay a signed `signal` event
  (payload JSON as the fragment) to channel subscribers; nothing is stored.
  Fails closed unless the target is a declared `signal`. The web runtime applies
  the payload to `[data-fa-signal]` elements as `data-*` attributes +
  `.fa-signal-live`, and reverts after the declared `ttl:`.
- **vault** — client-side decrypt in the web runtime: `fa.vault.key(name,
  hexKey)` registers an AES-GCM key (never sent to the server); the runtime
  decrypts each `[data-fa-vault]` element's `data-fa-envelope` (base64 IV‖CT)
  and renders the manifest's `decrypt:` body with `{plaintext}` (JSON plaintext
  exposes fields), HTML-escaped. Round 1 supports field interpolation (no
  client-side if/for).
- **media** — the web runtime mounts the player from the `source:` body inside
  `[data-fa-media]` elements, filling `{field}` holes from `data-*` attributes;
  `<hls>`/`<dash>` normalize to `<video controls>`.

### Compiler / library plumbing

- `Compile` now type-checks the per-kind extras: a malformed `throttle:` /
  `ttl:` (non-Go-duration) or `window:` (non-positive-int) is a **compile
  error**, not a silent runtime no-op.
- The manifest's primitive rules are parsed once into typed runtime metadata
  shared by `Compiled` and `App`; the client runtime builds the same registry
  from `/manifest.json` (no wire-format change, nothing new to sign).
- The hub's native transform passes `signal` events through untouched (their
  fragment is the JSON payload, not HTML).
- `codegen.GoName` (the snake_case → Go name mapping) is now exported for reuse.

Native runtimes (FacetKit / Compose) get window/TTL/vault/media enforcement in
the next round; they already receive signal events over the same signed wire.

## v0.11.0

### Language & compiler
- **The full primitive taxonomy now compiles.** The parser accepts all 8
  primitives (`facet`/`feed`/`stream`/`lifecycle`/`pipe`/`vault`/`media`/`signal`)
  with their per-kind blocks (`order`/`throttle`/`window`/`states`/`ttl`,
  `decrypt:`/`source:`), each type-checking its `what:` contract and emitting the
  right artifacts. An unknown primitive or a block on the wrong kind is a compile
  error that names the fix.
- **Structural guarantee enforced:** client-rendered kinds (`vault`/`media`/
  `signal`) emit **zero server template** — `looks:` on them is rejected, codegen
  produces no `.tmpl.html`, and the manifest carries the client render body
  instead. A compromised server has nothing to render vault plaintext with.
- Manifest gains `kind` plus per-primitive fields (`order`/`throttle`/`window`/
  `ttl`/`states`/`client`). Typed `<Kind>Data` structs are emitted for every kind.
- Note: per-primitive **runtime** semantics (ranking, windowing, client-side
  decrypt, binary delivery, ephemeral relay) are staged for the next round.

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
