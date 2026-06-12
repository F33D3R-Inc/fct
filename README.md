# Facet Architecture (`fct`)

**A server-authoritative web framework you download and build with — the way
people download React, but for the opposite architecture.** You write Facets in
**FDL** (`.fct` files); the `fct` compiler turns them into server templates + a
client manifest. The browser runs one fixed ~8 KB runtime that is pure plumbing —
**no application logic ever ships to the client.** Server is the single source of
truth; UI is a live projection of it, pushed over SSE.

> **This file is the single source of truth for the framework.** Read it first.
> New to FA? **`GUIDE.md`** is the hands-on "build a website" tutorial, and the
> **[`wiki/`](wiki/Home.md)** is the full user documentation (getting started,
> databases/Postgres, Docker, deployment, auth, testing, native clients).
> `DECISIONS.md` records *why* choices were made (the ADR log).
> `ENTERPRISE.md` tracks the remaining path to enterprise readiness.

---

## Status — proven end to end

The `facet` pipeline works through to a live page, a scaffolded project boots out
of the box, and the rest of the primitive taxonomy now compiles.

| Stage | State |
|---|---|
| Lexer → Parser → Codegen → Client runtime | ✅ working for `facet` |
| `fa` server library (SSE hub, HMAC-signed events, event router) | ✅ |
| Playground (the page base) + `fct new` scaffold + `fct dev` | ✅ |
| Child-facet composition (`<Avatar/>` + `slot:`) | ✅ |
| Typed codegen (`<Facet>Data` structs, idiomatic initialisms) | ✅ |
| All 8 primitives — parser + codegen + manifest surface | ✅ |
| Per-primitive **runtime** semantics — web (feed ranking, stream throttle/window, lifecycle transitions, signal relay + TTL, vault decrypt, media mount) | ✅ |
| Per-primitive runtime semantics — native clients (FacetKit / Compose) | ✅ |

Every primitive now has runtime behavior, server-side and in every runtime:
feeds rank via `SortFeed`, stream/pipe `throttle:` is enforced in the hub
(trailing-edge coalescing), `window:` trims the DOM, lifecycles validate
transitions, signals relay ephemerally and expire after `ttl:`, vaults decrypt
client-side (AES-GCM via `fa.vault.key` on web, `client.vaultKey` on native),
and the runtimes mount media players from `source:`. The client-rendered kinds
(`vault`/`media`/`signal`) still emit **zero server template** (the structural
guarantee). FacetKit (SwiftUI) and the Compose client enforce the same rules
from the same `/manifest.json` registry the web runtime builds.

Verified loop: `fct new` → `go run .` → a page with the Playground, a `Home`
facet and a live `LikeButton`; clicking it POSTs to one `/events` endpoint, the
server re-renders the facet and pushes an **HMAC-signed** `replace` over SSE, and
the runtime verifies + swaps it by `data-facet-id`.

---

## Install

> **Prerequisite — Go 1.26+.** FA apps *are* Go programs (the server is a single Go
> binary), so you need Go installed to build one. Get it at <https://go.dev/dl/>.

Install the **`fct`** command one of two ways.

### A) With Go (any OS — simplest)

```sh
go install github.com/F33D3R-Inc/fct/cmd/fct@latest
```

This puts `fct` in your Go bin dir (`go env GOBIN`, or `$(go env GOPATH)/bin`).
Make sure that's on your `PATH`.

### B) Prebuilt binary (from the Releases page)

