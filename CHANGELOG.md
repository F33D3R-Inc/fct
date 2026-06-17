# Changelog

All notable changes. Versioning, the frozen 1.0 surface, and the deprecation
policy are defined in `STABILITY.md` (pre-1.0: breaking changes land only at
minor bumps, never at patch bumps, with migration notes here).

## Unreleased

Make the scaffold inert and the `when` block real, so an app's `main.go` never
becomes the place features pile up.

### Runtime

- **`when <event>` blocks are now server handlers** — `app.AutoWire(c, event, reducer)`
  registers a handler straight from a facet's compiled `when` block (ADR-0005:
  the `when` block *is* the handler). The reducer does the one thing the language
  can't express — mutate server state, return the data to render — and the runtime
  builds the SSE events from the declared mutations: it renders the target/item
  facet and derives the `data-facet-id` from that render, so it can never drift
  from what the template emits. Supports `replace`, `append`/`prepend with <Facet>`,
  and `remove`; rejects `replace_all` and stray `with` clauses at startup (the
  client runtime applies no such op). Additive — `app.On` still works for anything
  imperative. The fragment is always server-rendered, so there is no injection
  surface (the old `with event.payload` echo is gone).
- **`app.Serve(mux)`** — the whole server lifecycle in one call: listen on
  `FA_ADDR` (default `localhost:7373`), block until SIGINT/SIGTERM, drain SSE, and
  shut down. App code no longer touches `net/http`, `os/signal`, or `syscall`.

### Scaffold (`fct new`)

- **`main.go` is now inert** — it only calls `app.Main()`. App code lives in an
  `app/` package, one file per feature (`app.go`, `routes.go`, `like.go`,
  `style.go`); the like feature is the `when post.like:` block plus a toggle
  reducer, with no hand-built facet-id strings or `fa.Event` construction. A guard
  test (`cmd/fct/scaffold_test.go`) fails the build if the entrypoint regrows.

### CI

- **gofmt gate** — CI now fails on unformatted code (it had silently let an
  unformatted file land).

## v0.14.0

Enterprise P0 round: observability in standard formats, the API stability
contract, and supply-chain hardening (see `ENTERPRISE.md`).

### Auth (batteries-included)

- **`fa.Auth`** — built-in authentication, not a bolt-on: `app.Auth(store)` ties
  password hashing, an account store, and the app session together. `Signup`,
  `Authenticate`, `Login`/`Logout`, `Current`, and `Identity` (an `App.Identify`
  resolver) — a logged-in account IS the FA identity that scopes SSE delivery,
  `App.Guard`, and `who:` policies. `auth.Guard("admin")` / `auth.Authorize("admin")`
  gate events and the admin panel (Django-style). `auth.MountLogin` registers a
  ready-to-use `/login` (form + POST) and `/logout`, same-origin-checked.
- **Password hashing is dependency-free** — PBKDF2-HMAC-SHA256 (stdlib
  `crypto/pbkdf2`, OWASP work factor) in a versioned, self-describing hash
  string, so the cost can be raised or the algorithm swapped (argon2id) without
  invalidating stored hashes. Constant-time verify; unknown login and wrong
  password return the same error (no account enumeration), with timing equalized.
- **`AuthStore`** interface with an in-memory default (`NewMemoryStore`) for
  development; implement it over your database for production.

### Language

- **Named slots** — a facet can declare multiple insertion points with
  `slot name:`, and a parent targets each with a `fill name:` block (content
  outside any `fill` goes to the default `slot:`). Every slot may carry default
  content shown when unfilled. Filling a slot the child does not declare is a
  compile error. Backward compatible: the default slot is unchanged.
- **Computed fields** — `what:` now accepts derived values: `total = price * qty`
  or `free: bool = total >= 100` (type optional). They are resolved server-side
  at render time and excluded from the generated `<Name>Data` struct (the caller
  never supplies them). A computed field may use any input field and any computed
  field declared above it; a forward/self reference is a compile error.
- **Integer arithmetic stays integer.** `+`, `-`, `*` now return an integer when
  both operands are integral (previously always `float64`), so results compare
  cleanly against integer literals (e.g. `likes + 1 > 100`). `/` still yields a
  float. Display output is unchanged for whole numbers.
- **Client-side `if`/`for` in vault/media bodies** — `decrypt:` and `source:`
  bodies now support `{if expr}…{else}…{end}` and `{for v in path}…{end}` over
  the (decrypted / metadata) values, not just `{field}` interpolation. Dotted
  paths and Go-style truthiness (empty array/string/0 are falsy). Interpolated
  values stay HTML-escaped — the vault safety guarantee is unchanged. Implemented
  in all three runtimes; the web runtime is node-tested (`runtime/fill_test.js`).
- **Cross-platform `style:` block** — a facet can set its root layout/appearance
  with design tokens (`direction`, `gap`, `pad`, `align`, `justify`, `grow`,
  `width`/`height`, `bg`, `fg`, `radius`, `font-size`, `font-weight`). The
  compiler resolves tokens at build time and attaches the result to the root
  element, so the *same* declaration renders identically on web, iOS (SwiftUI)
  and Android (Compose) via the neutral tree. Every property/token is validated
  (a typo is a compile error). It is not arbitrary CSS — web-only effects
  (`:hover`, `@media`, animations) remain in the global stylesheet. Server-
  rendered primitives only.

