# Language Reference

The complete syntax of Facet as of v1.3.0. Facet is **whitespace-significant**
(indentation defines blocks, like Python) and every file is one `app`.

- [File structure](#file-structure)
- [Types](#types)
- [`app`](#app)
- [`auth`](#auth)
- [`entity`](#entity)
- [`enum`](#enum)
- [`state`](#state)
- [`derive`](#derive)
- [`policy`](#policy)
- [`action`](#action)
- [`job`](#job)
- [`component` and `layout`](#component-and-layout)
- [`theme`](#theme)
- [`view`](#view)
- [View nodes](#view-nodes)
- [Expressions](#expressions)
- [Builtins](#builtins)
- [Comments](#comments)

---

## File structure

```
app Name:
    auth                      # optional
    enum   ...                # zero or more, any order
    entity ...
    state  ...
    derive ...
    policy ...
    action ...
    job    ...
    component ...
    layout ...
    theme:
    view   ...                # one or more
```

Everything is nested under `app Name:` by indentation. Declaration order does
not matter — the compiler resolves references in any order.

## Types

| Type | Meaning | Storage |
|---|---|---|
| `int` | 64-bit integer | `BIGINT` |
| `text` | string | `TEXT` |
| `bool` | boolean | `BOOLEAN` |
| `money` | integer minor units (cents) | `BIGINT` |
| `date` | a timestamp (unix seconds) | `BIGINT` |
| `EntityName` | a **relation** — stores the referenced row's id | `BIGINT` foreign key, `ON DELETE CASCADE` |
| `EnumName` | a **closed text type** (see [`enum`](#enum)) | `TEXT` |

Two modifiers apply to entity fields, state cells, and parameters:

- **`T?`** — optional / nullable.
- **`[T]`** — a list of `T` (state cells only; default `[]`).

On entity fields only:

- **`@secret`** — the column is AES-256-GCM encrypted at rest (see
  [Security](Authorization-and-Security.md#secret-field-encryption)).

## `app`

```
app Social:
    ...
```

Declares the application and its name. Exactly one per file; everything else is
indented beneath it.

## `auth`

```
app Social:
    auth
```

A single bare line that turns on built-in users. It gives you, for free:

- the actions `signup`, `login`, `logout`, `setRole`, password reset, account
  verification, and TOTP MFA enrollment/verification;
- the scope values `actor` (the signed-in username, or `"guest"`), `role`
  (`admin` | `member` | `guest`), and `verified` (bool);
- optional OIDC single sign-on (configured from the environment).

The **first account to sign up becomes the admin.** See
**[Authorization & Security](Authorization-and-Security.md)** for the full model.

## `entity`

A durable, shared record type — one Postgres table.

```
entity Post:
    id: int                 # every entity has an int id (the primary key)
    author: text
    body: text
    created: int

entity Message:
    id: int
    to: User                # a relation: stored as the User's id (a foreign key)
    body: text
    secret: text @secret    # encrypted at rest
    nickname: text?         # nullable
    sent: date
```

- Give every entity an `id: int`.
- A field whose type is another entity name is a **relation**. The column stores
  that row's id; deletes cascade (removing a `User` removes their `Message`s, in
  the database and the live in-memory set). Read across it with a nested lookup:
  `User(m.to).name`.
- The compiler builds a **database index** for every field it sees filtered,
  ordered, or used as a relation, so reads stay fast as the table grows.

See **[Data Modeling](Data-Modeling.md)**.

## `enum`

A closed text type with ordered members.

```
enum Status: draft, published, archived
```

Use `Status` as a field/state/param type. A bare `select bind cell` over an
enum-typed cell defaults its options to the members. Reference a member as a
literal where a value is expected.

## `state`

```
state name: Type = default [@client | @server]
```

A state cell. **Authoritative (server) by default**; mark it `@client` for
ephemeral, local, browser-only state. `@server` is the (rarely needed) explicit
form of the default.

```
state count: int = 0                 # server (per session)
state draft: text = "" @client       # client (never reaches the authority)
state tags: [text] = [] @client      # a list cell
state nickname: text? @client        # optional, no default needed
```

## `derive`

```
derive name: Type = expression
```

A named computed value. It is **pure** and **reactive**: the compiler inlines it
at every use site and recomputes it wherever its inputs change (on the client,
off its live mirror of the data).

```
derive postCount: int = count(Post)
derive totalLikes: int = sum(Post.likes)
derive total: int = count + bonus     # spans server + client -> computed client-side
```

## `policy`

A named predicate, enforced on the authority and also shipped so the UI can hide
what the actor may not do.

```
policy admin:                          # a plain permission (zero params)
    role == "admin"

policy mine(id: int):                  # row-level (parameterized)
    actor == Post(id).author
```

A **zero-parameter** policy is a permission. A **parameterized** policy is
*row-level*: the gate binds its parameters from the `requires` call site's
arguments before evaluating. Reference policies from an action with `requires`,
or guard a route with `view … requires <policy>`.

## `action`

The only thing that changes data. Placement is computed (see
[Placement Calculus](Placement-Calculus.md)).

```
action name(p1: T1, p2: T2) [@optimistic]:
    requires policyA, policyB(arg)     # optional, comma-separated
    check <cond> "message"             # optional, zero or more
    <statement>
    <statement>
```

**Statements** (the body):

| Statement | Effect |
|---|---|
| `target = expr` | **assign** — update a server/client state cell |
| `add Entity { f: e, ... }` | insert a new row (id auto-assigned) |
| `set Entity(key).field = expr` | update one field of one row |
| `remove Entity(key)` | delete a row (cascades to children) |
| `clear Entity` | delete every row of the entity |

**`requires`** — one or more policy checks; a row-level policy takes arguments
(`requires mine(id)`). Any `requires` forces server placement.

**`check`** — an input precondition: `check qty > 0 "quantity must be positive"`.
Checks run *before* the body; a failed check aborts the action with its message.

**`@optimistic`** — the client predicts the result before the round-trip
completes, then reconciles with the authority's response.

```
action post(body: text):
    check body != "" "say something"
    add Post { author: actor, body: body, likes: 0, created: now() }

action like(id: int) @optimistic:
    set Post(id).likes = Post(id).likes + 1

action erase(id: int):
    requires mine(id)
    remove Post(id)
```

A multi-statement server action runs in **one database transaction** — all of
it commits, or none of it does.

## `job`

A scheduled server action — no user in the loop, run under the synthetic
`system` actor.

```
job cleanup every 1h -> purge          # every interval
job warmup on start -> seedCache        # once at startup
```

Intervals accept `s`, `m`, `h` (e.g. `30s`, `5m`, `1h`). In a cluster the job
fires **once across all instances** via the durable queue (see
[Operations → Durable jobs](Operations.md#durable-jobs)).

## `component` and `layout`

A **component** is a reusable view fragment with parameters, invoked with `use`:

```
component Avatar(name: text, size: int):
    box:
        text "{name}"

view Home at "/":
    box:
        use Avatar("ada", 48)
```

A **layout** wraps pages around a `slot`. A view opts in with `in Layout`:

```
layout Shell:
    box:
        text "My App"
        slot                  # the page renders here

view Home at "/" in Shell:
    box:
        text "home"
```

## `theme`

A block of `name "value"` lines, each emitted as a CSS custom property
(`--fa-<name>`) the rendered pages use.

```
theme:
    accent "#5b8cff"
    bg "#0f1115"
    radius "8px"
```

## `view`

A page bound to a route.

```
view Name [at "/path"] [in Layout] [requires policy]:
    <node>
    <node>
```

- **`at "/path"`** — the URL. Omit it and the view is the default page. A path
  segment of the form `:name` is a **dynamic parameter** (e.g.
  `view Post at "/post/:id":`), available in the page scope.
- **`in Layout`** — render inside a [`layout`](#component-and-layout).
- **`requires policy`** — a **route guard**: the authority refuses to render the
  page unless the (zero-arg) policy passes, and the client hides links to it.

## View nodes

The body of a `view`, `component`, or `layout` is a tree of nodes:

| Node | Syntax | Meaning |
|---|---|---|
| `box` | `box:` + children | a container (groups + nests) |
| `text` | `text "literal {expr}"` | text with `{…}` interpolation |
| `button` | `button "label" -> action(args)` | invokes an action on click |
| `input` | `input bind cell [placeholder "…"]` | two-way bound text input |
| `select` | `select bind cell:` + `option "Label" -> "value"` | dropdown (enum cells default their options) |
| `form` | `form "Submit" -> action(args):` + children | groups inputs; submits to an action |
| `upload` | `upload bind urlCell [label "…"]` | file/media upload; stores the URL in the cell |
| `link` | `link "label" -> "/path"` | SPA navigation to a route |
| `if` | `if <cond>:` + children | conditional region (reactive) |
| `for` | `for x in Coll [where c] [by f desc\|asc] [limit n]:` + children | a query/list region |
| `use` | `use Component(args)` | render a component |
| `slot` | `slot` | in a layout: where the page renders |

**`for`** is the query primitive — filter (`where`), sort (`by … asc|desc`), and
paginate (`limit`). In the JSON API the same clauses push down to indexed SQL.
See **[Data Modeling → Queries](Data-Modeling.md#queries)**.

```
for p in Post where p.author == actor by created desc limit 50:
    box:
        text "{p.author}: {p.body}"
        button "delete" -> remove(p.id)
```

## Expressions

Used in `derive`, `policy`, `check`, `where`, `if`, `set`/`assign` values, action
arguments, and `{…}` interpolation.

- **Literals** — `42`, `"text"`, `true`, `false`.
- **References** — a state cell, a derive, `actor`, `role`, `verified`, a loop
  variable, an action/policy parameter, and (under multi-tenancy) `tenant` /
  `tenantRole`.
- **Field access** — `p.body`, `m.to`.
- **Entity lookup** — `Post(id)` (a row by id), `Post(id).author` (a field of
  it), `User(m.to).name` (across a relation).
- **Operators** — `+ - * /`, comparison `== != < <= > >=`, boolean `&& || !`.
- **Calls** — the [builtins](#builtins) below.

Interpolation embeds an expression in text: `text "{postCount} posts by {actor}"`.

## Builtins

The builtins are a small **pure standard library** plus two effectful clock/RNG
functions. Every pure builtin is evaluated identically on the server and in the
client runtime, so all executors agree.

**Aggregates** (pure):

| Builtin | Result |
|---|---|
| `count(Entity)` | row count |
| `sum(Entity.field)` | sum of a numeric field |

**Effectful** (force server placement — see [Placement](Placement-Calculus.md)):

| Builtin | Result |
|---|---|
| `now()` | the server clock (unix seconds) |
| `rand(n)` | a random int in `[0, n)` |

**Math** (pure): `abs(n)` · `min(a, b)` · `max(a, b)` · `floor(n)` · `round(n)`
(integers only, so `floor`/`round` are identity).

**String** (pure): `len(s)` (rune count, or list length) · `upper(s)` ·
`lower(s)` · `trim(s)`.

**Money** (pure): `money(cents)` → the canonical two-decimal string
(`money(1999)` → `"19.99"`). Store `money` as integer minor units; use this to
display it.

**Date** (pure): `year(ts)` · `month(ts)` · `day(ts)` — components of a unix
seconds timestamp (UTC). Pair with `now()`.

## Comments

`#` to end of line.

```
# this is a comment
state count: int = 0    # and so is this
```

→ Next: **[Data Modeling](Data-Modeling.md)**.
