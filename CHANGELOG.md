# Changelog

All notable changes. Versioning, the frozen 1.0 surface, and the deprecation
policy are defined in `STABILITY.md` (pre-1.0: breaking changes land only at
minor bumps, never at patch bumps, with migration notes here).

## v0.14.7

Close the Go-only gap: the framework "batteries" are now **uniform across every
backend language**, so `fct` + Node/Python/Rust is a real choice, not Go-plus-demos.
Each runtime ports `fa/session.go`, `fa/authz.go`, `fa/security.go`, `fa/form.go`,
`fa/broker.go`; the compiler now emits `who:` into `render.json`.

### Uniform framework surface

- **Signed-cookie sessions** — HMAC layout matches Go byte-for-byte: all four
  languages mint the identical `fa_session=…` cookie and read each other's, so a
  session established on a Go server stays valid behind a Node/Python/Rust server
  with the same key. Tampered cookies are rejected.
- **`who:` authorization** — `require` policy gate (denied → empty render) and
  `redact` field stripping, enforced from the IR's new `who` block and threaded
  through child facets.
- **CSRF / same-origin** (cross-origin `POST /events` → 403), **per-IP rate
  limiting** (token bucket → 429 over burst), **security headers** (CSP +
  `X-Content-Type-Options` + `Referrer-Policy` on the shell).
- **Forms** (parse + chainable validators) and a **pluggable broker**
  (`publish`/`subscribe`, in-process default, Redis/NATS opt-in via the same
  interface in every language).
- Same API shape everywhere: `app.identify`, `app.policy`, `app.sessions` /
  `with_sessions`, `app.broker`.

Verified cross-runtime: byte-identical session cookies, identical authz
deny/redact, 403 on cross-origin, 429 over burst. Still Go-only and additive: admin
panel, observability/tracing, built-in password store. See `runtimes/README.md`,
`docs/BACKENDS.md`. Closes ENTERPRISE P2 #13.

## v0.14.6

The non-Go backends become **runnable**. v0.14.5 made the compiler emit for
Node/Python/Rust; this release ports the server runtime to each, so a facet app
actually runs on all four languages — and the existing iOS/Android clients work
against every one of them.

### Server runtimes (`runtimes/{node,python,rust}`)

- The `fa/` live-render loop, ported to each language, dependency-free: manifest +
  render-IR load, a render-IR interpreter + neutral expression evaluator with HTML
  auto-escaping, **HMAC-SHA256 signing byte-identical to `fa/event.go`**
  (`op\0facet_id\0fragment`), an in-memory SSE hub, the `GET /` · `GET /sse` ·
  `POST /events` · `/manifest.json` · `/render.json` · `/fa-runtime.js` endpoints,
  and a `when:`-driven re-render → signed push. Each ships a runnable LikeButton
  demo. The Rust runtime hand-rolls SHA-256/HMAC, a JSON parser, and a minimal
  HTTP/1.1+SSE server (std only).
- **Neutral render IR** (`codegen.RenderIR` → `render.json`): a flat
  text/expr/if/else/end/for/child op stream with expressions as a neutral JSON AST.
  Emitted for non-Go targets; the manifest and Go path are untouched.

### Mobile parity (FA-Native neutral tree)

- All three runtimes implement the `FA-Native` path — render HTML, then parse it
  into a platform-neutral ViewNode tree (mirror of Go's
  `RenderTree = ParseView(Render(...))`; ports of `fa/view.go` + `fa/style.go`,
  incl. the std design-system class table). `GET <route>` + `FA-Native: 1` returns
  `{title, tree}`; a native SSE connection is pushed ViewNode-tree fragments,
  signed identically. The emitted tree JSON is **byte-identical to `fa.ParseView`**,
  verified across Go/Node/Python/Rust, so `clients/swift` (FacetKit) and
  `clients/android` (facetkit) render against any of the four backends.

One server, three renderers (web/iOS/Android) — now over four server languages.
Remaining `fa/` surface (sessions, forms, `who:` authz, broker, rate limiting) is
Go-only and additive against the shared contract. See `runtimes/README.md`,
`docs/BACKENDS.md`.

## v0.14.5

Break the Go lock — the compiler is no longer language-locked. One FDL source now
compiles to **four targets** (`go` default, `node`, `python`, `rust`), selected by
`fct.toml [compiler] target`. This is ROADMAP #4 / ENTERPRISE P2 #13 and the named
reversal condition of ADR-0001 (now DECISIONS.md ADR-0010).

### Pluggable codegen backends

- New `codegen.Backend` interface (`internal/codegen/backend.go`) with a registry
  and `BackendFor`/`BackendNames`. The compiler front end (lex → parse → AST →
  checks → flat render stream → `manifest.json`) is target-neutral; a backend
  supplies only `Expr` (FDL expression → target expression), `FieldName`
  (identifier casing), `Types` (typed per-facet data declarations), and `TypesFile`.
- Three new targets, each ~80 lines: `backend_node.go` (camelCase, TS interfaces,
  JS infix), `backend_python.go` (snake_case, `@dataclass`, `and`/`or`/`not` +
  `True`/`False`), `backend_rust.go` (snake_case, `pub struct`, Rust infix).
- One neutral expression parser (`exprast.go`: `parseExpr` → `exNode` tree +
  `renderInfix`) feeds all non-Go targets — not four hand-rolled parsers. The Go
  path is **byte-for-byte unchanged**: `goBackend` delegates to the original
  `goExpr`/`GoName`/`GoStructs`.
- `fct build` reads `[compiler] target` (stdlib-only reader) and routes through the
  selected backend. Non-Go targets emit the neutral `manifest.json` + typed data in
  the target language; the Go `html/template` files are the Go render path and are
  skipped. Tested in `internal/codegen/backend_test.go`.