### Observability (Prometheus + tracing)

- **`GET /metrics`** — Prometheus exposition format, dependency-free:
  `fa_events_in_total`, `fa_events_out_total`, `fa_sse_connections_active`,
  `fa_sse_connections_total`, `fa_events_rate_limited_total`,
  `fa_events_forbidden_total`, plus two latency histograms —
  `fa_dispatch_duration_seconds` (guard + handler + emit, per client action)
  and `fa_fanout_duration_seconds` (one broker message applied to local
  connections). `/debug/metrics` JSON unchanged.
- **`fa.Tracer` + `fa.WithTracer`** — a three-method hook (StartSpan /
  Inject / Extract) FA calls around `fa.dispatch` → `fa.emit` → `fa.deliver`.
  The emitter's trace context rides inside the broker message, so one request
  is traceable across instances; wire it to OpenTelemetry with ~20 lines
  (example in `wiki/Deployment.md`). FA itself stays dependency-free.

### API stability

- **`STABILITY.md`** — semver commitment, the 1.0 surface freeze (`fa`, `std`,
  `fatest`, manifest schema, SSE wire format), and the deprecation policy
  (≥ one full minor of `Deprecated:` warning before any removal).
- **Wire-format version negotiation** (`fa.WireVersion`, currently `"1"`):
  clients declare their version at connect — `FA-Wire-Version` header (native)
  or `?v=` (web; EventSource can't set headers) — and a mismatch is rejected
  loud with **426 Upgrade Required**. The `_conn` hello frame now carries the
  server's version (`"v"`); all three runtimes check it on every (re)connect
  and stop with a fatal, human-readable error (`wireError` on FacetKit/Compose,
  console error on web) instead of rendering garbage or reconnect-looping.
  Clients that predate negotiation are treated as v1 — nothing breaks today.

### Supply chain

- Releases now ship a keyless **cosign** signature over `checksums.txt`
  (Sigstore/Rekor), **SLSA build provenance** (GitHub artifact attestations),
  and an **SPDX SBOM**; binaries build reproducibly (`-trimpath -buildid=`,
  `-buildvcs=false`, CGO off). Verify with three commands —
  `docs/REPRODUCIBLE_BUILDS.md`.
- New `ci.yml`: vet + tests, **govulncheck** on every push/PR, dependency
  review on PRs. All third-party actions pinned to commit SHAs.

## v0.13.0

### Primitive runtime semantics — native parity (FacetKit Swift + Compose)

The round staged in v0.12.0: both native client runtimes now enforce the same
per-primitive rules as the web runtime, driven by the same `/manifest.json`
registry (keyed by facet name; `Primitives.swift` / `Primitives.kt` hold the
shared pure logic, unit-tested):

- **stream `window:`** — after an `append`/`prepend` the target node's children
  are capped, trimming the opposite end (oldest dropped on append).
- **signal `ttl:`** — relayed signal payloads land as `data-*` attributes plus
  `fa-signal-live` on every node whose `data-fa-signal` matches, and revert
  after the declared TTL; reserved keys (`action`, `fa*`) can't be hijacked.
  A programmatic `onSignal` hook fires for non-tree consumers.
- **vault** — `client.vaultKey(name, hexKey)` registers a device-held AES-GCM
  key (never sent to the server); the runtime decrypts each matching node's
  `data-fa-envelope` (base64 IV‖CT‖tag) and renders the manifest's `decrypt:`
  body with `{plaintext}` / JSON fields, HTML-escaped. Fails closed — a wrong
  key or bad envelope leaves the node untouched.
- **media** — the `source:` body is filled from the node's `data-*` attributes,
  `<hls>`/`<dash>` normalize to `<video>`, and the node renders as a real
  player: AVKit (`VideoPlayer`) on Apple platforms; on Android a pluggable
  `FacetKitConfig.mediaRenderer` (e.g. Media3/ExoPlayer — FacetKit itself adds
  no player dependency), defaulting to a poster placeholder.

Wire note: native SSE frames were already re-signed over the styled tree JSON,
so the bytes each device verifies are exactly the bytes it renders; signal
frames pass through with their payload-JSON signature intact.

### Docs

- `wiki/` — full user documentation (13 pages: getting started, tutorial,
  databases, deployment, FDL reference, std, auth/forms, realtime, admin,
  testing, native, troubleshooting), wired into mkdocs.
- `ENTERPRISE.md` — the enterprise-readiness scorecard and prioritized
  remaining work.

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

### Responsive layouts (enterprise polish)

- **std theme**: added the page-level breakpoints the fluid components needed —
  the AppShell now collapses like a first-class product: 3 columns → the right
  rail drops (≤1100px) → the nav rail goes icon-only (≤768px) → single column
  with a fixed bottom nav bar incl. safe-area inset (≤520px). Tables scroll
  horizontally instead of overflowing; story viewer goes full-bleed on phones.
- **Admin panel**: the fixed 240px sidebar grid now collapses to a sticky top
  bar with wrapping nav on narrow windows; detail field grids go single-column;
  tables scroll instead of crushing the layout.
- (Everything else was already fluid: viewport meta in the shell, flex-wrap,
  `minmax()` grids, aspect-ratio, percentage caps.)

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

Native runtimes (FacetKit / Compose) got window/TTL/vault/media enforcement in
v0.13.0; at this release they already received signal events over the same
signed wire.

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
