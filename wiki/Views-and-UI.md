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
| **image** | `image "url" [alt "…"]` — URL interpolates, e.g. `image "…/avatar?seed={t.author}"` (renders a rounded avatar by default) |
| **video** | `video "url" [poster "url"] [alt "…"] [autoplay] [loop] [muted]` — a player with controls; `poster` is the still before playback; `autoplay` implies `muted` (no browser autoplays with sound) |
| **richtext** | `richtext "{p.body}"` — renders a safe Markdown subset: `#`/`##`/`###` headings, `- ` and `1. ` lists, `> ` quotes, ```` ``` ```` code, `---` rules, `[text](url)` links (http/https/mailto/relative only), `**bold**` `*italic*` `~~struck~~` `` `code` ``. Input is HTML-escaped first; the same renderer runs on server and client |
| **icon** | `icon "name"` — a named glyph the theme's CSS fills |
| **badge** | `badge "{unread}"` — a small pill for counts and status |
| **tabs** | `tabs bind cell:` then `tab "Label" -> "value":` blocks — a segmented control over a `@client` cell |
| **button** | `button "label" -> action(args)` — the label interpolates too: `button "♥ {t.likes}" -> like(t.id)` |
| **input** | `input bind cell [placeholder "…"]` |
| **select** | `select bind cell:` then `option "Label" -> "value"` lines |
| **form** | `form "Submit" -> action(args):` then children |
| **upload** | `upload bind urlCell [label "…"]` |
| **overlay** | `overlay bind boolCell:` then children — a modal layer shown while the cell is true |
| **typeahead** | `typeahead bind textCell from Entity.field [placeholder "…"]` — input with a native suggestion list |
| **link** | `link "label" -> "/path"` |
| **if** | `if <cond>:` then children |
| **for** | `for x in Coll [where c] [by f desc\|asc] [limit n] [more action]:` then children — `more` makes it an infinite scroll (see below) |
| **use** | `use Component(args)` |
| **slot** | `slot` (layouts) — where the routed view renders; `slot <name>` in a [wireframe frame](Layered-Facets.md) — where a socket's content composites in |

### for … more — infinite scroll

`limit` may be a `@client` cell, and `more` names the zero-argument action that
grows it. While the limit holds rows back, a **More** control follows the last
row; the browser fires the action as that control scrolls into view (an
`IntersectionObserver`), and a click or Enter on it does the same, so it works
with no observer and from the keyboard. The control disappears when the last row
is on the page — the server asks the store for one row past the limit, so it
knows.

```
state shown: int = 20 @client
action loadMore:
    shown = shown + 20

for p in Post by created desc limit shown more loadMore:
    use PostCard(p)
```

`loadMore` writes only client state, so the compiler places it on the client:
growing the page is a local write, and the rows come from the authority in one
`/region` request.

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
`FACET_UPLOAD_DIR`. A **large file uploads in resumable chunks automatically** —
the `upload` node POSTs a small file once but sends a big one in pieces that the
server reassembles, so a transfer larger than one request limit still completes.
See [Operations → Media handoff](Operations.md#media-handoff) for signed URLs,
size limits, and HLS, and [Configuration](Configuration.md).

### overlay & typeahead

`overlay` is a modal layer shown while a bound `@client` **bool** cell is true. A
dimmed backdrop centers a panel of the children; clicking the backdrop (or pressing
nothing — just the backdrop) sets the cell false. Open it from a client action that
sets the cell true:

```
state composing: bool = false @client
action openComposer():
    composing = true

overlay bind composing:
    box:
        text "New note"
        button "Save" -> save(draft)
```

`typeahead` is a text input wired to a native completion list of an entity field's
existing values — suggestions as the actor types. The chosen text binds to a
`@client` text cell like any input:

```
typeahead bind tagQuery from Tag.name placeholder "tag"
```

Suggestions reflect the collection at render time. A runnable demo combining both
is in `examples/overlay.fct`.

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

A reusable fragment with parameters, invoked with `use`. A parameter may be an
entity: `component PostCard(t: Post)` takes the row a `for` binds — `use
PostCard(p)` — and reads `t.body`, `t.author` with every field checked against
the entity. See [Language Reference → component](Language-Reference.md#component-and-layout).

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

### Dark mode

A `theme dark:` block overrides the same tokens under the OS dark scheme
(`@media (prefers-color-scheme: dark)`) — one declarative source styles both
schemes, no extra CSS:

```
theme:
    bg "#ffffff"
    fg "#16181c"
theme dark:
    bg "#000000"
    fg "#e7e9ea"
```

## Page metadata

A view may declare `meta title` and `meta description`. They render server-side
into `<title>`, `<meta name="description">`, and OpenGraph tags (`og:title`,
`og:description`) — and both **interpolate**, so a dynamic route gets a per-record
title for search engines and link previews:

```
view Read at "/post/:id":
    meta title "{Post(id).title} — The Blog"
    meta description "{Post(id).body}"
    box:
        text "{Post(id).body}"
```

Metadata is evaluated once per render (not a reactive client binding) and stays
correct across SPA navigation. With no `meta title`, the title is the app name.

→ Next: **[Projections & the API](Projections-and-API.md)**.
