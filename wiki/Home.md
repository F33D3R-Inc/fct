# Facet Architecture Wiki

**FA is a server-authoritative web framework.** You write **facets** in FDL
(`.fct` files) — server-rendered HTML fragments wired to events. The browser
runs one fixed ~8 KB runtime that is pure plumbing: it holds an SSE connection,
swaps signed HTML fragments in by id, and forwards clicks to the server. No
JavaScript to write, no API layer, no client state to sync. Your app is a Go
program; the server is the single source of truth.

If you know Django: it feels like "templates + views," except the page updates
live without reloads. If you know React: read
[Migrating from React](../REACT_MIGRATION.md) — FA is the opposite architecture
on purpose.

## Start here

1. **[Getting Started](Getting-Started.md)** — install `fct`, scaffold a
   project, see a live page in two minutes.
2. **[Building Your First Website](Building-Your-First-Website.md)** — the full
   tutorial: pages, facets, events, forms, login. Build a working guestbook.
3. **[Working with Databases](Working-with-Databases.md)** — hook the app up to
   Postgres (or SQLite), with migrations and Docker for local dev.
4. **[Deployment](Deployment.md)** — Docker, docker-compose, reverse proxies,
   multiple instances, health checks.

## Reference

- **[FDL Reference](FDL-Reference.md)** — the language: all 8 primitives,
  blocks, expressions, template syntax.
- **[Standard Library](Standard-Library.md)** — 229 ready-made facets and how
  to use them.
- **[Sessions, Auth & Forms](Sessions-Auth-and-Forms.md)** — logged-in users,
  guarded events, validated forms, file uploads.
- **[Realtime Patterns](Realtime-Patterns.md)** — who sees an update:
  `EmitTo`, `EmitChannel`, `Broadcast`, signals, streams.
- **[Admin Panel](Admin-Panel.md)** — the built-in, auth-gated admin.
- **[Testing](Testing.md)** — unit-test facets and handlers with `fatest`.
- **[Native Clients](Native-Clients.md)** — the same app on iOS (SwiftUI) and
  Android (Compose) with zero app logic on the device.
- **[Troubleshooting & FAQ](Troubleshooting.md)** — common errors and what
  they mean.

## How a click works (the whole mental model)

```
user clicks an element with data-action="post.like"
  → the runtime POSTs {type:"post.like", payload:{…}} to /events
  → YOUR Go handler runs, changes real state (memory, Postgres, anywhere)
  → it returns new HTML for the affected facet(s)
  → the framework HMAC-signs the fragment and pushes it over SSE
  → the runtime verifies the signature and swaps the node in place
```

That's the entire framework. Everything in this wiki is a variation on that
loop.

## Project status

The core loop, security suite, router, sessions, forms, admin, std library,
Redis scale-out, and native clients are built and tested. Pre-1.0: minor
versions may break (see [ENTERPRISE.md](../ENTERPRISE.md) for the remaining
path to a stability commitment).
