# Actions & Logic

Behavior in Facet lives in **actions**, **derives**, **jobs**, and the
expressions that thread through them.

## Actions

An action is the only thing that changes data. You declare its parameters,
optional permission checks, optional input validation, and a body of statements.

```
action name(p1: T1, p2: T2) [@optimistic]:
    requires <policy>[, <policy>(args)]
    check <cond> "message"
    <statements>
```

Its **placement is computed** — you never write `server`/`client`:

- writes an entity or server state → **server**;
- writes only `@client` state → **client** (no network);
- calls `now()`/`rand()` or has a `requires` → **server**.

See [Placement Calculus](Placement-Calculus.md).

### Statements

| Statement | Effect | Example |
|---|---|---|
| `target = expr` | assign a state cell | `count = count + 1` |
| `add Entity { f: e, ... }` | insert a row (id auto-assigned) | `add Post { author: actor, body: body, created: now() }` |
| `set Entity(key).field = expr` | update one field of one row | `set Post(id).likes = Post(id).likes + 1` |
| `remove Entity(key)` | delete a row (cascades) | `remove Post(id)` |
| `clear Entity` | delete every row | `clear Post` |

A multi-statement server action is **one transaction** — all or nothing
([Data Modeling → Transactions](Data-Modeling.md#transactions)).

### Validation with `check`

`check` rejects bad input with a friendly message. It runs **in source order**, so
a check may sit after a `let` and validate a bound result — including a brain's
answer from a [request→response](Services.md) call:

```
action transfer(to: int, cents: money):
    check cents > 0 "amount must be positive"
    check to != actor "you cannot transfer to yourself"
    ...

action enroll(handle: text, sig: text):
    let uuid = call Verity.verify(handle, sig)
    check uuid != "" "device verification failed"   # validates the bound result
    add Account { handle: handle, pid: uuid }
```

A failed check aborts the action and returns its message to the caller (HTTP and
UI alike — it also surfaces via `failed(<action>)`).

**Checks (and `let` binds) must come before any mutation** (add/set/remove/clear/
assign/establish). That guarantees a failed check — or a failed brain call — aborts
with nothing written and nothing to roll back. Put validation first, then mutate.

### Permissions with `requires`

`requires` names one or more [policies](Authorization-and-Security.md#policies).
A row-level policy takes arguments bound from the call site:

```
policy mine(id: int):
    actor == Post(id).author

action erase(id: int):
    requires mine(id)
    remove Post(id)
```

The authority enforces every `requires` no matter what a client sends.

### Optimistic actions

`@optimistic` lets the client predict the result before the server round-trip
returns, then reconcile with the authoritative response — useful for likes,
toggles, and counters.

```
action like(id: int) @optimistic:
    set Post(id).likes = Post(id).likes + 1
```

## Derives

A `derive` is a named, **pure**, **reactive** computed value. The compiler
inlines it everywhere it's read and recomputes it when its inputs change — on
the client, off its live mirror of the data — so counts and totals move the
instant anyone writes.

```
derive postCount: int = count(Post)
derive totalLikes: int = sum(Post.likes)
derive total: int = count + bonus       # spans server+client -> client-side
```

Derives may not be impure and may not depend on something invisible at their
evaluation site (the compiler checks this).

## Jobs

A `job` runs a server action on a schedule with no user in the loop, under the
synthetic `system` actor.

```
action purge:
    clear Message

job cleanup every 1h -> purge      # on an interval (s/m/h)
job warmup on start -> seedCache   # once at startup
```

In a cluster, the durable queue guarantees the job fires **once across all
instances**, with retries/backoff/dead-letter on failure. See
[Operations → Durable jobs](Operations.md#durable-jobs).

## Triggers

A `job` is driven by a clock; a **trigger** is driven by a domain event. `on
<action> -> <reaction>` runs a reaction whenever the source action completes
successfully — the non-cron sibling of a job.

```
action post(body: text):
    add Post { author: actor, body: body }
action fanout:
    add Notice { msg: "new post" }

on post -> fanout      # when `post` succeeds, run `fanout`
```

- The reaction runs **synchronously after the source action commits**, so its
  effects fan out in the same request. It runs under the `system` actor (admin
  authority), like a job — so it passes policies a trusted internal caller would.
- A reaction is a **zero-argument server action** (same rule as a job target). It
  reads state and entities to do its work.
- Triggers **chain**: a reaction's own success fires its triggers, so `on a -> b`
  and `on b -> c` runs `a → b → c`. The compiler proves the trigger graph is
  **acyclic** — a cycle (`on a -> b`, `on b -> a`) is a compile error, so reactions
  always terminate.
- Only a **successful** action fires its triggers; a denied or failed action fires
  nothing.

### Grammar

```
on <action> -> <reaction>
```

A runnable demo is in `examples/trigger.fct`.

## Expressions and builtins

Expressions appear in derives, policies, checks, `where`/`if` conditions,
statement values, action arguments, and `{…}` interpolation.

- literals · references (`actor`, `role`, `verified`, state, derives, loop vars,
  params, `tenant`/`tenantRole`) · field access (`p.body`) · entity lookup
  (`Post(id).author`, `User(m.to).name`) · arithmetic · comparison · boolean.

| Group | Builtins | Effect |
|---|---|---|
| aggregates | `count(Entity)`, `sum(Entity.field)` | pure |
| effectful | `now()`, `rand(n)` | **→ server** |
| math | `abs`, `min`, `max`, `floor`, `round` | pure |
| string | `len`, `upper`, `lower`, `trim`, `take`, `contains` | pure |
| money | `money(cents)` → `"19.99"` | pure |
| date | `year`, `month`, `day` | pure |
| formatting | `ago(ts)` → `"20h"`, `compact(n)` → `"1.3K"`, `commas(n)` → `"1,352"` | pure (render-time) |

The full list with arities is in the
[Language Reference](Language-Reference.md#builtins).

→ Next: **[Authorization & Security](Authorization-and-Security.md)**.
