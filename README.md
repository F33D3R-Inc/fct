# Facet Architecture (`fct`)

**A server-authoritative web framework.** You write declarative facets in FDL
(`.fct` files); the `fct` compiler turns them into server templates and runtime
metadata. Your application runs entirely on the server — the browser (or iOS /
Android app) runs a small fixed runtime that only renders signed updates and
forwards user actions. **No application logic ever ships to a client.**

The UI is a live projection of server state, pushed over Server-Sent Events:
no API layer to design, no client state to sync, no JavaScript to write, and
no stale-cache or optimistic-update bugs — the server is the single source of
truth.

```
user clicks  →  the runtime POSTs the event to the server
             →  YOUR handler changes real state and re-renders the facet
             →  the framework signs the fragment and pushes it over SSE
             →  every targeted client verifies and swaps it in place
```

## Why FA

- **Server-authoritative** — state lives in your process/database; clients
  cannot hold state they shouldn't.
- **Zero client logic** — the web runtime is ~8 KB of fixed plumbing; native
  apps are thin renderers of the same wire protocol.
- **Secure by default** — scoped delivery, structural `who:` authorization,
  CSRF, rate limits, CSP, and HMAC-signed updates verified on every client.
- **Compile-time safety** — a typo'd field or unknown child facet is a build
  error naming the fix, not a blank spot in production.
- **One codebase, three renderers** — web (DOM), iOS (SwiftUI), Android
  (Jetpack Compose), all driven by one server.
- **Batteries included** — router, sessions, forms, admin panel, a
  229-component standard library, test harness, and multi-instance scale-out.

## Install