Download the binary for your platform from
**[Releases](https://github.com/F33D3R-Inc/fct/releases/latest)**, then:

**macOS** (Apple Silicon → `darwin-arm64`, Intel → `darwin-amd64`)
```sh
chmod +x fct-*-darwin-*
sudo mv fct-*-darwin-* /usr/local/bin/fct
xattr -d com.apple.quarantine /usr/local/bin/fct 2>/dev/null || true   # if Gatekeeper blocks it
```

**Linux** (`linux-amd64` or `linux-arm64`)
```sh
chmod +x fct-*-linux-*
sudo mv fct-*-linux-* /usr/local/bin/fct
```

**Windows** (`windows-amd64.exe`) — rename to `fct.exe` and put it in a folder on
your `PATH` (e.g. PowerShell):
```powershell
mkdir "$env:USERPROFILE\bin" -Force
move .\fct-*-windows-amd64.exe "$env:USERPROFILE\bin\fct.exe"
setx PATH "$env:PATH;$env:USERPROFILE\bin"   # reopen the terminal after this
```

Verify: `fct version`.

## Quick start

```sh
fct new myapp
cd myapp
go run .            # open http://localhost:7373  (FA_ADDR overrides the port)
fct dev             # same, but rebuilds on .fct change
```

`fct new` runs `go mod tidy` for you, so `go run .` downloads the framework
(`github.com/F33D3R-Inc/fct/fa` and `/std`) from GitHub and boots — there is no
separate "install the library" step. A scaffolded `main.go` is pure wiring:
compile the facets, declare what each event does, serve the Playground. No client
framework, no API layer, no shell HTML to hand-write.

> Working from a local checkout instead of the published module? Point a scaffold
> at it with `go mod edit -replace github.com/F33D3R-Inc/fct=/path/to/fct`.

---

## Repo layout

```
cmd/fct/            the compiler + CLI (new/dev/build/parse/lex/version)
  scaffold/         embedded project template `fct new` writes
internal/
  lexer/            offside-rule tokenizer (INDENT/DEDENT/LINE)
  ast/              v0 syntax tree
  parser/           recursive-descent parser → ast.Facet
  codegen/          ast → Go html/template + manifest.json
fa/                 SERVER runtime library (imported by every FA app)
  compile.go        PUBLIC compiler API: Compile / CompileDir / Render
  app.go            App: On(event) router, Mount, Page/HandlePage
  hub.go            SSE hub (fan-out, heartbeats, graceful)
  event.go          Event + HMAC signing
  shell.go          the Playground (fa.Shell) — the page base canvas
runtime/
  fa-runtime.js     the fixed ~8 KB web client runtime
  runtime.go        embeds it
clients/swift/      FacetKit — the SwiftUI (iOS/macOS) native client runtime
clients/android/    FacetKit — the Jetpack Compose (Android) native client runtime
examples/demo/      the reference app (same machinery as a scaffold)
DECISIONS.md        ADR log — the "why"
```

---

## The language (FDL v0)

FDL accepts all 8 primitives (`facet`/`feed`/`stream`/`lifecycle`/`pipe`/`vault`/
`media`/`signal`). They share one block set — `who`/`what`/`looks`/`when` — and
each adds a small declarative surface of its own (below). Anything outside this is
a **syntax error with a clear message** (we never silently ignore unsupported
input — an unknown primitive, or a block on the wrong kind, names the fix).

### Primitive-specific blocks

| Kind | Renders | Extra blocks |
|---|---|---|
| `facet` | server | — |
| `feed` | server | `order: <field>` |
| `stream` | server | `throttle: <dur>`, `window: <n>` |
| `lifecycle` | server | `states: a, b, c` |
| `pipe` | server | `throttle: <dur>` |
| `vault` | **client** | `decrypt:` body (no `looks:`) |
| `media` | **client** | `source:` body (no `looks:`) |
| `signal` | **client** | `ttl: <dur>` (relays `what:`) |

Client-rendered kinds replace `looks:` with their own render/transport block and
the compiler emits **no server template** for them — the bytes that render vault
content never exist on the server. (Per-primitive *runtime* behavior: see "How
the primitives behave at runtime" below.)

### Lexical rules
- UTF-8, case-sensitive.
- **Indentation is significant.** Nesting is by *relative* indentation; 4 spaces
  per level is the convention but not enforced as a fixed width. **Tabs are an
  error.** Because nesting is relative, wrapped HTML continuation lines inside a
  `looks:` body may align to any column. A dedent that lands between two open
  levels is still an error.
- **Comments are full-line only:** a line whose first non-whitespace char is `#`
  (so HTML/CSS `#fff` and `href="#"` survive). `#| … |#` spans whole lines.

### Block keywords
One consistent set across the (future) whole taxonomy:

- `what:` — the data the facet needs (props; `name: Type` per line).
- `looks:` — the template: raw HTML + `{expr}` holes + `if`/`for`/`else`, child
  facets `<Avatar user="{user}"/>`, and `slot:` (a content hole a parent fills by
  using the facet in block form `<Card …> …content… </Card>`).
- `when <event>:` — an event handler; the subscription is implied (there is no
  separate `subscribe` block).
- `who:` — authorization: `require: <policy>` and `redact <field> [unless <policy>]`
  (named policies the app implements). Recorded in the manifest and reported by
  `fct audit`. See Security below.

```
facet LikeButton:
    what:
        post: Post
        count: int
        liked: bool
    looks:
        <button class="like{ if liked } active{ end }" data-action="post.like" data-post-id="{post.id}">
            <svg ... fill="{ if liked }currentColor{ else }none{ end }">…</svg>
            if count:
                <span>{count}</span>
        </button>
    when post.like_toggled:
        replace LikeButton with event.payload
```

### `looks:` template syntax
- `{expr}` → interpolation. Inline control: `{ if cond } … { else } … { end }`.
- Block control on its own line: `if cond:` / `for x in y:` / `else:` with an
  indented body.
- **Expressions** support identifier paths (`post.id` → `{{.Post.Id}}`, loop vars
  → `$x`), method/function calls (`viewer.can_view(post)`), comparisons
  (`== != < <= > >=`), boolean (`&& || !`), arithmetic (`+ - * / %`), and literals
  (`123`, `"text"`, `true`/`false`) — e.g. `if likes > 100 && viewer.can_view(post):`.
  Segments are Title-cased, so backend fields/methods must be exported.

### facet-id
Auto-derived from the first custom-typed (capitalized) `what:` field as
`Name:field:{field.id}` (e.g. `LikeButton:post:{post.id}`). No custom-typed
field → singleton id `Name`. Override with `facet-id: "…"`.

### Compiler outputs (per facet)
1. a Go `html/template` with `data-facet-id` injected on the root element;
2. an entry in `manifest.json`: `{ name, facet_id, template, when[] }`.

(The spec's other two outputs — event-handler stubs and the SSE subscription map
— are not generated yet.)

---

## The Playground & the facet hierarchy

Every app is a tree of facets rooted on the **Playground** — the base webpage
canvas the framework provides. You never hand-write the document shell.

```
Playground            <html>/<head>/<body> + the FA runtime + <meta name="fa-key">,
  │                   with the content mount at data-facet-id="fa:root"
  └ wireframe facets  nav, sidebars, headers
      └ template facets   cards, profile headers
          └ composites      action bars, media grids
              └ atomics         buttons, avatars, badges
```

The Playground is provided by `fa.App.Page` / `HandlePage` (`fa/shell.go`):
it renders the full document including the signing key (so the client can verify
pushed events), a configurable title/theme/CSS, the `<main data-facet-id="fa:root">`
mount, and `<script src="/fa-runtime.js">`. App content renders inside `fa:root`;
sub-facets keep their own ids and are swapped surgically.

**Composition today:** facets compose by the server rendering each and the app
assembling them (e.g. `Home` + `LikeButton`). FDL child-facet calls
(`<LikeButton/>` inside a parent's `looks:`) and `slot`/`fill` are designed but
not yet implemented — until then, concatenate rendered facets in the handler.

---

## The primitive taxonomy (canonical; all 8 compile and run)

All 8 are accepted by the compiler (parser + codegen + manifest) and have
runtime behavior on the server and in the web runtime. The "Server renders?"
column is the architectural line the compiler **enforces**: client-rendered
kinds emit zero server template.

| Primitive  | Role                          | Server renders? |
|------------|-------------------------------|-----------------|
| `facet`    | reactive UI fragment          | yes — **v0**    |
| `feed`     | ranked, ordered list          | yes             |
| `stream`   | append-only, high frequency   | yes (`throttle`/`window`) |
| `signal`   | ephemeral peer state          | no (relays; TTL) |
| `lifecycle`| multi-step state machine      | yes             |
| `pipe`     | continuous data               | yes             |
| `vault`    | E2E-encrypted content         | **NO** — see below |
| `media`    | binary delivery (video/audio) | no (delivers)   |

**`vault` is the architectural line.** It is the one primitive where the server
never renders the content: `what:` carries only the encrypted envelope,
`decrypt:` runs client-side, and `looks:` renders in the browser. The compiler
emits **zero** server-side template for vault content, so a compromised server
still cannot produce plaintext — a guarantee React cannot make structurally,
because React has no notion of *where* rendering happens as a security primitive.

**`media`** delivers binary (HLS/DASH/WebRTC via a `source:` block); `looks:` is
just the player container and the runtime owns what's inside it.

### How the primitives behave at runtime

- **feed `order:`** — `c.SortFeed("Timeline", items)` sorts a slice (structs,
  struct pointers, or maps) by the declared field before render. A bare field
  ranks **descending** (a feed is best/newest-first); `order: <field> asc` flips
  it. Fails closed: a missing field or non-comparable type is an error and the
  slice is untouched.
- **stream/pipe `throttle:`** — enforced in the hub at the emitting instance.
  Trailing-edge coalescing per (scope, target, facet instance): the first frame
  in a quiet period goes out immediately; frames arriving inside the interval
  replace each other and the **latest** flushes when it elapses. Final state is
  always delivered; intermediate frames are dropped.
- **stream `window:`** — enforced by the web runtime: after an `append`/
  `prepend` the container is trimmed from the opposite end, so the DOM never
  grows unbounded.
- **lifecycle `states:`** — `c.Lifecycle("Order")` returns the validated state
  machine: `Initial`, `Valid`, `Next`, and `CanTransition(from, to)` (forward by
  exactly one declared state; branches like cancellation are app logic on top of
  `Valid`).
- **signal `ttl:`** — `app.Signal(channel, facetID, payload)` (or `ctx.Signal`
  from a handler) relays a signed `signal` event to a channel's subscribers and
  stores **nothing**. In the page, elements opt in with
  `data-fa-signal="Typing"`: the runtime sets each payload key as a `data-*`
  attribute, adds `.fa-signal-live`, and reverts both after `ttl:` — a typing
  indicator is pure CSS, zero app JS.
- **vault `decrypt:`** — the app provides the key in the browser via
  `fa.vault.key("DM", hexKey)` (derived client-side; never sent). The runtime
  AES-GCM-decrypts each `[data-fa-vault="DM"]` element's `data-fa-envelope`
  (base64 of 12-byte IV ‖ ciphertext) and renders the `decrypt:` body with
  `{plaintext}` (a JSON plaintext also exposes its fields), HTML-escaped.
- **media `source:`** — the runtime mounts the player inside each
  `[data-fa-media="Clip"]` element, filling `{field}` holes from the element's
  `data-*` attributes; `<hls>`/`<dash>` normalize to `<video controls>`.

Native runtimes (FacetKit / Compose) enforce the same semantics: each client
fetches `/manifest.json`, builds the per-primitive registry, trims stream
`window:` children, applies/expires signals after `ttl:`, decrypts vault
envelopes with a device-held key (`client.vaultKey(name, hexKey)` — never sent),
and mounts media players (AVKit on Apple; a pluggable
`FacetKitConfig.mediaRenderer` on Android, defaulting to a placeholder).
Native frames are re-signed over the styled tree JSON, so the bytes the device
verifies are exactly the bytes it renders.

---

## Architecture

- **Compiler (Go).** Hand-written offside-rule lexer → recursive-descent parser →
  codegen to `html/template`. Hosted in Go (not Rust as the spec said) so it can
  type-check the FDL↔Go boundary against real Go packages later. See ADR-0001.
- **`fa` server library.** What apps import. `Compile`/`CompileDir` (the public
  face of the compiler — apps can't import `internal/`), `App` (one `/events`
  sink that routes by event type to your handlers), `Hub` (SSE fan-out,
  heartbeats), and **HMAC-signed events** (`op\0facet_id\0fragment`).
- **Client runtime (`fa-runtime.js`).** Fixed, app-logic-free. Holds the SSE
  connection, applies fragments by `data-facet-id`, **verifies each event's HMAC**
  against `<meta name="fa-key">`, and forwards `[data-action]` clicks to the
  single `/events` endpoint (no per-action route table — the server routes).

---

## Security (built-in)

FA aims for Django-style secure-by-default. Full audit + roadmap in `SECURITY.md`.

- **Scoped delivery.** A handler's events go only to the **acting connection** —
  never broadcast. `EmitTo` (user), `EmitChannel` (subscribers, **deny-by-default**
  `channel_auth`), and `Broadcast` (public) are explicit. No accidental cross-user
  leaks.
- **Authorization.** `App.Guard(event, fn)` gates events before the handler runs.
  The `who:` block (`require`/`redact`) is enforced at render: a protected facet
  refuses plain `Render` and must use `RenderFor(view, …)` (checks policies via
  `Compiled.Policy`, strips redacted fields). `fct audit` lists the whole surface.
- **CSRF.** `/events` needs the per-connection id (readable only same-origin) plus
  an Origin check.
- **DoS.** Per-IP rate limit on `/events` and per-IP SSE connection cap.
- **XSS.** `html/template` contextual auto-escaping; the `{…}` parser blocks
  `{{…}}` injection; CSP locks `script-src 'self'` (no inline/eval).
- **Tamper-evidence.** Events are HMAC-signed and verified client-side.
- **Compile-time.** Child-facet cycles, unknown children, and unknown props are
  rejected before they can run.

---

## Running in production (multi-instance)

Two things are REQUIRED to run more than one instance behind a load balancer:

**1. A stable, shared signing key.** Pushed events are HMAC-signed; a page served
by one instance must verify events from any instance and after a redeploy. Set
the same key everywhere:

```sh
export FA_SIGNING_KEY=$(openssl rand -hex 32)   # same value on every instance
```
(or `fa.New(manifest, fa.WithSigningKey(key))`). Without it each process uses a
random ephemeral key — fine for dev, logged as a warning, broken across instances.

**2. A cross-instance Broker.** The default hub is in-process; supply a pub/sub
Broker so events reach connections on other instances. A Redis adapter is ~25
lines (the framework stays dependency-free):

```go
type RedisBroker struct{ rdb *redis.Client; ctx context.Context }
func (b *RedisBroker) Publish(msg []byte) error { return b.rdb.Publish(b.ctx, "fa", msg).Err() }
func (b *RedisBroker) Subscribe(fn func([]byte)) {
    go func() { for m := range b.rdb.Subscribe(b.ctx, "fa").Channel() { fn([]byte(m.Payload)) } }()
}
// app := fa.New(manifest, fa.WithSigningKey(key), fa.WithBroker(&RedisBroker{rdb, ctx}))
```

**Deployment note:** run SSE behind **sticky load-balancing** (session affinity)
so a client's `/sse` and `/events` land on the same instance — its own connection
stays local; the Broker handles delivery to connections on other instances. This
is the standard way to run SSE/WebSocket at scale.

### Operations (built in)

- **Health/readiness.** `GET /healthz` (liveness) and `GET /readyz` (readiness —
  returns 503 once `app.Shutdown()` starts, so a load balancer drains the node).
- **Metrics.** `GET /debug/metrics` (JSON: events in/out, active/total conns,
  rate-limited, forbidden). `app.Metrics()` for the live counters.
- **Structured logging.** wrap your mux in `fa.LogRequests` (method/path/status/ms).
- **Graceful shutdown.** `app.Shutdown()` drains: marks `/readyz` unhealthy, then
  closes SSE connections. The scaffold wires SIGTERM → drain → `http.Server.Shutdown`.
- **Docker.** the scaffold ships a distroless static `Dockerfile`.

### Performance

Per-op cost (Apple-class core, `go test -bench`; reproduce with `go test ./fa -bench .`):

| Operation | Cost | ~Throughput/core |
|---|---|---|
| Render one facet | ~17 µs | ~58k/s |
| Full event dispatch (guard+handler+render) | ~18 µs | ~55k/s |
| Signed fan-out to 1000 subscribers | ~200 µs | ~5M deliveries/s |
| Cold compile (startup) | ~66 µs | one-time |

(Network load testing under real concurrency — k6/vegeta against a deployed
instance — is the production-validation step.)

### Testing your app

`github.com/F33D3R-Inc/fct/fatest` (like `httptest`) tests facets and handlers with no server:

```go
html := fatest.Render(t, src, "LikeButton", map[string]any{...})
events := fatest.Dispatch(t, app, "post.like", map[string]string{"postId": "1"})
fatest.AssertFragment(t, events, "post:1", "active")
```

### Typed data

`fct build` emits a `<Facet>Data` struct per `what:` block (idiomatic Go names —
`avatar_url` → `AvatarURL`), so you can render with compile-time-checked data:
`c.Render("LikeButton", LikeButtonData{Post: p, Count: 5, Liked: true})`. The
`map[string]any` path still works for the no-build-step flow.

### Optimistic UI

Add `data-fa-optimistic="active"` to an action element: the runtime toggles that
class **instantly** on click (zero perceived latency), then the server's
authoritative replace reconciles. If no reply arrives within the TTL, the guess
auto-reverts.

---

## Application building blocks

A render model isn't a framework on its own — real apps have many URLs, logged-in
users, validated forms, and more than one server. These are built in.

### Routing (multi-page, SPA-style)

`Route` registers a page at a URL pattern (`:name` captures a path parameter);
`MountRouter` serves them. A normal load returns the full document; a `data-nav`
link is fetched as a fragment and swapped into the page **without a reload — the
SSE connection and every live facet survive across pages**.

```go
app.Route("/", "Home", func(rc fa.RouteCtx) template.HTML { return c.MustRender("Home", nil) })
app.Route("/u/:handle", "Profile", func(rc fa.RouteCtx) template.HTML {
    u := db.UserByHandle(rc.Param("handle"))
    return c.RenderFor(rc.View(), "Profile", u) // who:-aware render
})
app.NotFound(func(rc fa.RouteCtx) template.HTML { return c.MustRender("NotFound", nil) })
app.Mount(mux)                                   // /sse, /events, runtime
app.MountRouter(mux, fa.ShellOptions{CSS: ...})  // the pages
```

A link renders client-side when marked: `<a href="/u/ada" data-nav>` (the stdlib
nav/feed facets already emit `data-nav`).

### Compile-time data safety

Every identifier a facet uses in `looks:` must be declared in `what:`. A typo or a
renamed field is a **compile error naming the facet and field** — not a blank spot
at runtime. The compiler enforces the data contract each facet states.

### Sessions & auth

Signed-cookie sessions (HMAC with the app key, HttpOnly/SameSite, tamper-rejected).
You verify credentials however you like, then record the user id:

```go
sess := app.Sessions()
app.Identify(sess.Identity)             // logged-in user → SSE delivery identity
// on login:  sess.Save(w, map[string]string{"uid": user.ID})
// on logout: sess.Clear(w)
```

### Forms & validation

A fluent validator that accumulates one error per field (binds to the stdlib
`FieldError` facet):

```go
f := fa.NewForm(r)
f.Required("email", "Email is required").Email("email", "Enter a valid email")
f.Required("password", "Required").MinLen("password", 8, "At least 8 characters")
if !f.Valid() { /* re-render the form facet with f.Errors */ }
file, hdr, _ := f.File("avatar") // multipart uploads
```

### Admin panel (the Django feature, built in)

React never had one; Django is loved for it. FA ships a built-in, **auth-gated
(deny-by-default)** admin: register your resources and it auto-generates a
navigable UI — a dashboard with live system metrics, a list view per resource, and
a detail view — mounted under any prefix.

```go
adm := fa.NewAdmin("Acme").
    Authorize(func(r *http.Request) bool { return sess.Get(r, "role") == "admin" }).
    WithMetrics(app.Metrics()).
    Resource(fa.AdminResource{
        Name: "users", Label: "Users", Columns: []string{"Handle", "Name"},
        List: func(ctx context.Context) ([]fa.AdminRow, error) { /* your data */ },
        Get:  func(ctx context.Context, id string) ([]fa.AdminField, error) { /* one record */ },
    })
adm.Mount(mux, "/admin")
```

Self-contained and server-rendered; you provide `List`/`Get`, FA renders the UI.

### Multi-instance fan-out (production broker)

A built-in, **zero-dependency** Redis-backed `Broker` (raw RESP over TCP) delivers
events across instances. Run behind sticky load-balancing:

```go
b, _ := fa.NewRedisBroker(os.Getenv("REDIS_ADDR"))
app := fa.New(manifest, fa.WithBroker(b), fa.WithSigningKey(key))
```

See "Running in production" for the deployment shape.

### Beyond the browser — native iOS / Android (the renderer-neutral path)

FA's client is a thin renderer of a wire protocol, not an HTML-bound web app. The
same way React swaps `react-dom` for `react-native` under one component tree, FA
swaps the *renderer* under one wire protocol — and goes further: **no application
logic ships to the device at all**, so web, iOS, and Android are all thin
renderers of one server.

The keystone is `Compiled.RenderTree`, which renders a facet to a **platform-neutral
view tree** instead of HTML:

```go
node, _ := c.RenderTree("TipButton", data)
// → {kind:"button", action:"tip.send", facetId:"TipButton",
//    children:[{kind:"text", text:"🪙 100"}]}
```

`kind` is abstract (`box`/`text`/`button`/`image`/`input`/`link`/`icon`), `facetId`
still targets surgical updates, and `action` is the event a tap sends. A web client
renders this to DOM; a Swift runtime renders it to UIKit/SwiftUI; a Kotlin runtime
renders it to Jetpack Compose — all consuming the same SSE protocol and posting to
the same `/events`. **State and logic stay on the server for every platform.**

**iOS and Android are built.** Two native client runtimes consume the same wire
protocol:

- `clients/swift` — **FacetKit** (SwiftUI): `FacetScreen(client:route:)` is a whole app.
- `clients/android` — **FacetKit** (Jetpack Compose): `FacetScreen(client, route)` is a whole app.

```swift
// iOS
struct ContentView: View {
    @StateObject var client = FacetClient(baseURL: URL(string: "https://app.example.com")!)
    var body: some View { FacetScreen(client: client, route: "/") }
}
```

Both load a route as a neutral tree (`FA-Native: 1`), render it to native views,
hold the SSE connection, apply surgical updates by `facetId`, and forward taps to
`/events` — **zero app logic on device**.

**Layout is server-driven, and the style table is single-source.** The server
resolves each node's `Style` (`direction`/`gap`/`pad`/`align` + `bg`/`fg`/
`fontWeight`/`radius`) from inline styles and a design-system class table
(`fa/style.go`), so native renderers lay out exactly. Native SSE connections
(`FA-Native: 1`) receive each update as an **already-styled neutral tree**, so the
clients hold no style logic at all — `fa/style.go` is the only style table.

**Pushed events are HMAC-verified on native too** (parity with the web runtime):
the signing key arrives on the `_conn` frame; the native frame's fragment is the
**signed** tree JSON, so the bytes the device renders are exactly the bytes
authenticated, and a tampered frame is dropped.

Built and tested: the server-side neutral tree + style model (`RenderTree`,
`ParseView`, `Style`; signed styled trees over SSE; proven on the real stdlib) and
both client runtimes (model, SSE, HMAC verification, surgical updates, native
renderer driven by the server style, per-primitive semantics — stream `window:`,
signal `ttl:`, vault decrypt, media mount; unit tests).

---

## Standard library (`github.com/F33D3R-Inc/fct/std`)

So you don't start from a blank file. **229 ready facets**, every one tested to
compile — enough to cover the surfaces of a real social / live-streaming / video
product (X.com, Instagram Live, Chaturbate, YouTube) without writing a component
from scratch:

- **atomic** — Button, IconButton, Icon, Avatar, Badge, Tag, Count, Spinner,
  Skeleton, Divider, Link, Toggle
- **feedback / state** — Alert, Toast, Banner, EmptyState, ProgressBar,
  ErrorState, RetryCard, LoadMoreButton, NewItemsPill, Paginator, SkeletonCard,
  OfflineBanner, EndOfFeed
- **layout / app shell** (slot-based) — Card, Stack, Row, Modal, AppShell,
  LeftRail, MainColumn, RightRail, RightRailCard, NavRail, NavRailItem, FeedTabs,
  Sidebar, TopBar, BottomNav, Grid, ScrollArea, BackBar, SectionHeader
- **form** — FormField, TextInput, TextArea, Checkbox, SubmitButton, Switch,
  RadioOption, Slider, FileUpload, SearchInput, OTPInput, CharCounter, FieldError
- **nav / search** — NavBar, NavItem, TabBar, Tab, Crumb, SearchBar,
  SearchResultPerson, TrendingItem, ExploreTile, CategoryChips
- **feed / compose** — PostCard, PostHeader, PostBody, PostActionBar, QuotedPost,
  CommentItem, WhoToFollowRow, Timeline, Composer, ReplyComposer, QuoteComposer,
  Poll, PollEditor, MediaPreview *(compose atomics; per-instance ids so a like
  updates one card surgically)*
- **media / video** — Image, VideoPlayer, AudioPlayer, GifPlayer, LinkPreview,
  SensitiveVeil, VideoControls, Scrubber, MediaGrid, Carousel, VideoCard,
  ChannelHeader, EngagementBar, ChapterList, ShortCard, PlaylistCard, WatchNextList
- **stories** — StoryRing, StoryBar, StoryProgress, StoryViewer
- **live / streaming** — LiveBadge, ViewerCount, LiveStreamPlayer, LiveChat,
  ChatMessage, TipButton, TipGoal, GiftTray, GoLiveButton, BroadcastControls,
  ReactionRail, PrivateShowBar, TokenBalance
- **commerce** — CoinBalance, SubscribeButton, Paywall, WalletCard, PriceTag, GiftItem
- **audio rooms** — SpaceCard, SpaceBar, SpeakerGrid, SpeakerTile, MicButton,
  RaiseHandButton, RoomControls
- **comments** — CommentThread, CommentNode, CommentComposer, CommentVote, PinnedComment
- **profile** — ProfileHeader, ProfileTabs, ProfileStats, BioBlock, ProfileSummary
- **overlays** — Sheet, Drawer, Dropdown, ContextMenu, Tooltip, ConfirmDialog,
  Lightbox, EmojiPicker, GifPicker
- **notifications** — NotificationItem, NotificationList, NotificationBell, ToastStack
- **settings** — SettingsSection, SettingsRow, SettingsToggleRow, AccountSwitchRow
- **analytics / studio** — StatCard, AnalyticsCard, KPIRow, Table, Sparkline, MetricDelta
- **status atoms** — StatusDot, Pill, Stars, Chip, VerifiedTick

A scaffolded project (`fct new`) is wired to the standard library out of the box —
`std.CompileDir("facets")` makes the whole catalog available to your facets by
name, and `std.CSS` is served as the default theme. To use it in an existing app:

```go
c, _ := std.CompileDir("facets")            // your facets on top of the stdlib
// or: c, _ := fa.Compile(std.Source() + myFDL)   // std.Names() lists them all
// in HandlePage opts: CSS: template.CSS(std.CSS) + appCSS
```

Use them by name — in a facet via `<Card title="Stats"> <Badge .../> </Card>`, or
directly with `c.Render("Button", ...)`.

---

## Community packages (the registry)

Create, save, and share facets like npm packages. The whole loop is built in:

```sh
fct init my-pkg social/post-card   # a package (fct.pkg.json + .fct files)
# …edit the manifest + facets…
fct pack my-pkg                    # build a .tgz (validates it compiles)
fct publish my-pkg                 # submit to the registry (FA_REGISTRY)
fct search post                    # discover packages
fct add social/post-card           # install into facets/ (validated on install)
fct registry ./store               # run your own registry (self-hostable)
```

The registry rejects packages whose facets don't compile, so you can't publish
broken code. `fct add` also takes a URL or a local path. Editor support:
`editor/vscode` (highlighting + `fct lsp` diagnostics).

---

## CLI reference

| Command | Purpose |
|---|---|
| `fct new <dir> [module]` | scaffold a runnable project |
| `fct dev [dir]` | run, rebuilding on `.fct` change |
| `fct build <file.fct> [outdir]` | compile to template + manifest + typed structs |
| `fct check <file\|dir>` | validate (parse + codegen + composition) |
| `fct fmt <file\|dir>` | format `.fct` files |
| `fct lsp` | language server (editor diagnostics) |
| `fct audit <file.fct>` | print the access-control surface |
| `fct init / pack / publish / search / add / registry` | community packages |
| `fct parse / lex / version` | debug / info |

---

## Known gaps & roadmap (in priority order)

> This section tracks **language/DX** gaps. Production/organizational gaps
> (load validation, API stability, supply chain, SSO, a11y, …) are tracked in
> **`ENTERPRISE.md`** — the enterprise-readiness scorecard.

**Core language / compiler**
- ✅ **Composition** — child facets `<Avatar …/>` and `slot:` (block-form fill).
- ✅ **Security** — scoped SSE, `who:` authz (Guard + RenderFor), CSRF, CSP, rate
  limits (see `SECURITY.md`).
1. **Named slots** — `slot name:` + `fill name:` (today: one default/children slot).
2. **Scoped styles** — a `style:` block, auto-scoped per facet.
3. **Typed codegen** — generate Go structs from `what:` instead of `map[string]any`;
   handle initialisms (`id` → `ID`, not `Id`).
- ✅ **Richer expressions** — comparisons, boolean, arithmetic, method calls, literals.
4. **Typed data + computed fields** — see #3 above (the big remaining language gap).
- ✅ **Primitive runtime semantics (server + web)** — feed ranking (`SortFeed`),
  stream `throttle` (hub coalescing) + `window` (DOM trim), lifecycle state
  transitions, signal relay + TTL expiry, `vault` client-side decrypt (AES-GCM),
  media player mounting.
- ✅ **Primitive runtime semantics — native** — window/TTL/vault/media
  enforcement in FacetKit (SwiftUI) and the Compose client, driven by the same
  manifest registry as the web runtime.

**Tooling / ecosystem**
- ✅ `fct check` / `fct fmt` / `fct lsp` (editor diagnostics) / `fct add` (registry).
- ✅ Standard library (`std`, 229 facets, wired into `fct new`) + VS Code extension.
6. **More backend targets** — codegen for Node / Python / Rust, not just Go
   (FA's pitch is language-agnostic via the compiler).
7. **Hosting** — add a hosted registry + deploy the docs site (see
   `PUBLISHING.md`); the catalog (live, video, audio rooms, commerce, settings,
   analytics) is built, and the user wiki (`wiki/`) + mkdocs config are ready
   to publish.

---

*License: MIT.*
