# Facet

**A compiler-first application language.** You describe a whole application as one
typed graph — data, state, behavior, permissions, auth, pages — and the Facet
compiler decides *where each piece runs* (server vs. client), emits a neutral
**IR**, and runs it. There is no "frontend" and no "backend" in the source: the
**placement calculus** computes that from the shape of your declarations.

```
.fct source ──▶ facet compiler ──▶ Facet IR ──▶ runtime
 (your app)     (parse · placement · deps)      (server authority + client executor + Postgres)
```

You write `.fct` and run the `facet` binary. That single file is the whole
surface — compiler, web server, JSON API, and client runtime are all inside it.

## Install

Download the binary for your platform from the
[latest release](https://github.com/F33D3R-Inc/fct/releases/latest):

- **macOS / Linux:** `chmod +x facet-*` then `sudo mv facet-* /usr/local/bin/facet`
- **Windows:** rename `facet-windows-amd64.exe` to `facet.exe`, put it on your PATH

## Quick start

```sh
facet new myapp
cd myapp
export FACET_DATABASE_URL=postgres://user:pw@localhost:5432/yourdb
facet run app.fct
```

Open <http://localhost:7373>. The same app is also a JSON API at `/api`. The first
account you sign up becomes the admin.

> Facet stores data in **Postgres** — point `FACET_DATABASE_URL` at your database.
> Same model as Django/Rails/Phoenix: you bring the database, the framework does
> the rest. Each entity is a real, typed, indexed table; relations are foreign
> keys with cascade. The schema is reconciled on startup, or explicitly with
> `facet migrate app.fct` (`--plan` to dry-run).

## The one idea

You declare **what** each thing is; the compiler infers **where** it lives.

| You write | Compiler infers |
|---|---|
| `entity Post:` | durable, shared → **server** (a Postgres table) |
| `state count: int = 0` | authoritative → **server** (per session) |
| `state draft: text = "" @client` | ephemeral/local → **client** |
| `action like(id)` (mutates an entity) | authoritative → **server** |
| `action addBonus` (mutates only client state) | → **client** (zero network) |
| `now()` / `rand()` in an action | impure → **server** (the authority owns it) |

Placement is **sound**, checked at compile time: a server action can neither read
nor write client-only state; `requires <policy>` forces server placement; a
`where`/derive/view must be pure.

## The language

```
app Social:
    auth                                   # built-in users, login, roles

    entity Post:                           # durable data (a Postgres table)
        id: int
        author: text
        body: text
        created: int

    state username: text = "" @client
    state password: text = "" @client
    state draft: text = "" @client

    derive postCount: int = count(Post)    # named, reactive computed value

    action post(body: text):               # now() ⇒ runs on the server
        add Post { author: actor, body: body, created: now() }

    view Home at "/":                       # a page at a route
        box:
            text "{postCount} posts"
            if actor == "guest":
                input bind username placeholder "username"
                input bind password placeholder "password"
                button "sign up" -> signup(username, password)
                button "log in" -> login(username, password)
            if actor != "guest":
                text "signed in as {actor} ({role})"
                button "log out" -> logout
                input bind draft placeholder "what's happening?"
                button "post" -> post(draft)
            for p in Post by created desc limit 50:   # filter · sort · paginate
                box:
                    text "{p.author}: {p.body}"
```

| Construct | Meaning |
|---|---|
| `import "file.fct"` | merge another module's declarations (above the `app` header); split an app or reuse a facet |
| `auth` | built-in users: `signup`/`login`/`logout`, `setRole`, reset, verify, MFA, OIDC SSO, `actor`/`role`/`verified` |
| `entity Name:` | durable record type; a field may reference another entity (`f: User`); `@secret` encrypts at rest; `T?` is nullable |
| `enum Name: a, b, c` | a closed text type, usable as a field/state/param type and in `select` |
| `state n: T = v [@client]` | a state cell; authoritative unless `@client`; `[T]` is a list, `T?` optional |
| `derive n: T = expr` | a named computed value (inlined, reactive) |
| `policy n[(p)]:` | a predicate over `actor`/`role`/`verified`/state; parameters make it row-level |
| `action n(p) [@optimistic]:` | a mutation: `assign`/`add`/`set`/`remove`/`clear`; `requires n(args)`; `check <cond> "msg"` |
| `job n every 30s -> a` | a scheduled server action (durable cron in a cluster) |
| `component N(p):` / `layout N:` | a reusable fragment (invoked with `use`) / a wrapper with a `slot` |
| `theme:` | a block of `name "value"` lines → CSS custom properties |
| `view N at "/p/:id" [in L] [requires pol]:` | a page: `box`·`text`·`button`·`for…where…by…limit`·`if`·`input`·`select`·`form`·`upload`·`link`·`use` |
| builtins | `count(E)` · `sum(E.f)` · `now()` · `rand(n)` |

## Projections

One graph, served three ways with no extra code:

- **Web** — server-rendered first paint, then a fine-grained client runtime.
- **Live** — entity changes fan out over SSE; every open tab converges.
- **API** — `GET /api` (schema) · `GET /api/<Entity>` · `POST /api/<action>`.
  Entity lists push down to indexed SQL: `?by=field&desc=1&limit=20`,
  `field=value` filters, and keyset cursor pagination (`?after=…`, with a `next`
  cursor in the reply) — large tables are never loaded whole.

Every projection is hardened the same way: server-enforced RBAC + row-level
policies, signed sessions, CSRF on the browser channel, rate limiting,
brute-force lockout, an admin audit feed (`GET /api/_audit`), `@secret` field
encryption, and optional OIDC SSO — set `FACET_SECRET` to key it all.

## Examples

`examples/social.fct` (auth + pages + feed), `examples/secure.fct` (RBAC +
row-level authz + `@secret`), `examples/chirp.fct`, `examples/inbox.fct`
(relations + jobs), `examples/counter.fct` (the minimal server-vs-client demo).

```sh
facet build examples/social.fct   # print the IR (the application graph)
facet run   examples/social.fct   # serve it
```

## Status

**v1.4.0 — modules & imports.** An app no longer has to be one file: a `.fct`
can `import "other.fct"`, and the compiler merges every module into one graph
before placement. That keeps a large app as many small files **and** is the
foundation for reusable "facets" (data + logic + UI bundled and pulled into any
app). See the [Modules & Imports](wiki/Modules.md) guide and
`examples/modular/`.

**v1.3.0 — every roadmap phase is shipped.** The enterprise platform
(ENTERPRISE.md Phase 6) lands on top of the earlier phases:

- **v1.3 — reliability, language depth, delivery, enterprise:** clustering over
  Postgres `LISTEN`/`NOTIFY` · durable job queue (retries/backoff/dead-letter/
  cron) · Prometheus `/metrics` + `/healthz`/`/readyz` · graceful shutdown ·
  lists/optionals/`money`/`date`/enums · `check` validation · dynamic routes,
  layouts & route guards · components · `select`/`form`/`upload` · theming ·
  SPA navigation · hot-reload dev server · `console` · `seed` · `test` · LSP +
  editor highlighting · Docker/compose · SBOM + cosign signing + SLSA
  provenance · multi-tenancy · auto-admin at `/admin` · billing ledger ·
  i18n/GDPR/retention · `facet generate` native mobile clients.
- **v1.2 — authorization & security:** RBAC + row-level policies · signed
  sessions · CSRF · rate limiting · brute-force lockout · password reset ·
  verification · TOTP MFA · OIDC SSO · admin audit log · `@secret` encryption.
- **v1.1 — data at scale:** typed indexed columns · relations with cascade · SQL
  query pushdown + keyset pagination · transactions · migrations.
- **v1.0 — foundation:** data · queries · auth · pages · projections · packaging.

**Full guide → the [Facet wiki](wiki/Home.md)** is the single reference for
building with the language. See [ENTERPRISE.md](ENTERPRISE.md) for the road that
got here. Every item is another node kind or runtime service through the **same
IR and placement calculus** — the language grows by addition, never by forking
into frontend and backend again.
```sh
go test ./...   # placement soundness, authz, security primitives, queries, auth
```