> **Prerequisite:** Go 1.26+ (<https://go.dev/dl/>). FA apps are Go programs —
> a single static binary. No Node, no bundler.

```sh
go install github.com/F33D3R-Inc/fct/cmd/fct@latest
fct version
```

Or download a prebuilt binary from
[Releases](https://github.com/F33D3R-Inc/fct/releases/latest) — see the
[Getting Started guide](https://github.com/F33D3R-Inc/fct/wiki/Getting-Started)
for platform-specific steps (macOS Gatekeeper, Windows PATH).

## Quick start

```sh
fct new myapp
cd myapp
go run .            # open http://localhost:7373
fct dev             # same, but rebuilds on every .fct save
```

You get a live multi-page app out of the box: a `Home` facet and a working
`LikeButton` — click it and it updates with no page reload, no fetch call, and
no client code.

## What a facet looks like

```
facet LikeButton:
    what:                      # the data this facet needs (compile-checked)
        post: Post
        count: int
        liked: bool

    looks:                     # the HTML (server-rendered, auto-escaped)
        <button data-action="post.like" data-post-id="{post.id}">
            if liked:
                ♥ {count}
            else:
                ♡ {count}
        </button>
```

And the entire server side of that interaction:

```go
app.On("post.like", func(ctx fa.Ctx) ([]fa.Event, error) {
    post.Toggle(ctx.Payload["postId"])              // change real state
    return []fa.Event{{Op: "replace",
        FacetID:  "LikeButton:post:" + ctx.Payload["postId"],
        Fragment: string(renderLike()),             // push the new HTML
    }}, nil
})
```

## Documentation

**The [wiki](https://github.com/F33D3R-Inc/fct/wiki) is the place for
everything** — guides, reference, and help. Questions, bug reports, and facet
contributions all happen here on GitHub.

| Topic | Where |
|---|---|
| Getting started | [wiki/Getting-Started](https://github.com/F33D3R-Inc/fct/wiki/Getting-Started) |
| Build your first website (tutorial) | [wiki/Building-Your-First-Website](https://github.com/F33D3R-Inc/fct/wiki/Building-Your-First-Website) |
| Databases (Postgres, SQLite) | [wiki/Working-with-Databases](https://github.com/F33D3R-Inc/fct/wiki/Working-with-Databases) |
| Deployment (Docker, scaling) | [wiki/Deployment](https://github.com/F33D3R-Inc/fct/wiki/Deployment) |
| FDL language reference | [wiki/FDL-Reference](https://github.com/F33D3R-Inc/fct/wiki/FDL-Reference) |
| Standard library (229 facets) | [wiki/Standard-Library](https://github.com/F33D3R-Inc/fct/wiki/Standard-Library) |
| Sessions, auth & forms | [wiki/Sessions-Auth-and-Forms](https://github.com/F33D3R-Inc/fct/wiki/Sessions-Auth-and-Forms) |
| Realtime patterns | [wiki/Realtime-Patterns](https://github.com/F33D3R-Inc/fct/wiki/Realtime-Patterns) |
| Native iOS / Android | [wiki/Native-Clients](https://github.com/F33D3R-Inc/fct/wiki/Native-Clients) |
| Testing | [wiki/Testing](https://github.com/F33D3R-Inc/fct/wiki/Testing) |
| Troubleshooting & FAQ | [wiki/Troubleshooting](https://github.com/F33D3R-Inc/fct/wiki/Troubleshooting) |
| Technical architecture | [ARCHITECTURE.md](ARCHITECTURE.md) |
| Migrating from React | [REACT_MIGRATION.md](REACT_MIGRATION.md) |
| Design decisions (ADRs) | [DECISIONS.md](DECISIONS.md) |

> The same pages live in [`wiki/`](wiki/) in this repo (the versioned source of
> the GitHub wiki).

## The primitive taxonomy

FDL has 8 primitives, each with enforced runtime behavior on the server, the
web runtime, and both native runtimes:

| Primitive | Role |
|---|---|
| `facet` | reactive UI fragment |
| `feed` | ranked, ordered list (`order:`) |
| `stream` | append-only, high-frequency (`throttle:`, `window:`) |
| `lifecycle` | multi-step state machine (`states:`) |
| `pipe` | continuous data (`throttle:`) |
| `signal` | ephemeral peer state — relayed, never stored (`ttl:`) |
| `vault` | E2E-encrypted content — the **server never renders it** (`decrypt:`) |
| `media` | binary delivery: video/audio (`source:`) |

`vault` is the architectural line: the compiler emits **zero server template**
for vault content, so even a compromised server cannot produce plaintext.
Details: [ARCHITECTURE.md](ARCHITECTURE.md#the-primitive-taxonomy).

## Production

An FA app is one static binary with health checks, graceful drain, metrics,
and a distroless Dockerfile scaffolded for you. Two rules for multi-instance:
a shared `FA_SIGNING_KEY`, and the built-in Redis broker behind a sticky load
balancer. The full story — compose files, proxy configs, checklists — is in
[wiki/Deployment](https://github.com/F33D3R-Inc/fct/wiki/Deployment).

## Status

**Functional end to end, pre-1.0** (minor versions may break — see
[CHANGELOG.md](CHANGELOG.md)). Implemented and tested: the full compiler
pipeline, the server runtime and security suite, per-primitive runtime
semantics on web **and** native (FacetKit SwiftUI / Compose), typed codegen,
the standard library, the admin panel, multi-instance fan-out, and the
community package registry.

- What's next (features): [ROADMAP.md](ROADMAP.md)
- Production maturity, honestly tracked: [ENTERPRISE.md](ENTERPRISE.md)
- Reporting vulnerabilities: [SECURITY.md](SECURITY.md)

## Community

Share facets like packages: `fct init` → `fct pack` → `fct publish`, and
install with `fct add social/post-card` (everything is compile-validated on
publish *and* install). Want your facet in the standard library? See
[wiki/Community-Packages](https://github.com/F33D3R-Inc/fct/wiki/Community-Packages)
— submissions happen through GitHub.

Contributing to the framework itself: [CONTRIBUTING.md](CONTRIBUTING.md) ·
governance: [GOVERNANCE.md](GOVERNANCE.md).

## License

MIT — see [LICENSE](LICENSE).
