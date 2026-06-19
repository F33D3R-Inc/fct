# Examples & Cookbook

The repo ships five example apps under `examples/`. Each is a complete,
runnable `.fct` file. Build the IR or run any of them:

```sh
facet build examples/social.fct    # print the compiled graph
facet dev   examples/social.fct    # run with hot reload, no database
facet run   examples/social.fct    # serve for real (needs Postgres)
```

## The examples

| File | Teaches |
|---|---|
| **`counter.fct`** | the [placement calculus](Placement-Calculus.md) — server vs. client state and actions, a derive that spans both. The smallest complete program. |
| **`chirp.fct`** | durable data, aggregates (`count`/`sum`), a permission `policy`, the server clock (`now()`), a live list, permission-gated UI. |
| **`social.fct`** | built-in `auth`, two routed pages with `link` navigation, a query feed. |
| **`secure.fct`** | RBAC + row-level authorization (`requires mine(id)`), and a `@secret` field encrypted at rest. |
| **`inbox.fct`** | relations (`Message.to: User`, read back with `User(m.to).name`), a scheduled `job`, the JSON API. |

## Cookbook

### A like button (optimistic, server-authoritative)

```
action like(id: int) @optimistic:
    set Post(id).likes = Post(id).likes + 1

# in a view:
button "♥ {p.likes}" -> like(p.id)
```

### "Only the author may edit" (row-level policy)

```
policy mine(id: int):
    actor == Post(id).author

action edit(id: int, body: text):
    requires mine(id)
    set Post(id).body = body
```

### A paginated, filtered feed

```
for p in Post where p.author == actor by created desc limit 20:
    box:
        text "{p.body}"
```

…and over the API: `GET /api/Post?author=ada&by=created&desc=1&limit=20`, then
follow the `next` cursor with `?after=…`.

### Input validation with a friendly error

```
action signupGuest(name: text):
    check name != "" "please enter a name"
    check count(User) < 1000 "the beta is full"
    add User { name: name }
```

### A scheduled cleanup

```
action purge:
    clear Message

job cleanup every 1h -> purge
```

### A relation and a cross-relation read

```
entity User:
    id: int
    name: text

entity Message:
    id: int
    to: User
    body: text

# in a view:
for m in Message where m.to == 1 by id desc limit 20:
    text "to {User(m.to).name}: {m.body}"
```

### An admin-only destructive action

```
policy admin:
    role == "admin"

action wipe:
    requires admin
    clear Post
```

### Money done right (integer minor units)

```
entity Invoice:
    id: int
    cents: money          # store integer minor units, e.g. 1999

action bill(cents: money):
    check cents > 0 "amount must be positive"
    add Invoice { cents: cents }

# render it with the money() builtin -> "19.99"
for inv in Invoice by id desc limit 20:
    text "${money(inv.cents)}"
```

### A reusable component + a layout

```
layout Shell:
    box:
        text "MyApp"
        slot

component Tag(label: text):
    box:
        text "#{label}"

view Home at "/" in Shell:
    box:
        use Tag("intro")
```

→ Back to **[Home](Home.md)** · the full grammar is in the
**[Language Reference](Language-Reference.md)**.
