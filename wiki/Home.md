# The Facet Wiki

**Facet is a compiler-first application language.** You describe a whole
application as one typed graph — data, state, behavior, permissions, auth, and
pages — in a single `.fct` file. The compiler decides *where each piece runs*
(server vs. client), emits a neutral **IR**, and the runtime executes it as a
web app, a live-updating UI, and a JSON API at once.

There is no "frontend" and no "backend" in the source. The **placement
calculus** computes that for you from the shape of your declarations.

```
.fct source ──▶ facet compiler ──▶ Facet IR ──▶ runtime
 (your app)     (parse · placement · deps)      (server authority + client executor + Postgres)
```

This wiki is the **single reference for building with Facet**. If you read it
top to bottom you will know everything the language can do as of **v1.3.0**.

---

## Start here

1. **[Installation](Installation.md)** — get the `facet` binary.
2. **[Getting Started](Getting-Started.md)** — build and run your first app in
   ten minutes.
3. **[The Placement Calculus](Placement-Calculus.md)** — the one idea the whole
   language is built on. Read this before anything else.

## Reference

| Page | What's in it |
|---|---|
| **[Language Reference](Language-Reference.md)** | Every keyword, every construct, the complete grammar. |
| **[Data Modeling](Data-Modeling.md)** | Entities, the type system, relations, queries, indexes, migrations. |
| **[Actions & Logic](Actions-and-Logic.md)** | Actions, statements, `derive`, validation (`check`), jobs, builtins. |
| **[Authorization & Security](Authorization-and-Security.md)** | `auth`, policies (RBAC + row-level), sessions, CSRF, MFA, SSO, audit, encryption. |
| **[Views & UI](Views-and-UI.md)** | View nodes, interpolation, components, layouts, routing, theming, uploads. |
| **[Projections & the API](Projections-and-API.md)** | Web rendering, live SSE updates, the JSON API, all HTTP routes. |
| **[Enterprise](Enterprise.md)** | Multi-tenancy, the auto-admin, billing, compliance (i18n/GDPR/retention), mobile client generation. |
| **[Operations](Operations.md)** | Deploying, clustering, observability, durable jobs, backup/restore. |
| **[CLI Reference](CLI-Reference.md)** | Every `facet` subcommand. |
| **[Configuration](Configuration.md)** | Every `FACET_*` environment variable. |
| **[Examples & Cookbook](Examples.md)** | The bundled example apps and common recipes. |

---

## The one idea, in one table

You declare **what** each thing is; the compiler infers **where** it lives.

| You write | Compiler infers |
|---|---|
| `entity Post:` | durable, shared → **server** (a Postgres table) |
| `state count: int = 0` | authoritative → **server** (per session) |
| `state draft: text = "" @client` | ephemeral/local → **client** |
| `action like(id)` (mutates an entity) | authoritative → **server** |
| `action addBonus` (mutates only client state) | → **client** (zero network) |
| `now()` / `rand()` in an action | impure → **server** (the authority owns it) |

Placement is **sound**, checked at compile time: a server action can neither
read nor write client-only state; `requires <policy>` forces server placement;
a `where`/`derive`/view expression must be pure. See
**[The Placement Calculus](Placement-Calculus.md)**.

## A complete app

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
            for p in Post by created desc limit 50:
                box:
                    text "{p.author}: {p.body}"
```

That single file is the whole surface — compiler, web server, JSON API, live
updates, and client runtime are all inside the `facet` binary.
