# Views & UI

A `view` is a page bound to a route. Its body is a tree of nodes the runtime
renders server-side for first paint, then keeps live in the browser. You write
UI in the same file, same language, as everything else.

## Declaring a view

```
view Name [at "/path"] [in Layout] [requires policy]:
    <nodes>
```

- `at "/path"` — the URL. Omit it for the default page. `:name` segments are
  dynamic parameters (`view Post at "/post/:id":`).
- `in Layout` — render inside a [layout](#layouts).
- `requires policy` — a [route guard](#routing).

## Nodes

| Node | Syntax |
|---|---|
| **box** | `box:` then indented children — a vertical container |
| **row** | `row:` then indented children — a horizontal container that wraps and collapses to a column on a narrow viewport (responsive multi-column) |
| **text** | `text "literal and {expr}"` |
| **button** | `button "label" -> action(args)` |
| **input** | `input bind cell [placeholder "…"]` |
| **select** | `select bind cell:` then `option "Label" -> "value"` lines |
| **form** | `form "Submit" -> action(args):` then children |
| **upload** | `upload bind urlCell [label "…"]` |
| **link** | `link "label" -> "/path"` |
| **if** | `if <cond>:` then children |
| **for** | `for x in Coll [where c] [by f desc\|asc] [limit n]:` then children |
| **use** | `use Component(args)` |
| **slot** | `slot` (layouts) — where the routed view renders; `slot <name>` in a [wireframe frame](Layered-Facets.md) — where a socket's content composites in |

### text & interpolation

Embed any expression in `{…}`:

```
text "{postCount} posts · {totalLikes} likes"
text "signed in as {actor} ({role})"
```

Interpolations are reactive — they update when their inputs change.

### button

Wires a click to an [action](Actions-and-Logic.md). The compiler already knows
whether that action is server (round-trips) or client (instant).

```
button "post" -> post(draft)
button "log out" -> logout
button "♥ like" -> like(p.id)
```

### input & select

`input` is a two-way text binding to a state cell. `select` is a dropdown; over
an enum-typed cell its options default to the enum members, or you list them:

```
input bind draft placeholder "what's happening?"

select bind status:
    option "Draft" -> "draft"
    option "Published" -> "published"
```

### form & upload

`form` groups inputs and submits them to an action; `upload` posts a file and
stores the resulting URL in a cell.

```
form "Save profile" -> saveProfile(name, bio):
    input bind name placeholder "name"
    input bind bio placeholder "bio"
    upload bind avatarUrl label "Avatar"
```

Uploads are served from `/uploads/`; set the storage dir with
`FACET_UPLOAD_DIR`. See [Configuration](Configuration.md).

### if & for

`if` is a reactive conditional region; `for` is the query/list region (filter,
sort, paginate — see [Data Modeling → Queries](Data-Modeling.md#queries)).

```
if actor == "guest":
    button "log in" -> login(username, password)

for p in Post by created desc limit 50:
    box:
        text "{p.author}: {p.body}"
        button "delete" -> remove(p.id)
```

## Components

A reusable fragment with parameters, invoked with `use`:

```
component Avatar(name: text):
    box:
        text "@{name}"

view Home at "/":
    box:
        use Avatar("ada")
        use Avatar(actor)
```

## Layouts

A `layout` wraps pages around a `slot`. Views opt in with `in Layout`:

```
layout Shell:
    box:
        text "My App"
        slot                 # page content renders here
        text "© 2026"

view Home at "/" in Shell:
    box:
        text "home"
```

## Routing

- **Multiple pages:** declare several `view … at "/path"` and navigate between
  them with `link "label" -> "/path"`. Matched links do **SPA navigation** — the
  page swaps with no full reload.
- **Dynamic params:** a `:name` segment (`/post/:id`) is captured into the page
  scope.
- **Route guards:** `view Admin at "/admin-area" requires admin:` — the
  authority refuses to render the page unless the policy passes, and the client
  hides links to it.

## Theming

A `theme:` block becomes CSS custom properties (`--fa-<name>`) used by the
rendered pages:

```
theme:
    accent "#5b8cff"
    bg "#0f1115"
    radius "8px"
```

→ Next: **[Projections & the API](Projections-and-API.md)**.