### Staged, not shipped

The per-language **server runtimes** (render-IR interpreter, SSE hub, fragment
signing, sessions, routing, authz — the `fa/` equivalent) are the remaining track.
The neutral render IR and the runtime surface each language must implement are
specified in `docs/BACKENDS.md`; the shared `manifest.json` + wire/signing format
bound the work. ADR-0001 stays only in that the compiler itself remains Go-hosted.

## v0.14.4

Make the reactive engine enterprise-grade — parity with Svelte/Solid on the parts
that matter for building real apps. The four hard pieces, all shipping and tested
(`runtime/reactive_test.js` — 51 assertions, `internal/codegen/view_test.go`):

### Fine-grained invalidation

- The dependency graph compiled into the manifest now **drives dispatch**, not just
  records it. On an event the runtime diffs the signal store once, expands the
  change through the derived graph (`dirtySet`), recomputes only the dirty derived
  values (cached per instance), and patches only the bindings / attributes / lists /
  regions / inputs whose roots changed (`update` / `invalidate`). No more
  recompute-everything-per-event — this is Svelte's compiled dirty-tracking model.
  Writes within one event are batched into a single dispatch.

### Real keyed list reconciler + virtualization

- `reconcileChildren` keys children, reuses by key, and computes a
  longest-increasing-subsequence (`longestIncreasing`) so a reorder/insert moves
  only the nodes outside the stable run — O(moved), not O(n).
- **`for v in list virtual <px>`** windows a large list: only the rows in the scroll
  viewport (+overscan) are in the DOM (`visibleRange` / `reconcileVirtual`), with an
  honest scrollbar via a sized spacer. A 100k-row feed stays O(viewport).
  `examples/feed.fct`.

### Structural control flow

- A reactive `{if}`/`{for}` (condition/iterable over signals) is lifted to a client
  region that truly **mounts/unmounts** the active branch — the inactive branch is
  not in the DOM, unlike a `hidden` show-binding. Fully nestable; `else:` gives the
  alternate branch. Control over server data stays a server `{{if}}`/`{{range}}`.
  `examples/tabs.fct`.

### Client-side facet instantiation (components in regions/lists)

- A `<Child/>` call inside a reactive `{if}`/`{for}` now renders as a real reactive
  instance in the browser. The compiler emits a fill-renderable client `view` per
  facet (binding ids pinned to the manifest, `TestClientViewBindingIDsAlign`); a
  child call becomes a `{cmp Name|field=expr|…}` token the runtime resolves by
  evaluating each prop in the parent scope (**object props pass by reference, not
  stringified**) and recursing into the child's view, then hydrating each instance
  (its own signals/bindings/handlers). So `<Tweet author="{t.author}"/>` lives inside
  a reactive — and virtualized — feed, each row its own stateful component.
  `examples/timeline.fct`. Honest limitations: a client-instantiated child does not
  render block-form slot content, and a binding is pure-prop or pure-signal (no mix).

## v0.14.3

Compiled fine-grained client reactivity — do what React does, with minimal
front-end JS and no virtual DOM, as compiler output rather than a shipped
framework (see `docs/REACTIVITY.md`). The `fct` compiler bakes each facet's
reactive graph into the manifest and a tiny per-instance updater; the runtime
patches exactly the bound node when a signal changes. Server-authoritative stays
the default — this layer owns only ephemeral client state.

### Language

- **`state:`** — local reactive values (signals) with required initial values.
- **derived** — a computed `what:` field whose roots are all signals recomputes
  client-side; its first paint is baked into the server template (no flash).
- **`actions:`** — named, reusable client handlers (`signal = expr`), wired to an
  element with `on:<event>="name"`; mutate signals locally with zero round-trip.
- **`effects:`** — `on <signals>: <action>` runs an action when a dependency
  signal changes (once per cycle, never re-triggering — loop-free).
- **reactive lists** — `for v in <signal>` over a list signal reconciles by key.
- **attribute / class / show bindings** — a signal inside an attribute value
  patches that attribute: `class`/`href`/`aria-*` set the value, and boolean
  attrs (`hidden`/`disabled`/`checked`/…) toggle by presence, so
  `hidden="{!visible}"` is real show/hide and `disabled="false"` can never be
  emitted.
- **forms** — `bind:value` / `bind:checked` two-way-bind a control to a state
  signal (reads on `input`/`change`, writes back on flush without clobbering a
  focused field). Only a mutable `state:` signal can be a target.
- **routing** — built-in reactive `route` signal seeded from the path and updated
  on client navigation; a client router falls out of show bindings
  (`<section hidden="{route != "/about"}">`), and matching `data-nav` links get
  `.fa-active` + `aria-current`.
- **async** — `query: name from "url"` exposes a fetch to the reactive layer as
  `{loading, error, data}`; loading/error/data are just show bindings over a
  query value. Server-authoritative by transport (a normal same-origin endpoint).

All new blocks are additive and typed: a `bind:` to a non-signal, a reserved
`route` name, a query-name collision, or an undeclared root is a compile error,
not a silent blank.

### Runtime (`fa-runtime.js`)

- A CSP-safe expression evaluator (no `eval`/`Function`) mirroring the compiler
  grammar drives signals, derived values, actions, effects, list reconciliation,
  attribute bindings, form sync, the `route` signal, and async queries. The graph
  is compiled into the manifest; only leaf expressions are evaluated — "one IR,
  swappable executors."

### Examples

- `counter`, `poll`, `todo`, `tracker`, `like`, `greeter`, `site`, `forecast` —
  one reference facet per brick.

## v0.14.2

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
