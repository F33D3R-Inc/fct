# The Placement Calculus

This is the one idea Facet is built on. Everything else is detail.

## The problem it removes

Every other stack makes you split one feature across two codebases. A "like
button" is a React component, a fetch call, an API route, a controller, a
validation layer, an ORM model, and a database table — written in two languages,
deployed as two services, kept in sync by hand. The split between "frontend" and
"backend" is not in the *problem*; it's an artifact of the *tools*.

Facet deletes the split. You write **one** declarative graph. The compiler reads
the **lifecycle and effects** of each declaration and computes where it must run.

## The two domains

| Domain | What lives there | Why |
|---|---|---|
| **Server** (the authority) | durable data, shared/authoritative state, anything secured or impure | the single source of truth; the only place you can trust |
| **Client** (the executor) | ephemeral, local, latency-sensitive state | instant feedback, no round-trip |

You never type `server` or `client` as a placement. The compiler emits it into
the IR (`"placement": "server" | "client"`).

## The rules

The compiler assigns placement by these rules, then **checks the result is
sound**:

1. **Entities are server.** They are durable and shared → a Postgres table.
2. **State is server by default.** A `state` cell is authoritative unless you
   mark it `@client`. `@client` means ephemeral and local (a draft, a toggle, a
   form field) — it lives in the browser and never reaches the authority.
3. **An action's placement follows the state it touches.**
   - Writes an entity or server state → **server action** (round-trips,
     authoritative).
   - Writes only `@client` state → **client action** (runs locally, zero
     network).
4. **Impurity forces the server.** Calling an effectful builtin (`now()`,
   `rand()`) makes an action impure, so it is pinned to the authority — every
   client sees one agreed timestamp, not its own wall clock.
   - **Exception:** an impure action that writes *only* `@client` state runs on
     the client. "One agreed result" is a rule about a shared result, and
     per-browser state has none — `seenAt = now()` into a client cell is
     exactly the "mark what I have already seen" pattern, and without the
     exception it could be placed nowhere (the authority must run it, and the
     authority cannot write client state). Anything shared — an entity write,
     a `@server` cell, a service call, `requires` — still pins the action to
     the authority.
5. **`requires <policy>` forces the server.** A permission check is only
   meaningful where it can be trusted, so any action with a `requires` clause
   runs on the authority.
6. **Pure, read-only expressions can run anywhere.** A `derive`, a `where`
   filter, an `if` condition, or a text interpolation is recomputed on the
   client off its live mirror of the data — that is what makes the UI reactive.

## Soundness — what the compiler refuses

Placement is not a hint; it is **checked at compile time**. The compiler rejects
a program where placement would be unsound:

- A **server action cannot read or write `@client` state.** The authority cannot
  see the browser's local state, so depending on it is a compile error.
- A **`derive`/`where`/view expression cannot be impure** and cannot depend on
  something invisible at its evaluation site.
- A value that spans both domains can only be computed where every input is
  visible. In `examples/counter.fct`, `total = count + bonus` mixes server
  `count` and client `bonus`, so it is computed on the **client** (the only side
  that can see both) and refreshes whenever either changes.

Because the result is checked, you can read a `.fct` file and know that nothing
secret leaks to a client and nothing authoritative is decided by one.

## See it

`examples/counter.fct` is the whole thesis in 20 lines:

```
app Counter:
    state count: int = 0            # authoritative  -> SERVER
    state bonus: int = 0 @client    # ephemeral/local -> CLIENT

    derive total: int = count + bonus   # spans both -> computed on the CLIENT

    action increment:               # writes count -> SERVER action (round-trips)
        count = count + 1

    action addBonus:                # writes bonus -> CLIENT action (no network)
        bonus = bonus + 1

    view Main:
        box:
            text "Server count: {count}"
            text "Client bonus: {bonus}"
            text "Total: {total}"
            button "increment (server)" -> increment
            button "add bonus (client)" -> addBonus
```

Run `facet build examples/counter.fct` and look at the `"placement"` field on
each action and state in the IR — the compiler wrote it, you didn't.

## Why it matters

- **One language, one file, one mental model.** No serialization boundary to
  design, no API to keep in sync.
- **Security by construction.** The authority/client boundary is enforced by the
  type system, not by remembering to check.
- **The language grows by addition.** Every feature in
  [ENTERPRISE.md](../ENTERPRISE.md) — relations, RBAC, jobs, tenancy, billing —
  is another node kind or runtime service through this *same* model. Nothing
  ever forks back into "frontend" and "backend."

→ Next: **[Language Reference](Language-Reference.md)**.
