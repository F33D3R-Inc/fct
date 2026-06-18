# FA server runtimes (non-Go targets)

These are the server runtimes for the non-Go compiler targets — the `fa/` package
ported to each language. They turn the compiler's **neutral** output into a live
FA app: serve the page, hold the SSE stream, run `when:` handlers, re-render
facets from the render IR, and push **HMAC-signed** fragments the shared browser
runtime verifies and swaps in.

| Runtime | Dir | Dependencies | Notes |
|---|---|---|---|
| Node.js | `node/` | none (built-ins) | `crypto` for HMAC |
| Python | `python/` | none (stdlib) | `http.server`, `hmac` |
| Rust | `rust/` | none (std only) | hand-rolled SHA-256/HMAC, JSON, HTTP |

All three speak the **same wire format, signing layout, and manifest** as the Go
runtime (`fa/wire.go`, `fa/event.go`) and serve the same client
(`runtime/fa-runtime.js`). The contract — not a per-language reinvention — is what
makes one compiler drive all of them. See `../docs/BACKENDS.md`.

## How it fits together

```
.fct ──fct build (target=node|python|rust)──▶ generated/
                                                ├─ manifest.json   (shared, neutral)
                                                ├─ render.json      (neutral render IR)
                                                └─ types.{ts,py,rs}  (typed data)

runtime  ──reads render.json──▶ render-IR interpreter ──▶ HTML fragment
         ──signs (HMAC-SHA256)──▶ SSE ──▶ fa-runtime.js verifies + swaps
```

The render IR (`render.json`) is a flat op stream (`text`/`expr`/`if`/`else`/
`end`/`for`/`child`) with expressions as a neutral JSON AST. Each runtime ships an
identical ~120-line interpreter + evaluator over it.

## Running a demo

Each runtime has a `demo/` (or `bin/demo.rs`) that wires the scaffold `LikeButton`.
First compile the facet to that target's IR, then run:

**Node**
```sh
cd runtimes/node
fct build ../../cmd/fct/scaffold/facets/like_button.fct demo/generated   # fct.toml target = "node"
node demo/server.js          # http://localhost:7373
```

**Python**
```sh
cd runtimes/python
fct build ../../cmd/fct/scaffold/facets/like_button.fct demo/generated   # fct.toml target = "python"
python3 demo/server.py
```

**Rust**
```sh
cd runtimes/rust
fct build ../../cmd/fct/scaffold/facets/like_button.fct generated        # fct.toml target = "rust"
cargo run --bin demo
```

Set `FA_SIGNING_KEY` (hex) to enable signing; unset, events are unsigned (dev) and
the client skips verification — same semantics as the Go runtime.

## Mobile (Swift / Kotlin)

The native clients (`clients/swift` FacetKit, `clients/android` facetkit) work
against all three runtimes. A native client connects with `FA-Native: 1` and
receives a platform-neutral **ViewNode tree** instead of HTML: each runtime
renders the same HTML, then parses it into the tree (mirror of Go's
`RenderTree = ParseView(Render(...))`, in `native.js` / `native.py` / `native.rs`).
`GET <route>` with `FA-Native: 1` returns `{title, tree}`; an SSE connection with
that header is pushed ViewNode-tree fragments, signed identically. The emitted
tree JSON is **byte-identical to Go's `fa.ParseView`** (verified across all three).

## Framework parity (uniform with `fa/`)

The "batteries" are now identical across every backend language — no Go-only
tier. Each runtime has a `framework` module porting `fa/session.go`,
`fa/authz.go`, `fa/security.go`, `fa/form.go`, `fa/broker.go`:

- **Signed-cookie sessions** — HMAC layout matches Go byte-for-byte; a cookie
  minted by a Go server reads on Node/Python/Rust and vice versa (verified: all
  four mint the identical `fa_session=…` and reject tampered cookies).
- **`who:` authorization** — `require` policy gate (denied → empty render) and
  `redact` field stripping, enforced from the IR's `who` block, threaded through
  child facets. (Verified: anon denied, member allowed, field redacted — same in
  all three.)
- **CSRF / same-origin** — cross-origin POST `/events` → 403.
- **Per-IP rate limiting** — token bucket on `/events` → 429 over burst.
- **Security headers** — CSP + `X-Content-Type-Options` + `Referrer-Policy` on
  the shell.
- **Forms** — `parse` + chainable validators (`required`/`min_len`/`email`/…).
- **Broker** — pluggable `publish`/`subscribe`; in-process default; a Redis/NATS
  adapter is opt-in with the same interface in every language.

Register via the same shape everywhere: `app.identify(...)`, `app.policy(name,
fn)`, `app.sessions()` (Node/Python) / `with_sessions()` (Rust), `app.broker(b)`.

## Observability, auth & admin (also uniform)

- **Password store** — `app.auth()`: PBKDF2-HMAC-SHA256 hashing
  (`pbkdf2-sha256$iter$salt$key`) identical to `fa/auth.go`, so a password hash
  made by any backend verifies on the others. `signup`/`login`, min-length +
  taken-login checks, constant-time verify. (Verified: Go↔Node↔Python↔Rust hashes
  cross-verify.)
- **Observability** — `GET /healthz`, `/readyz`, `/debug/metrics` (JSON), and
  `/metrics` (Prometheus exposition) with the same counters as `fa/observe.go`
  (`events_in`/`out`, `conns_active`/`total`, `rate_limited`, `forbidden`).
- **Admin panel** — `app.admin(...)`: deny-by-default (no `authorize` ⇒ 403), a
  live metrics + connections dashboard, and resource list/detail views.

## Status

Implemented in all three, at parity with `fa/`: the core live-render loop (page,
SSE, signing, `when:` re-render, render-IR interpretation incl. child facets /
`if` / `for`), the native neutral-tree path, the framework surface (sessions,
`who:`, CSRF, rate limiting, forms, broker), and observability / auth / admin.
There is no Go-only tier.
