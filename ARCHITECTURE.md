# Facet Architecture — Technical Architecture

The deep technical reference: the compiler, the server runtime, the wire
protocol, the client runtimes, and the security model. For *what FA is and how
to use it*, start at the [README](README.md) and the
[wiki](https://github.com/F33D3R-Inc/fct/wiki). For *why* decisions were made,
see the ADR log in [DECISIONS.md](DECISIONS.md).

## The pipeline

```
.fct files (FDL)
    │  fct compiler (lexer → parser → codegen)
    ▼
Go html/templates  +  manifest.json  +  typed <Facet>Data structs
    │  fa server library (your Go program imports it)
    ▼
rendered HTML / neutral view trees, HMAC-signed events
    │  SSE (one connection per client)
    ▼
web runtime (fa-runtime.js, ~8 KB)  /  FacetKit SwiftUI  /  FacetKit Compose
```

The browser/device runs **fixed plumbing only** — no application logic ever
ships to a client. All state, all logic, and all rendering decisions live in
the server process.

## Repo layout

```
cmd/fct/            the compiler + CLI (new/dev/build/check/fmt/audit/lsp/…)
  scaffold/         embedded project template `fct new` writes
internal/
  lexer/            offside-rule tokenizer (INDENT/DEDENT/LINE)
  ast/              syntax tree
  parser/           recursive-descent parser → ast.Facet
  codegen/          ast → Go html/template + manifest.json + typed structs
fa/                 SERVER runtime library (imported by every FA app)
  compile.go        public compiler API: Compile / CompileDir / Render
  app.go            App: On(event) router, Mount, Page/HandlePage
  hub.go            SSE hub (fan-out, heartbeats, graceful drain)
  event.go          Event + HMAC signing
  shell.go          the Playground (fa.Shell) — the page base canvas
  router.go         multi-page routing with client-side nav
  session.go        signed-cookie sessions
  form.go           form validation + uploads
  admin.go          the built-in admin panel
  authz.go          who: enforcement (RenderFor, View, policies)
  broker.go         cross-instance fan-out interface
  redisbroker.go    zero-dependency Redis Broker (raw RESP)
  style.go          the single-source design-system style table
  view.go           RenderTree / ParseView — the neutral view tree
  primitives.go     per-primitive runtime semantics (server side)
runtime/
  fa-runtime.js     the fixed ~8 KB web client runtime
clients/swift/      FacetKit — SwiftUI (iOS/macOS) native runtime
clients/android/    FacetKit — Jetpack Compose native runtime
std/                standard library (229 facets) + default theme
fatest/             test harness (like httptest, for facets/handlers)
examples/demo/      the reference app
```

## The compiler

Hand-written, hosted in Go (not Rust — ADR-0001) so it can type-check the
FDL↔Go boundary against real Go packages.

1. **Lexer** — offside-rule tokenizer. Indentation is significant and
   *relative*; tabs are an error; comments are full-line only (`#`, `#|…|#`).
2. **Parser** — recursive descent to an AST. All 8 primitives, each with its
   per-kind blocks (`order`/`throttle`/`window`/`states`/`ttl`,
   `decrypt:`/`source:`). Anything outside the grammar is a **syntax error
   naming the fix** — nothing is silently ignored.
3. **Codegen** — emits, per facet:
   - a Go `html/template` with `data-facet-id` injected on the root element
     (client-rendered kinds emit **no template** — see vault below);
   - a `manifest.json` entry: `{name, kind, facet_id, template|client,
     order/throttle/window/ttl/states, who, when[]}`;
   - a typed `<Facet>Data` struct (idiomatic naming: `avatar_url` →
     `AvatarURL`, `id` → `ID`).

Compile-time guarantees: every `looks:` identifier must be declared in
`what:`; child-facet cycles, unknown children, and unknown props are rejected;
`{{…}}` template injection is impossible (the `{…}` parser consumes every
`{`); malformed `throttle:`/`ttl:`/`window:` values are compile errors.

## The `fa` server library

What apps import. An FA application is: `Compile` → `New` → `On(…)` → `Mount`.

- **App** — one `/events` POST endpoint routes `{type, payload}` by event type
  to your handlers; the client has no route table. `Guard(event, fn)` gates
  events before handlers run. `Identify` maps a request to a user identity
  (typically from the built-in signed-cookie sessions).
- **Hub** — the SSE connection manager. Scoped delivery is structural:
  handler returns go only to the **acting connection**; `EmitTo` (identity),
  `EmitChannel` (subscribers, deny-by-default `ChannelAuth`), and `Broadcast`
  are explicit. Heartbeats, per-IP connection caps, graceful drain.
- **Events** — `{Op, FacetID, Fragment}` with Op ∈ replace/append/prepend/
  remove/signal. Every event is **HMAC-SHA256 signed** over
  `op\0facet_id\0fragment` before publish.
- **Router** — `Route(pattern, title, fn)` with `:param` captures. A normal
  request returns the full document; an `FA-Nav` fetch returns
  `{title, html}` and the runtime swaps the root mount **without a reload**,
  so the SSE connection and all live facets survive navigation.
- **Playground** — the provided document shell (`fa/shell.go`): `<html>`,
  CSP/meta, the signing key (`<meta name="fa-key">`), the
  `data-facet-id="fa:root"` mount, and the runtime script. Apps never
  hand-write the shell.

### The facet hierarchy

```
Playground            the base canvas (provided)
  └ wireframe facets  nav, sidebars, headers
      └ template facets   cards, profile headers
          └ composites      action bars, media grids
              └ atomics         buttons, avatars, badges
```

facet-ids address surgical updates: auto-derived per instance from the first
custom-typed `what:` field (`LikeButton:post:42`), singleton otherwise,
overridable with `facet-id: "…"`.

## The primitive taxonomy

| Primitive  | Role                          | Server renders? |
|------------|-------------------------------|-----------------|
| `facet`    | reactive UI fragment          | yes             |
| `feed`     | ranked, ordered list          | yes             |
| `stream`   | append-only, high frequency   | yes (`throttle`/`window`) |
| `signal`   | ephemeral peer state          | no (relays; TTL) |
| `lifecycle`| multi-step state machine      | yes             |
| `pipe`     | continuous data               | yes             |
| `vault`    | E2E-encrypted content         | **NO** — see below |
| `media`    | binary delivery (video/audio) | no (delivers)   |

**`vault` is the architectural line.** `what:` carries only the encrypted
envelope; `decrypt:` runs client-side; the compiler emits **zero server-side
template**, so a compromised server cannot produce plaintext — a guarantee
client-agnostic frameworks cannot make structurally.

### Runtime semantics (enforced on server, web, and native)

- **feed `order:`** — `Compiled.SortFeed` sorts the slice by the declared
  field before render (bare field = descending; `asc` flips; fails closed).
- **stream/pipe `throttle:`** — enforced in the hub per (scope, target, facet
  instance): trailing-edge coalescing — first frame immediate, intermediate
  frames replaced, the latest always flushes.
- **stream `window:`** — the runtimes trim the container after
  `append`/`prepend` from the opposite end; the DOM/tree never grows unbounded.
- **lifecycle `states:`** — `Compiled.Lifecycle` returns the validated machine
  (`Initial`/`Valid`/`Next`/`CanTransition`, forward-by-one).
- **signal `ttl:`** — `App.Signal`/`Ctx.Signal` relay a signed event to channel
  subscribers and store **nothing**; runtimes apply the payload to
  `data-fa-signal` elements as `data-*` attributes + `.fa-signal-live` and
  revert after the TTL.
- **vault `decrypt:`** — the key is registered on the device
  (`fa.vault.key(name, hex)` on web, `client.vaultKey(name, hex)` on native)
  and never sent; the runtime AES-GCM-decrypts `data-fa-envelope`
  (base64 IV‖CT‖tag) and renders the `decrypt:` body, HTML-escaped, failing
  closed.
- **media `source:`** — the runtime mounts the player, filling `{field}` holes
  from `data-*` attributes; `<hls>`/`<dash>` normalize to `<video>`. Native:
  AVKit on Apple; a pluggable `FacetKitConfig.mediaRenderer` on Android.

## The wire protocol

One SSE connection per client. The hello frame (`_conn`) carries the
connection id and the (public) signing key. Every subsequent frame is an
event, signed server-side and **verified by the client before it touches the
UI** — a tampered frame is dropped.

User actions are the reverse path: a `data-action` element's tap/click POSTs
`{type, payload, conn}` to `/events`; every other `data-*` attribute on the
element becomes the payload. CSRF: `/events` requires the per-connection id
(readable only over the same-origin stream) plus an Origin check.

### The native protocol

Native clients send `FA-Native: 1`. Routes then return `{title, tree}` where
`tree` is a **platform-neutral view tree** (`RenderTree`): `kind` ∈
box/text/button/image/input/link/icon, plus `facetId`, `action`, `attrs`, and
a server-resolved `Style` (direction/gap/pad/align + bg/fg/fontWeight/radius)
from the single style table (`fa/style.go`) — native clients hold **no style
logic**. On SSE, the hub transforms each event's HTML fragment into the styled
tree JSON and **re-signs it**, so the bytes a device verifies are exactly the
bytes it renders. Signal frames pass through with their payload-JSON signature
intact.

Both FacetKit runtimes (SwiftUI, Compose) implement: model, SSE + reconnect,
HMAC verification, surgical updates by facetId, action forwarding, and the
per-primitive semantics above, driven by the same `/manifest.json` registry
the web runtime builds.

## Security architecture

Defense in depth, secure-by-default (full audit: [SECURITY_AUDIT.md](SECURITY_AUDIT.md);
reporting: [SECURITY.md](SECURITY.md)):

- scoped SSE delivery (no broadcast by default; deny-by-default channels);
- structural authz — `Guard` gates events, `who:` (`require`/`redact`) is
  enforced at render via `RenderFor` and fails closed; `fct audit` prints the
  surface;
- CSRF (conn-id + Origin), per-IP rate limits + connection caps;
- contextual auto-escaping (`html/template`) + template-injection closed +
  CSP (`script-src 'self'`);
- HMAC-signed events verified on every runtime;
- compile-time rejection of cycles/unknown children/unknown props.

## Multi-instance design

The hub is in-process; horizontal scale-out needs two things (see the
[Deployment wiki page](https://github.com/F33D3R-Inc/fct/wiki/Deployment)):

1. a **stable shared signing key** (`FA_SIGNING_KEY`) on every instance;
2. a **Broker** for cross-instance fan-out — a built-in zero-dependency Redis
   adapter ships (`fa.NewRedisBroker`); events are signed before publish so
   all instances emit identical, client-verifiable frames.

Run SSE behind sticky load-balancing; each client's own connection stays local
and the broker delivers to the rest. `/healthz`, `/readyz` (drains on
`Shutdown`), and `/debug/metrics` are built in.

## Performance baseline

Per-op cost (Apple-class core; reproduce with `go test ./fa -bench .`):

| Operation | Cost | ~Throughput/core |
|---|---|---|
| Render one facet | ~17 µs | ~58k/s |
| Full event dispatch (guard+handler+render) | ~18 µs | ~55k/s |
| Signed fan-out to 1000 subscribers | ~200 µs | ~5M deliveries/s |
| Cold compile (startup) | ~66 µs | one-time |

Network-level load validation against a deployed multi-instance setup is
tracked in [ENTERPRISE.md](ENTERPRISE.md).
